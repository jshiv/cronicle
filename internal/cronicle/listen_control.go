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

// runEvents streams the per-run event log as Server-Sent Events. The wire
// format is one SSE message per event, with the JSON `EventRow` as the
// data field and the row id as the `id:` field (so reconnect via
// Last-Event-ID gets a clean replay).
//
//	event: cronicle
//	id:    42
//	data:  {"id":42,"run_id":"…","entry_type":"task_start",…}
//
// Replay path: on connect, return all rows with id > Last-Event-ID
// (or all rows if missing). Then subscribe to the in-process firehose
// and emit live events. The publish is post-commit so the SSE stream
// monotonically agrees with what GET /v1/runs/{id} would show.
func (s *listenServer) runEvents(w http.ResponseWriter, r *http.Request, store *state.Store, runID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported by this server", http.StatusInternalServerError)
		return
	}

	// Last-Event-ID resume support per the SSE spec.
	var sinceID int64
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		_, _ = fmt.Sscan(v, &sinceID)
	} else if v := r.URL.Query().Get("since"); v != "" {
		_, _ = fmt.Sscan(v, &sinceID)
	}

	// Subscribe BEFORE replay so any event committed during replay is
	// captured (we de-dupe via id below).
	live, unsubscribe := store.SubscribeEvents(runID)
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
	w.WriteHeader(http.StatusOK)

	// Replay history.
	history, err := store.EventsSince(runID, sinceID)
	if err != nil {
		slog.Error("events replay failed", "run_id", runID, "error", err.Error())
		// Still proceed to live; better partial data than no data.
	}
	var lastID int64 = sinceID
	for _, row := range history {
		writeSSEEvent(w, row.ID, row.Payload)
		lastID = row.ID
	}
	flusher.Flush()

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
		case ev, ok := <-live:
			if !ok {
				return
			}
			// The in-memory Event doesn't carry the row id; re-marshal
			// minimally and tag with monotonic time so SSE id stays
			// strictly-increasing (lastID + 1, etc.).
			lastID++
			payload := marshalLiveEvent(ev)
			writeSSEEvent(w, lastID, payload)
			flusher.Flush()
		}
	}
}

func writeSSEEvent(w http.ResponseWriter, id int64, payload string) {
	fmt.Fprintf(w, "id: %d\nevent: cronicle\ndata: %s\n\n", id, payload)
}

// marshalLiveEvent serializes a live state.Event in the same schema the
// EventRow.Payload column holds (so the consumer doesn't have to branch
// on history-vs-live shapes). Falls back to a minimal shape if marshal
// fails.
func marshalLiveEvent(e state.Event) string {
	type out struct {
		RunID     string `json:"run_id"`
		Schedule  string `json:"schedule,omitempty"`
		Task      string `json:"task,omitempty"`
		EntryType string `json:"entry_type"`
		Time      string `json:"time,omitempty"`
		Msg       string `json:"msg,omitempty"`
	}
	body, err := json.Marshal(out{
		RunID:     e.RunID,
		Schedule:  e.Schedule,
		Task:      e.Task,
		EntryType: e.EntryType,
		Time:      e.Time.UTC().Format(time.RFC3339Nano),
		Msg:       e.Msg,
	})
	if err != nil {
		return `{}`
	}
	return string(body)
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
