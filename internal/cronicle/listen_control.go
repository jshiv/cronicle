// Phase 3 listener handlers: workers registry, cancel/retry, SSE
// control channel.
//
// Cancel marks the run canceled in the queue + projection AND, when
// the job is in flight on a worker, pushes a cancel message over the
// worker's SSE control stream so it can preempt the in-process execute
// context. Workers without an SSE subscription fall back on the
// next heartbeat returning 409 — the same lost-claim shape they
// already handle.
//
// Retry re-enqueues a terminal run with a new run_id. The original
// run row stays in the projection as audit trail.

package cronicle

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// ---- /v1/runs/{id}/(get|cancel|retry) --------------------------------------

// handleRunRoute dispatches under /v1/runs/. Shapes:
//
//	GET  /v1/runs/{id}         → run + tasks
//	GET  /v1/runs/{id}/events  → SSE stream of replayed history + live events
//	POST /v1/runs/{id}/cancel  → cancel a running or pending run
//	POST /v1/runs/{id}/retry   → re-enqueue a terminal run with a new id
//	POST /v1/runs/{id}/resume  → re-enqueue a non-terminal run (rare)
func (s *listenServer) handleRunRoute(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	store := s.currentStore()
	if store == nil {
		http.Error(w, "state store not enabled", http.StatusServiceUnavailable)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v1/runs/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "missing run_id", http.StatusBadRequest)
		return
	}
	runID := parts[0]

	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		s.runGet(w, store, runID)
	case len(parts) == 2 && parts[1] == "events" && r.Method == http.MethodGet:
		s.runEvents(w, r, store, runID)
	case len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost:
		s.runCancel(w, store, runID)
	case len(parts) == 2 && parts[1] == "retry" && r.Method == http.MethodPost:
		s.runRetry(w, store, runID)
	case len(parts) == 2 && parts[1] == "resume" && r.Method == http.MethodPost:
		s.runResume(w, store, runID)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// runEvents streams the per-run event log as Server-Sent Events.
//
// Wire format:
//
//	id: <lifetime>-<seq>
//	event: cronicle
//	data: {"time":"…","level":"INFO","msg":"…","entry_type":"task_start","run_id":"…","seq":42,"lifetime":"a1b2c3d4",…}
//
// `<lifetime>-<seq>` is the de-dup key. lifetime is an 8-char hex nonce
// minted once per producer process; seq is a per-process monotonic
// int64 minted by state.Tagger at the top of the slog chain. Together
// they identify exactly one record across the producer's lifetime
// AND across restarts (different lifetime → fresh seq space). Clients
// MUST de-dup on (lifetime, seq); the same record reaches them once
// via replay and once via live within the reconnect window.
//
// Two payload sources, identical bytes:
//
//   - Replay: rows from the events table (raw JSON line stored in
//     events.payload), with (lifetime, seq) read back from dedicated
//     columns (schema v4) and surfaced in the SSE id field.
//   - Live: the LiveSink slog handler, sitting alongside the file/stdout
//     handlers. Records with `run_id` pass the LiveSink filter — even
//     records WITHOUT entry_type (warnings, lifecycle prints) reach
//     subscribers. The payload already carries seq + lifetime because
//     Tagger injected them upstream of LiveSink.
//
// Resume: clients reconnect with Last-Event-ID = "<lifetime>-<seq>".
// EventsResume returns everything for the run that's not (lifetime ==
// clientLifetime AND seq <= clientSeq) — covering both the in-lifetime
// catch-up case and the producer-restart case (where lifetime mismatch
// triggers full replay). To close the small window where a record is
// committed AFTER the replay query but BEFORE the live subscription
// catches up, we subscribe BEFORE replay and re-query once more after
// the initial loop to backfill (Option A in the design doc).
func (s *listenServer) runEvents(w http.ResponseWriter, r *http.Request, store *state.Store, runID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported by this server", http.StatusInternalServerError)
		return
	}

	// Last-Event-ID resume support per the SSE spec. New shape:
	// "<lifetime>-<seq>". Legacy ?since=<int> still understood for
	// scripts that pre-date the seq+lifetime change.
	clientLifetime, clientSeq := parseEventID(r.Header.Get("Last-Event-ID"))
	if clientLifetime == "" && clientSeq == 0 {
		// Header empty/unparseable — fall back to ?since= query for legacy
		// callers that pass the rowid directly.
		if v := r.URL.Query().Get("since"); v != "" {
			_, _ = fmt.Sscan(v, &clientSeq)
		}
	}

	// Subscribe to LiveSink BEFORE the replay query so anything written
	// while we're scanning history is captured (events sit in the buffered
	// chan until we start consuming). If no LiveSink is wired (defensive
	// — usually means the listener was constructed in a test harness
	// without it), only the historical replay path runs.
	var live <-chan []byte
	var unsubscribe func()
	if ls := s.currentLiveSink(); ls != nil {
		live, unsubscribe = ls.Subscribe(runID)
		defer unsubscribe()
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
	w.WriteHeader(http.StatusOK)

	// Replay history then backfill. Track the highest (lifetime, seq) we've
	// seen so live records that overlap with the backfill window can be
	// short-circuited (they'd duplicate a replayed row otherwise — the
	// LiveSink subscribe + table commit are not atomic).
	lt, sq := s.replayHistory(w, store, runID, clientLifetime, clientSeq)
	flusher.Flush()
	lt, sq = s.replayHistory(w, store, runID, lt, sq)
	flusher.Flush()

	if live == nil {
		// No live source. Without a heartbeat to write, return now —
		// clients reconnect with Last-Event-ID to poll for more.
		return
	}

	// Heartbeat keeps proxies (k8s, nginx) from killing idle connections.
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			// SSE comment line — ignored by clients but keeps the conn warm.
			fmt.Fprintf(w, ": ping %d\n\n", time.Now().Unix())
			flusher.Flush()
		case line, alive := <-live:
			if !alive {
				return
			}
			lineLT, lineSeq := extractIDFromPayload(line)
			// Skip if we've already emitted this record during the
			// replay/backfill loop. Same lifetime + seq <= last seen
			// means it was already in the events table when we queried.
			if lineLT == lt && lineSeq > 0 && lineSeq <= sq {
				continue
			}
			writeSSEEventTagged(w, lineLT, lineSeq, string(line))
			if lineLT == lt && lineSeq > sq {
				sq = lineSeq
			} else if lineLT != lt && lineLT != "" {
				// Cross-lifetime live event — shouldn't happen within a
				// single connection (producer doesn't restart mid-stream)
				// but track it defensively so future events from this
				// new lifetime get cursor updates.
				lt = lineLT
				sq = lineSeq
			}
			flusher.Flush()
		}
	}
}

// replayHistory writes events for runID that the client hasn't already
// seen ((clientLifetime, clientSeq) cursor) and returns the
// highest (lifetime, seq) tuple it observed. Logs on error but does
// not abort — partial replay is better than no replay.
//
// Rows with NULL seq/lifetime (pre-v4 inserts, or programmatic Apply
// callers bypassing the slog chain) are emitted with an id field
// derived from the autoincrement row id as a fallback — better than
// no id at all for clients that have no other dedup option.
func (s *listenServer) replayHistory(w http.ResponseWriter, store *state.Store, runID string, clientLifetime string, clientSeq int64) (string, int64) {
	history, err := store.EventsResume(runID, clientLifetime, clientSeq)
	if err != nil {
		slog.Error("events replay failed", "run_id", runID, "error", err.Error())
		return clientLifetime, clientSeq
	}
	lt, sq := clientLifetime, clientSeq
	for _, row := range history {
		writeSSEEventTagged(w, row.Lifetime, row.Seq, row.Payload)
		// Advance cursor on same-lifetime rows so the subsequent live-tail
		// dedup window matches what we just emitted.
		if row.Lifetime == lt && row.Seq > sq {
			sq = row.Seq
		} else if row.Lifetime != "" && row.Lifetime != lt {
			lt = row.Lifetime
			sq = row.Seq
		}
	}
	return lt, sq
}

// writeSSEEventTagged emits an SSE frame with id = "<lifetime>-<seq>".
// Falls back to a bare seq (or no id at all) when lifetime is missing —
// e.g. pre-v4 events or programmatic Apply callers — so the SSE frame
// is still well-formed and the live tail still works.
func writeSSEEventTagged(w http.ResponseWriter, lifetime string, seq int64, payload string) {
	switch {
	case lifetime != "" && seq > 0:
		fmt.Fprintf(w, "id: %s-%d\nevent: cronicle\ndata: %s\n\n", lifetime, seq, payload)
	case seq > 0:
		fmt.Fprintf(w, "id: %d\nevent: cronicle\ndata: %s\n\n", seq, payload)
	default:
		fmt.Fprintf(w, "event: cronicle\ndata: %s\n\n", payload)
	}
}

// parseEventID parses an SSE Last-Event-ID in the new "<lifetime>-<seq>"
// form, returning ("", 0) on any failure. Lifetime is the leading
// non-dash run (length is intentionally not asserted to keep us
// forward-compat with a wider nonce in the future); seq is the trailing
// int64. Anything else returns zero values and the caller treats it as
// a fresh-connect cursor.
func parseEventID(id string) (string, int64) {
	if id == "" {
		return "", 0
	}
	i := strings.LastIndex(id, "-")
	if i <= 0 || i == len(id)-1 {
		return "", 0
	}
	lt := id[:i]
	var seq int64
	if _, err := fmt.Sscan(id[i+1:], &seq); err != nil {
		return "", 0
	}
	return lt, seq
}

// extractIDFromPayload pulls seq + lifetime out of the JSON line emitted
// by encodeRecord. Hot-path on the live tail — avoid a full unmarshal
// by scanning for the two known keys. Returns ("", 0) on parse failure;
// the caller emits an idless frame in that case.
func extractIDFromPayload(line []byte) (string, int64) {
	var m struct {
		Seq      int64  `json:"seq"`
		Lifetime string `json:"lifetime"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		return "", 0
	}
	return m.Lifetime, m.Seq
}

// runGet is what handleGetRun used to do — kept here so all run-route
// behavior is co-located.
func (s *listenServer) runGet(w http.ResponseWriter, store *state.Store, runID string) {
	run, err := store.GetRun(runID)
	if err != nil {
		if isNoRows(err) {
			http.Error(w, fmt.Sprintf("run %q not found", runID), http.StatusNotFound)
			return
		}
		slog.Error("get run failed", "run_id", runID, "error", err.Error())
		http.Error(w, "get run failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// runCancel marks the run canceled. If a worker holds the claim, push
// a cancel message via SSE so it preempts the in-flight execute. If
// the worker has no SSE subscription the heartbeat-based detection
// path still works (next /heartbeat returns 409).
func (s *listenServer) runCancel(w http.ResponseWriter, store *state.Store, runID string) {
	res, err := store.Cancel(runID)
	if err != nil {
		if errors.Is(err, state.ErrNotCancelable) {
			http.Error(w, "run is already terminal", http.StatusConflict)
			return
		}
		slog.Error("cancel failed", "run_id", runID, "error", err.Error())
		http.Error(w, "cancel failed", http.StatusInternalServerError)
		return
	}
	if res.WasClaimed && res.WorkerID != "" {
		// Push to the worker's SSE control channel. PushControl is a
		// best-effort hint; the projection is already authoritative.
		_ = store.PushControl(res.WorkerID, state.ControlMsg{
			Type:  "cancel",
			RunID: runID,
		})
	}
	slog.Info("run canceled",
		"run_id", runID,
		"was_claimed", res.WasClaimed,
		"was_pending", res.WasPending,
		"worker_id", res.WorkerID,
	)
	writeJSON(w, http.StatusOK, res)
}

// runRetry re-enqueues a terminal run. A fresh run_id is minted on the
// producer side so the new run is distinguishable in /v1/runs and
// transcripts.
func (s *listenServer) runRetry(w http.ResponseWriter, store *state.Store, runID string) {
	newID := newRunID()
	res, err := store.Retry(runID, newID)
	if err != nil {
		slog.Warn("retry failed", "run_id", runID, "error", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	slog.Info("run retried",
		"original_run_id", runID,
		"new_run_id", newID,
		"schedule", res.Schedule,
	)
	writeJSON(w, http.StatusAccepted, res)
}

// runResume re-enqueues a terminal run with only the tasks that didn't
// succeed in the original. Intended workflow: cancel → investigate →
// fix → resume from where it stopped. The new run's DAG is the
// original DAG minus already-succeeded nodes, with depends stripped
// of references to those nodes.
func (s *listenServer) runResume(w http.ResponseWriter, store *state.Store, runID string) {
	newID := newRunID()
	res, err := store.Resume(runID, newID)
	if err != nil {
		slog.Warn("resume failed", "run_id", runID, "error", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	slog.Info("run resumed",
		"original_run_id", runID,
		"new_run_id", newID,
		"schedule", res.Schedule,
		"skipped", res.SkippedTasks,
	)
	writeJSON(w, http.StatusAccepted, res)
}

// ---- /v1/workers + /v1/workers/{id}/control --------------------------------

// handleListWorkers is GET /v1/workers — returns the registry sorted
// newest-first. Status (active|idle|stale) is derived at read time.
func (s *listenServer) handleListWorkers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	store := s.currentStore()
	if store == nil {
		http.Error(w, "state store not enabled", http.StatusServiceUnavailable)
		return
	}
	ws, err := store.ListWorkers()
	if err != nil {
		slog.Error("list workers failed", "error", err.Error())
		http.Error(w, "list workers failed", http.StatusInternalServerError)
		return
	}
	if ws == nil {
		ws = []state.Worker{}
	}
	writeJSON(w, http.StatusOK, ws)
}

// handleWorkerRoute dispatches GET /v1/workers/{id}/control (SSE).
// Other paths under /v1/workers/ return 404 for now — future verbs
// (drain, pause) layer in here.
func (s *listenServer) handleWorkerRoute(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	store := s.currentStore()
	if store == nil {
		http.Error(w, "state store not enabled", http.StatusServiceUnavailable)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v1/workers/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[1] != "control" || parts[0] == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.serveControlSSE(w, r, store, parts[0])
}

// serveControlSSE is the long-lived SSE handler. Workers connect once
// per process; the producer pushes cancel messages down the stream. We
// also send a `ping` every 30s so intermediaries (load balancers,
// reverse proxies) don't kill the conn for inactivity, and so the
// worker can detect a dead producer via read-side timeout.
func (s *listenServer) serveControlSSE(w http.ResponseWriter, r *http.Request, store *state.Store, workerID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Register the worker in the registry on subscribe. Hostname is
	// optional and self-declared via X-Cronicle-Host (sane fallback for
	// when the worker doesn't set it; the registry tolerates empty).
	host := r.Header.Get("X-Cronicle-Host")
	_ = store.UpsertWorker(workerID, host)

	ch, unsub := store.Subscribe(workerID)
	defer unsub()

	pingT := time.NewTicker(30 * time.Second)
	defer pingT.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg, alive := <-ch:
			if !alive {
				// Subscribe replaced or store closed — disconnect cleanly.
				return
			}
			if err := writeSSE(w, "control", msg); err != nil {
				return
			}
			flusher.Flush()
		case <-pingT.C:
			if err := writeSSE(w, "ping", state.ControlMsg{Type: "ping"}); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeSSE serializes one event. SSE format: `event: <name>\ndata: <json>\n\n`.
func writeSSE(w http.ResponseWriter, event string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body)
	return err
}

// isNoRows returns true if err is sql.ErrNoRows. Wrapped so listen.go
// doesn't need to import database/sql twice.
func isNoRows(err error) bool {
	return strings.Contains(err.Error(), "no rows")
}
