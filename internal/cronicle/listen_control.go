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
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// ---- /v1/runs/{id}/(get|cancel|retry) --------------------------------------

// handleRunRoute dispatches under /v1/runs/. Shapes:
//
//	GET  /v1/runs/{id}                       → run + tasks (Phase 1)
//	GET  /v1/runs/{id}/events                → SSE: live-only stream of slog records for this run
//	POST /v1/runs/{id}/cancel                → cancel a running or pending run
//	POST /v1/runs/{id}/retry                 → re-enqueue a terminal run with a new id
//	POST /v1/runs/{id}/retry-failed    → re-enqueue with already-succeeded tasks skipped
//	POST /v1/runs/{id}/pause                 → block the DAG walker before launching new tasks
//	POST /v1/runs/{id}/resume                → clear the in-flight pause flag
//	POST /v1/runs/{id}/tasks/{task}/cancel   → cancel one task + cascade to dependents
//	POST /v1/runs/{id}/tasks/{task}/retry    → re-enqueue with only task + DAG dependents
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
		s.runEvents(w, r, runID)
	case len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost:
		s.runCancel(w, store, runID)
	case len(parts) == 2 && parts[1] == "retry" && r.Method == http.MethodPost:
		s.runRetry(w, store, runID)
	case len(parts) == 2 && parts[1] == "retry-failed" && r.Method == http.MethodPost:
		s.runRetryFailed(w, store, runID)
	case len(parts) == 2 && parts[1] == "pause" && r.Method == http.MethodPost:
		s.runPause(w, r, store, runID)
	case len(parts) == 2 && parts[1] == "resume" && r.Method == http.MethodPost:
		s.runResume(w, r, store, runID)
	case len(parts) == 4 && parts[1] == "tasks" && parts[3] == "cancel" && r.Method == http.MethodPost:
		s.taskCancel(w, store, runID, parts[2])
	case len(parts) == 4 && parts[1] == "tasks" && parts[3] == "retry" && r.Method == http.MethodPost:
		s.taskRetry(w, store, runID, parts[2])
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// taskRetry re-enqueues the run as a fresh run_id containing only the
// named task and its DAG dependents. Predecessor tasks (even succeeded
// ones) are dropped from the new payload because the operator is
// explicitly asking "re-run from this task forward."
//
// Cascade is computed from the schedule's loaded HCL; if the schedule
// can't be resolved we fall back to a singleton keep set (just the
// head task), trusting the operator's intent over a stale cache.
func (s *listenServer) taskRetry(w http.ResponseWriter, store *state.Store, runID, taskName string) {
	run, err := store.GetRun(runID)
	if err != nil {
		if isNoRows(err) {
			http.Error(w, fmt.Sprintf("run %q not found", runID), http.StatusNotFound)
			return
		}
		slog.Error("task retry: get run failed", "run_id", runID, "error", err.Error())
		http.Error(w, "retry failed", http.StatusInternalServerError)
		return
	}
	cascade := s.computeCascade(run.Schedule, taskName)
	keep := map[string]bool{taskName: true}
	for _, name := range cascade {
		keep[name] = true
	}

	newID := newRunID()
	res, err := store.RetryTask(runID, taskName, keep, newID)
	if err != nil {
		// Distinguish operator errors from server faults.
		msg := err.Error()
		if strings.Contains(msg, "not found") || strings.Contains(msg, "still in flight") || strings.Contains(msg, "empty task list") {
			http.Error(w, msg, http.StatusBadRequest)
			return
		}
		slog.Error("task retry failed", "run_id", runID, "task", taskName, "error", msg)
		http.Error(w, "retry failed", http.StatusInternalServerError)
		return
	}
	slog.Info("task retry enqueued",
		"entry_type", "task_retry",
		"original_run_id", runID,
		"new_run_id", newID,
		"schedule", res.Schedule,
		"task", taskName,
		"kept", append([]string{taskName}, cascade...),
		"dropped", res.SkippedTasks,
	)
	writeJSON(w, http.StatusAccepted, res)
}

// runStateResponse is the public shape of the in-flight pause row.
// Distinct from scheduleStateResponse (which describes future ticks).
type runStateResponse struct {
	RunID     string `json:"run_id"`
	Paused    bool   `json:"paused"`
	PausedAt  string `json:"paused_at,omitempty"`
	PausedBy  string `json:"paused_by,omitempty"`
	Reason    string `json:"reason,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

func runStateFromStore(store *state.Store, runID string) runStateResponse {
	out := runStateResponse{RunID: runID}
	row, err := store.GetRunState(runID)
	if err != nil {
		slog.Warn("get run state failed", "run_id", runID, "error", err.Error())
		return out
	}
	out.Paused = row.Paused
	out.PausedBy = row.PausedBy
	out.Reason = row.Reason
	if !row.PausedAt.IsZero() {
		out.PausedAt = row.PausedAt.UTC().Format(time.RFC3339Nano)
	}
	if !row.UpdatedAt.IsZero() {
		out.UpdatedAt = row.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	return out
}

// runPause flags an in-flight run as paused. The DAG walker reads the
// flag at every task-launch boundary and blocks until cleared. The run
// must exist in the projection — pausing a not-yet-queued run would
// race the projection writer; refuse with 404 to keep the surface
// honest.
//
// Body shape mirrors schedule-pause: optional {actor, reason} JSON or
// X-Actor / X-Reason headers.
func (s *listenServer) runPause(w http.ResponseWriter, r *http.Request, store *state.Store, runID string) {
	// Verify the run exists. A 404 here surfaces "you pasted a run_id
	// that doesn't match a real run" before any state row is created.
	if _, err := store.GetRun(runID); err != nil {
		if isNoRows(err) {
			http.Error(w, fmt.Sprintf("run %q not found", runID), http.StatusNotFound)
			return
		}
		slog.Error("run pause: get run failed", "run_id", runID, "error", err.Error())
		http.Error(w, "pause failed", http.StatusInternalServerError)
		return
	}
	actor, reason := readPauseBody(r)
	if err := store.PauseRun(runID, actor, reason); err != nil {
		slog.Error("pause run failed", "run_id", runID, "error", err.Error())
		http.Error(w, "pause failed", http.StatusInternalServerError)
		return
	}
	slog.Info("run paused",
		"entry_type", "run_paused",
		"run_id", runID,
		"actor", actor,
		"reason", reason)
	writeJSON(w, http.StatusOK, runStateFromStore(store, runID))
}

// runResume clears the in-flight pause flag — the natural partner to
// /pause. Distinct from /retry-failed which targets terminal
// runs; resume only affects an actively-paused walker.
func (s *listenServer) runResume(w http.ResponseWriter, r *http.Request, store *state.Store, runID string) {
	actor, _ := readPauseBody(r)
	if err := store.ResumeRun(runID, actor); err != nil {
		slog.Error("resume run failed", "run_id", runID, "error", err.Error())
		http.Error(w, "resume failed", http.StatusInternalServerError)
		return
	}
	slog.Info("run resumed",
		"entry_type", "run_resumed",
		"run_id", runID,
		"actor", actor)
	writeJSON(w, http.StatusOK, runStateFromStore(store, runID))
}

// taskCancel marks the named task and its transitive dependents in this
// run as canceled. The cascade set is computed from the schedule's
// loaded HCL (via confSrc); falling back to a degree-zero cascade if
// the schedule isn't found, so the head task at least gets canceled.
//
// MVP semantics: the projection is updated immediately and the DAG
// walker's per-task gate honors the canceled state at launch. An
// already-running task is NOT preempted (no per-task ctx yet) but its
// terminal event lands as canceled thanks to the sticky-cancel rule in
// applyTaskTerminal.
func (s *listenServer) taskCancel(w http.ResponseWriter, store *state.Store, runID, taskName string) {
	// Look up the run to find its schedule name; needed to compute the
	// cascade set against the loaded HCL.
	run, err := store.GetRun(runID)
	if err != nil {
		if isNoRows(err) {
			http.Error(w, fmt.Sprintf("run %q not found", runID), http.StatusNotFound)
			return
		}
		slog.Error("task cancel: get run failed", "run_id", runID, "error", err.Error())
		http.Error(w, "cancel failed", http.StatusInternalServerError)
		return
	}
	cascade := s.computeCascade(run.Schedule, taskName)

	res, err := store.CancelTask(runID, taskName, cascade)
	if err != nil {
		if errors.Is(err, state.ErrTaskNotCancelable) {
			http.Error(w, "task is already terminal", http.StatusConflict)
			return
		}
		// Surface NotFound for missing head task.
		if strings.Contains(err.Error(), "not found in run") {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		slog.Error("task cancel failed", "run_id", runID, "task", taskName, "error", err.Error())
		http.Error(w, "cancel failed", http.StatusInternalServerError)
		return
	}
	// Best-effort SSE hint so a future per-task preemption can wire in
	// without API churn. Today the worker has no handler for this msg;
	// the projection-based gate is the load-bearing path.
	if res.WasClaimed && res.WorkerID != "" {
		_ = store.PushControl(res.WorkerID, state.ControlMsg{
			Type:  "cancel_task",
			RunID: runID,
			Task:  taskName,
		})
	}
	slog.Info("task canceled (cascade)",
		"entry_type", "task_cancel_set",
		"run_id", runID,
		"task", taskName,
		"canceled_tasks", res.CanceledTasks,
		"skipped_terminal", res.SkippedTerminal,
		"worker_id", res.WorkerID,
	)
	writeJSON(w, http.StatusOK, res)
}

// computeCascade walks the schedule's DAG to find every transitive
// dependent of `task`. Returns an empty slice if the schedule isn't in
// the loaded config — the head task still gets canceled, but downstream
// blocking falls back to "whatever the worker does next." A missing
// schedule typically means the operator edited the HCL since the run
// was queued; we don't want to refuse the cancel for that.
func (s *listenServer) computeCascade(scheduleName, task string) []string {
	if s.confSrc == nil {
		return nil
	}
	conf := s.confSrc()
	if conf == nil {
		return nil
	}
	for i := range conf.Schedules {
		if conf.Schedules[i].Name == scheduleName {
			return conf.Schedules[i].downstreamTasks(task)
		}
	}
	return nil
}

// runEvents streams the events of one specific run. Drill-in case —
// operator clicks a run from a list, wants its frames only.
func (s *listenServer) runEvents(w http.ResponseWriter, r *http.Request, runID string) {
	s.streamLive(w, r, state.Filter{RunID: runID})
}

// scheduleEvents streams every run of the named schedule, including
// future runs that don't have a run_id yet. Solves the "open SSE
// before trigger" chicken-and-egg: the subscriber registers against a
// stable schedule name, and LiveSink delivers records as soon as the
// next run starts emitting them — no polling for the run_id in between.
//
// Frontend pattern: schedule overview page opens this endpoint once,
// renders runs as they fire. The pretty encoder's schedule_start /
// agent_run / shell_run blocks make run boundaries visually obvious in
// an xterm.js pane.
func (s *listenServer) scheduleEvents(w http.ResponseWriter, r *http.Request, name string) {
	s.streamLive(w, r, state.Filter{Schedule: name})
}

// handleEventsStream is the firehose: every run-bearing record across
// every schedule. For operator dashboards monitoring all activity at
// once. Volume can be high; xterm.js readers should mostly use the
// per-schedule endpoint instead.
func (s *listenServer) handleEventsStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.streamLive(w, r, state.Filter{})
}

// streamLive is the shared SSE plumbing for run / schedule / firehose
// endpoints. LIVE-ONLY: no replay on subscribe, no Last-Event-ID
// resume. Frame bytes are whatever LiveSink's configured Encoder
// produces (pretty ANSI by default — set via --live-format).
//
// For history (older runs, debugging) the frontend queries Loki using
// the seq+lifetime cursor IDs embedded in cronicle.jsonl. That's a
// separate pane in the UI — this code path is purely "what's
// happening right now."
//
// Disconnect = miss. Keeps the server stateless and the wire trivial.
func (s *listenServer) streamLive(w http.ResponseWriter, r *http.Request, filter state.Filter) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported by this server", http.StatusInternalServerError)
		return
	}
	ls := s.currentLiveSink()
	if ls == nil {
		http.Error(w, "live stream not enabled", http.StatusServiceUnavailable)
		return
	}

	ch, unsubscribe := ls.Subscribe(filter)
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Heartbeat keeps proxies (nginx, k8s, ELB) from killing idle
	// connections. SSE comment lines are ignored by clients but count
	// as activity at the TCP layer.
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprintf(w, ": ping %d\n\n", time.Now().Unix())
			flusher.Flush()
		case line, alive := <-ch:
			if !alive {
				return
			}
			writeSSEFrame(w, line)
			flusher.Flush()
		}
	}
}

// writeSSEFrame emits one SSE event with the given bytes as data. Pretty
// encoding produces multi-line text — SSE allows multiple `data:` lines
// in one frame; clients (browsers' EventSource) join them with '\n'.
// Empty payload is dropped (the encoder occasionally returns nothing
// for filtered records, no point shipping an empty frame).
func writeSSEFrame(w io.Writer, payload []byte) {
	if len(payload) == 0 {
		return
	}
	fmt.Fprint(w, "event: cronicle\n")
	// Per SSE spec, every newline in the payload starts a new `data:`
	// line. Trailing newline collapses to no extra line.
	start := 0
	for i := 0; i < len(payload); i++ {
		if payload[i] == '\n' {
			fmt.Fprintf(w, "data: %s\n", payload[start:i])
			start = i + 1
		}
	}
	if start < len(payload) {
		fmt.Fprintf(w, "data: %s\n", payload[start:])
	}
	fmt.Fprint(w, "\n")
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

// runRetryFailed re-enqueues a terminal run with only the tasks
// that didn't succeed in the original. Intended workflow: cancel →
// investigate → fix → re-fire from where it broke. The new run's DAG
// is the original DAG minus already-succeeded nodes, with depends
// stripped of references to those nodes. Distinct from runResume,
// which unpauses an in-flight DAG walker.
func (s *listenServer) runRetryFailed(w http.ResponseWriter, store *state.Store, runID string) {
	newID := newRunID()
	res, err := store.RetryFailed(runID, newID)
	if err != nil {
		slog.Warn("retry-failed failed", "run_id", runID, "error", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	slog.Info("run retry-failed",
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
