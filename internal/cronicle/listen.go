// HTTP listener for remote triggers. When `cronicle run` is started with
// --listen :PORT --listen-token TOKEN, this exposes a small REST API that
// lets external systems (a control plane, a UI, an alert webhook) fire
// schedules on demand without waiting for the cron tick.
//
// Endpoints:
//
//   GET  /healthz
//   GET  /v1/schedules                                   list configured schedules
//   POST /v1/schedules/{name}/trigger                    fire whole schedule
//   POST /v1/schedules/{name}/tasks/{task}/trigger       fire one task in a schedule
//   POST /v1/schedules/{name}/pause                      pause (silence cron + reject manual triggers)
//   POST /v1/schedules/{name}/resume                     clear a pause
//   GET  /v1/schedules/{name}/state                      report paused + drained + skipped-tasks
//   POST /v1/schedules/{name}/tasks/{task}/skip          flag task as skipped on next run
//   POST /v1/schedules/{name}/tasks/{task}/unskip        clear the skip flag
//
// Auth is bearer-token. The listener refuses to start without a token —
// this is an unattended cron service, hostile-by-default is the right
// posture. Single token in env / flag is sufficient for the v1 control
// plane → cronicle proxy path; rotate by restarting cronicle.
//
// Implementation: triggers reuse the same queue the cron tick uses.
// Single-node mode pushes to the in-process channel; distributed mode
// pushes to the vice transport (Redis/NSQ). Workers consume identically
// either way.

package cronicle

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// listenServer is bound to the producer's send queue plus a thread-safe
// snapshot of the most recently loaded Config (the heartbeat hot-reloads
// it via confPriorGlobal). The token is captured at construction; rotate
// by restarting the process.
//
// stateSrc returns the current state.Store (or nil) so /v1/runs handlers
// can answer queries against the projection. Indirected through a
// function so tests can inject a store without exporting a setter on
// the listener.
type listenServer struct {
	queue       chan<- []byte
	token       string
	confSrc     func() *Config
	stateSrc    func() *state.Store
	liveSinkSrc func() *state.LiveSink
}

// startListener brings up the HTTP server in a background goroutine.
// Returns immediately; failures are logged. Refuses to start when token
// is empty — an open trigger endpoint on an unattended service is a
// foot-cannon. Set --listen alone (no token) and you'll see a warning
// log but the listener won't bind.
func startListener(addr, token string, queue chan<- []byte) {
	if addr == "" {
		return
	}
	if token == "" {
		slog.Warn("--listen ignored: --listen-token is required (refusing to expose unauthenticated trigger endpoint)", "addr", addr)
		return
	}
	s := &listenServer{
		queue:       queue,
		token:       token,
		confSrc:     func() *Config { return confPriorGlobal },
		stateSrc:    StateStore,
		liveSinkSrc: LiveSink,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/schedules", s.handleListSchedules)
	mux.HandleFunc("/v1/schedules/", s.handleScheduleRoute)
	mux.HandleFunc("/v1/runs", s.handleListRuns)
	mux.HandleFunc("/v1/events", s.handleIngestEvents)
	mux.HandleFunc("/v1/events/stream", s.handleEventsStream)
	mux.HandleFunc("/v1/jobs", s.handleClaimJob)
	mux.HandleFunc("/v1/jobs/", s.handleJobControl)
	mux.HandleFunc("/v1/workers", s.handleListWorkers)
	mux.HandleFunc("/v1/workers/", s.handleWorkerRoute)
	mux.HandleFunc("/v1/runs/", s.handleRunRoute)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// WriteTimeout has to outlast the longest long-poll block. We
		// cap GET /v1/jobs at 60s, so 90s here gives the handler room
		// to finish writing the response after the wait returns.
		WriteTimeout: 90 * time.Second,
		// IdleTimeout closes connections that have been idle in the
		// keep-alive pool — independent of WriteTimeout, but a sensible
		// match for the long-poll cadence.
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		slog.Info("Trigger listener up", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("trigger listener exited", "error", err.Error())
		}
	}()
}

// authed returns true when the request carries a matching bearer token.
// /healthz is exempt (handled separately) since it's load-balancer fodder
// and shouldn't require credentials.
func (s *listenServer) authed(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	return strings.TrimPrefix(h, prefix) == s.token
}

func (s *listenServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// scheduleSummary is the public list-schedules response shape. Kept tight:
// name, cron, and task names. Token usage / cost / last-run details are
// intentionally NOT here — those live in slog and the file log; this
// endpoint is for "what's available to trigger?", not "what happened?".
type scheduleSummary struct {
	Name  string   `json:"name"`
	Cron  string   `json:"cron"`
	Tasks []string `json:"tasks"`
}

func (s *listenServer) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conf := s.confSrc()
	if conf == nil {
		writeJSON(w, http.StatusOK, []scheduleSummary{})
		return
	}
	out := make([]scheduleSummary, 0, len(conf.Schedules))
	for _, sch := range conf.Schedules {
		ts := make([]string, len(sch.Tasks))
		for i, t := range sch.Tasks {
			ts[i] = t.Name
		}
		out = append(out, scheduleSummary{Name: sch.Name, Cron: sch.Cron, Tasks: ts})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleScheduleRoute dispatches under /v1/schedules/. Shapes:
//
//	POST /v1/schedules/{name}/trigger
//	POST /v1/schedules/{name}/tasks/{task}/trigger
//	GET  /v1/schedules/{name}/events   → SSE: live frames for every
//	                                    run of this schedule (incl.
//	                                    runs that haven't started yet)
//
// We split on "/" rather than mounting per-pattern routers because the
// path shape is fixed and tiny; bringing in a routing library for three
// endpoints is overkill.
func (s *listenServer) handleScheduleRoute(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v1/schedules/")
	parts := strings.Split(rest, "/")

	if len(parts) < 2 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	schedName := parts[0]
	sch := s.findSchedule(schedName)
	if sch == nil {
		http.Error(w, fmt.Sprintf("schedule %q not found", schedName), http.StatusNotFound)
		return
	}

	// GET /v1/schedules/{name}/events — live SSE.
	if len(parts) == 2 && parts[1] == "events" && r.Method == http.MethodGet {
		s.scheduleEvents(w, r, schedName)
		return
	}
	// GET /v1/schedules/{name}/state — current pause/drain flags.
	if len(parts) == 2 && parts[1] == "state" && r.Method == http.MethodGet {
		s.handleGetScheduleState(w, r, schedName)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	switch {
	case len(parts) == 2 && parts[1] == "trigger":
		if s.isPaused(schedName) {
			http.Error(w, "schedule is paused", http.StatusConflict)
			return
		}
		ok := s.triggerSchedule(*sch)
		if !ok {
			http.Error(w, "queue unavailable", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"queued":   schedName,
			"schedule": schedName,
		})
	case len(parts) == 4 && parts[1] == "tasks" && parts[3] == "trigger":
		taskName := parts[2]
		if !hasTask(*sch, taskName) {
			http.Error(w, fmt.Sprintf("task %q not in schedule %q", taskName, schedName), http.StatusNotFound)
			return
		}
		if s.isPaused(schedName) {
			http.Error(w, "schedule is paused", http.StatusConflict)
			return
		}
		ok := s.triggerTask(*sch, taskName)
		if !ok {
			http.Error(w, "queue unavailable", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"queued":   schedName + "/" + taskName,
			"schedule": schedName,
			"task":     taskName,
		})
	case len(parts) == 4 && parts[1] == "tasks" && parts[3] == "skip":
		taskName := parts[2]
		if !hasTask(*sch, taskName) {
			http.Error(w, fmt.Sprintf("task %q not in schedule %q", taskName, schedName), http.StatusNotFound)
			return
		}
		s.handleSkipTask(w, r, schedName, taskName)
	case len(parts) == 4 && parts[1] == "tasks" && parts[3] == "unskip":
		taskName := parts[2]
		if !hasTask(*sch, taskName) {
			http.Error(w, fmt.Sprintf("task %q not in schedule %q", taskName, schedName), http.StatusNotFound)
			return
		}
		s.handleUnskipTask(w, r, schedName, taskName)
	case len(parts) == 2 && parts[1] == "pause":
		s.handlePauseSchedule(w, r, schedName)
	case len(parts) == 2 && parts[1] == "resume":
		s.handleResumeSchedule(w, r, schedName)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// taskStateResponse is the public shape of the per-task skip state row.
type taskStateResponse struct {
	Schedule  string `json:"schedule"`
	Task      string `json:"task"`
	Skipped   bool   `json:"skipped"`
	SkippedAt string `json:"skipped_at,omitempty"`
	SkippedBy string `json:"skipped_by,omitempty"`
	Reason    string `json:"reason,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// handleSkipTask flags (schedule, task) as skipped. Idempotent — re-skip
// preserves the original skipped_at. Body shape mirrors the pause
// endpoint: optional JSON {actor, reason} or X-Actor / X-Reason headers.
func (s *listenServer) handleSkipTask(w http.ResponseWriter, r *http.Request, schedule, task string) {
	st := s.stateSrc()
	if st == nil {
		http.Error(w, "state store unavailable", http.StatusServiceUnavailable)
		return
	}
	actor, reason := readPauseBody(r)
	if err := st.SetTaskSkipped(schedule, task, actor, reason); err != nil {
		slog.Error("skip task failed", "schedule", schedule, "task", task, "error", err.Error())
		http.Error(w, "skip failed", http.StatusInternalServerError)
		return
	}
	slog.Info("task skipped (flag set)",
		"entry_type", "task_skip_set",
		"schedule", schedule,
		"task", task,
		"actor", actor,
		"reason", reason)
	writeJSON(w, http.StatusOK, taskStateFromStore(st, schedule, task))
}

// handleUnskipTask clears the skip flag. Idempotent: a missing row
// returns 200 with skipped=false.
func (s *listenServer) handleUnskipTask(w http.ResponseWriter, r *http.Request, schedule, task string) {
	st := s.stateSrc()
	if st == nil {
		http.Error(w, "state store unavailable", http.StatusServiceUnavailable)
		return
	}
	actor, _ := readPauseBody(r)
	if err := st.ClearTaskSkipped(schedule, task, actor); err != nil {
		slog.Error("unskip task failed", "schedule", schedule, "task", task, "error", err.Error())
		http.Error(w, "unskip failed", http.StatusInternalServerError)
		return
	}
	slog.Info("task unskipped",
		"entry_type", "task_skip_cleared",
		"schedule", schedule,
		"task", task,
		"actor", actor)
	writeJSON(w, http.StatusOK, taskStateFromStore(st, schedule, task))
}

// taskStateFromStore reads the row and maps it to the wire shape.
func taskStateFromStore(st *state.Store, schedule, task string) taskStateResponse {
	out := taskStateResponse{Schedule: schedule, Task: task}
	row, err := st.GetTaskState(schedule, task)
	if err != nil {
		slog.Warn("get task state failed", "schedule", schedule, "task", task, "error", err.Error())
		return out
	}
	out.Skipped = row.Skipped
	out.SkippedBy = row.SkippedBy
	out.Reason = row.Reason
	if !row.SkippedAt.IsZero() {
		out.SkippedAt = row.SkippedAt.UTC().Format(time.RFC3339Nano)
	}
	if !row.UpdatedAt.IsZero() {
		out.UpdatedAt = row.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	return out
}

// pauseRequest is the (optional) body for POST /v1/schedules/{name}/pause.
// All fields are optional; callers commonly POST an empty body. Reason is
// surfaced in /state and in slog audit lines.
type pauseRequest struct {
	Actor  string `json:"actor"`
	Reason string `json:"reason"`
}

// scheduleStateResponse is the public shape of GET /v1/schedules/{name}/state.
type scheduleStateResponse struct {
	Name         string              `json:"name"`
	Paused       bool                `json:"paused"`
	PausedAt     string              `json:"paused_at,omitempty"`
	PausedBy     string              `json:"paused_by,omitempty"`
	Reason       string              `json:"reason,omitempty"`
	Drained      bool                `json:"drained"`
	UpdatedAt    string              `json:"updated_at,omitempty"`
	SkippedTasks []taskStateResponse `json:"skipped_tasks,omitempty"`
}

// handlePauseSchedule marks a schedule as paused so the cron loop and
// trigger endpoints skip its runs. Idempotent on the row level: a second
// pause refreshes actor/reason but preserves the original paused_at so
// "how long has this been off?" answers consistently.
//
// Reads actor/reason from the JSON body when provided; falls back to
// X-Actor / X-Reason headers, then to "" (handler-side audit reads
// these from slog regardless).
func (s *listenServer) handlePauseSchedule(w http.ResponseWriter, r *http.Request, name string) {
	st := s.stateSrc()
	if st == nil {
		http.Error(w, "state store unavailable", http.StatusServiceUnavailable)
		return
	}
	actor, reason := readPauseBody(r)
	if err := st.SetSchedulePaused(name, actor, reason); err != nil {
		slog.Error("pause schedule failed", "schedule", name, "error", err.Error())
		http.Error(w, "pause failed", http.StatusInternalServerError)
		return
	}
	slog.Info("schedule paused",
		"entry_type", "schedule_paused",
		"schedule", name,
		"actor", actor,
		"reason", reason)
	writeJSON(w, http.StatusOK, scheduleStateFromStore(st, name))
}

// handleResumeSchedule clears the pause flag. Idempotent: resume on an
// already-active schedule returns 200 + the unchanged state row.
func (s *listenServer) handleResumeSchedule(w http.ResponseWriter, r *http.Request, name string) {
	st := s.stateSrc()
	if st == nil {
		http.Error(w, "state store unavailable", http.StatusServiceUnavailable)
		return
	}
	actor, _ := readPauseBody(r)
	if err := st.ClearSchedulePaused(name, actor); err != nil {
		slog.Error("resume schedule failed", "schedule", name, "error", err.Error())
		http.Error(w, "resume failed", http.StatusInternalServerError)
		return
	}
	slog.Info("schedule resumed",
		"entry_type", "schedule_resumed",
		"schedule", name,
		"actor", actor)
	writeJSON(w, http.StatusOK, scheduleStateFromStore(st, name))
}

// handleGetScheduleState returns the current control row. A schedule
// with no row reports {paused: false, drained: false} — consistent with
// the cron loop's default.
func (s *listenServer) handleGetScheduleState(w http.ResponseWriter, r *http.Request, name string) {
	st := s.stateSrc()
	if st == nil {
		// Without state, we can still answer the question honestly:
		// nothing is paused (because nothing CAN be paused).
		writeJSON(w, http.StatusOK, scheduleStateResponse{Name: name})
		return
	}
	writeJSON(w, http.StatusOK, scheduleStateFromStore(st, name))
}

// readPauseBody pulls actor/reason from a JSON body when one is present,
// falling back to X-Actor / X-Reason headers. Bodyless POSTs (curl with
// no -d) are explicitly supported — the headers and falling-through
// empties keep the endpoint script-friendly.
func readPauseBody(r *http.Request) (string, string) {
	var req pauseRequest
	if r.Body != nil && r.ContentLength != 0 {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if req.Actor == "" {
		req.Actor = r.Header.Get("X-Actor")
	}
	if req.Reason == "" {
		req.Reason = r.Header.Get("X-Reason")
	}
	return req.Actor, req.Reason
}

// scheduleStateFromStore loads the row and maps it to the wire shape.
// Pulled out so the pause/resume handlers can echo current state back
// without re-implementing the conversion. Includes per-task skip rows
// so a single GET answers "what's the control state of this schedule?"
// completely.
func scheduleStateFromStore(st *state.Store, name string) scheduleStateResponse {
	out := scheduleStateResponse{Name: name}
	row, err := st.GetScheduleState(name)
	if err != nil {
		slog.Warn("get schedule state failed", "schedule", name, "error", err.Error())
		return out
	}
	out.Paused = row.Paused
	out.Drained = row.Drained
	out.PausedBy = row.PausedBy
	out.Reason = row.Reason
	if !row.PausedAt.IsZero() {
		out.PausedAt = row.PausedAt.UTC().Format(time.RFC3339Nano)
	}
	if !row.UpdatedAt.IsZero() {
		out.UpdatedAt = row.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	if skipped, err := st.ListSkippedTasksForSchedule(name); err == nil && len(skipped) > 0 {
		out.SkippedTasks = make([]taskStateResponse, 0, len(skipped))
		for _, ts := range skipped {
			entry := taskStateResponse{
				Schedule:  ts.Schedule,
				Task:      ts.Task,
				Skipped:   true,
				SkippedBy: ts.SkippedBy,
				Reason:    ts.Reason,
			}
			if !ts.SkippedAt.IsZero() {
				entry.SkippedAt = ts.SkippedAt.UTC().Format(time.RFC3339Nano)
			}
			if !ts.UpdatedAt.IsZero() {
				entry.UpdatedAt = ts.UpdatedAt.UTC().Format(time.RFC3339Nano)
			}
			out.SkippedTasks = append(out.SkippedTasks, entry)
		}
	}
	return out
}

// findSchedule walks the current loaded config. Returns a pointer into the
// slice, but the caller dereferences immediately into a local Schedule
// before queue write, so subsequent heartbeat reloads can't mutate the
// payload after it's been queued.
func (s *listenServer) findSchedule(name string) *Schedule {
	if s.confSrc == nil {
		return nil
	}
	conf := s.confSrc()
	if conf == nil {
		return nil
	}
	for i := range conf.Schedules {
		if conf.Schedules[i].Name == name {
			return &conf.Schedules[i]
		}
	}
	return nil
}

func hasTask(sch Schedule, name string) bool {
	for _, t := range sch.Tasks {
		if t.Name == name {
			return true
		}
	}
	return false
}

// triggerSchedule pushes the named schedule's full DAG onto the queue.
// Now is set to the current wall-clock so ${date}/${datetime}/${timestamp}
// resolve to the trigger moment, not whatever last cron tick set them to.
// Returns false when the queue is full / blocked / unavailable.
//
// Pause semantics: a paused schedule rejects manual triggers too. The
// rule is "paused means do not run, regardless of source." An operator
// who wants to run a paused schedule once should resume + trigger +
// pause, or use the (future) /v1/schedules/{name}/run-once endpoint
// that's explicit about bypassing pause.
func (s *listenServer) triggerSchedule(sch Schedule) bool {
	if s.isPaused(sch.Name) {
		return false
	}
	sch.Now = nowInScheduleZone(sch)
	sch.RunID = newRunID()
	sch.Source = "http"
	return s.send(sch.JSON(), sch.Name, "")
}

// triggerTask builds a sub-schedule containing only the requested task and
// pushes it. Depends are stripped from the task because the caller is
// asking specifically for this one to run; they're not asking for its
// upstream DAG. If the user wanted the full chain, they'd hit the
// schedule-level trigger.
//
// A paused schedule blocks per-task triggers too: pause is the schedule-
// wide "freeze" verb.
func (s *listenServer) triggerTask(sch Schedule, taskName string) bool {
	if s.isPaused(sch.Name) {
		return false
	}
	sub := sch
	sub.Tasks = nil
	for _, t := range sch.Tasks {
		if t.Name == taskName {
			t.Depends = nil
			sub.Tasks = append(sub.Tasks, t)
		}
	}
	sub.Now = nowInScheduleZone(sub)
	sub.RunID = newRunID()
	sub.Source = "http"
	return s.send(sub.JSON(), sch.Name, taskName)
}

// isPaused consults the state store. Returns false when the store is
// unavailable — failing open is correct here: the projection is a
// derived view, not the source of truth, and a transient DB hiccup
// shouldn't take the trigger surface offline. The cron-loop gate
// applies the same policy.
func (s *listenServer) isPaused(name string) bool {
	if s.stateSrc == nil {
		return false
	}
	st := s.stateSrc()
	if st == nil {
		return false
	}
	paused, err := st.IsSchedulePaused(name)
	if err != nil {
		slog.Warn("listener pause check failed; treating as active",
			"schedule", name, "error", err.Error())
		return false
	}
	return paused
}

// send pushes payload to the queue with a hard bound so a stuck consumer
// doesn't block the HTTP request indefinitely. 5s is generous for an
// in-process channel and a bounded redis push; if it fails, the API
// returns 503 and the caller can retry.
func (s *listenServer) send(payload []byte, schedName, taskName string) bool {
	if s.queue == nil {
		return false
	}
	select {
	case s.queue <- payload:
		slog.Info("triggered",
			"entry_type", "trigger",
			"schedule", schedName,
			"task", taskName,
			"source", "http",
		)
		return true
	case <-time.After(5 * time.Second):
		slog.Error("trigger queue blocked", "schedule", schedName, "task", taskName)
		return false
	}
}

// nowInScheduleZone returns the wall-clock time in the schedule's
// timezone, falling back to local. Mirrors ProduceSchedule's logic so
// trigger-fired runs and cron-fired runs see the same Now semantics.
func nowInScheduleZone(sch Schedule) time.Time {
	loc, err := loadLocation(sch.Timezone)
	if err != nil || loc == nil {
		loc = time.Local
	}
	return time.Now().In(loc)
}

// loadLocation wraps time.LoadLocation with empty-string passthrough so
// schedules without a timezone don't trip Go's "unknown" zone error.
func loadLocation(tz string) (*time.Location, error) {
	if tz == "" {
		return time.Local, nil
	}
	return time.LoadLocation(tz)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ---- /v1/runs ---------------------------------------------------------------

// handleListRuns answers GET /v1/runs with the most recent runs from the
// projection, filterable by status, schedule, since, and limit. When the
// state store is disabled the endpoint returns 503 — the API surface is
// honest about whether the projection is available rather than silently
// returning empty arrays.
//
// Query params:
//
//	status=succeeded|failed|queued|running|canceled
//	schedule=<name>
//	since=<RFC3339 timestamp>     (started_at >= since)
//	limit=<n>                     (1..500, default 50)
func (s *listenServer) handleListRuns(w http.ResponseWriter, r *http.Request) {
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
	q := r.URL.Query()
	f := state.ListFilter{
		Status:   q.Get("status"),
		Schedule: q.Get("schedule"),
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			http.Error(w, "invalid limit", http.StatusBadRequest)
			return
		}
		f.Limit = n
	}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			http.Error(w, "invalid since (need RFC3339)", http.StatusBadRequest)
			return
		}
		f.Since = t
	}
	runs, err := store.ListRuns(f)
	if err != nil {
		slog.Error("list runs failed", "error", err.Error())
		http.Error(w, "list runs failed", http.StatusInternalServerError)
		return
	}
	if runs == nil {
		runs = []state.Run{}
	}
	writeJSON(w, http.StatusOK, runs)
}

// handleGetRun is now folded into handleRunRoute (see listen_control.go).
// Kept here as a stub-removed marker for any callers that drift in.

// currentStore is a small indirection to handle the test path where
// stateSrc may be nil and the global StateStore is what we want.
func (s *listenServer) currentStore() *state.Store {
	if s == nil {
		return nil
	}
	if s.stateSrc != nil {
		return s.stateSrc()
	}
	return StateStore()
}

// currentLiveSink mirrors currentStore — falls back to the global
// LiveSink so tests that wire only stateSrc still get the firehose.
// Returns nil when the state subsystem is disabled.
func (s *listenServer) currentLiveSink() *state.LiveSink {
	if s == nil {
		return nil
	}
	if s.liveSinkSrc != nil {
		return s.liveSinkSrc()
	}
	return LiveSink()
}

// ---- /v1/events -------------------------------------------------------------

// maxEventBatchBytes caps a single ingest body so a misbehaving worker
// can't OOM the producer with one giant POST. 16 MiB is generous: a typical
// event is ~500 bytes, so 16 MiB is ~30k events — far more than any real
// batch should carry.
const maxEventBatchBytes = 16 << 20

// ingestResponse is the shape returned by POST /v1/events. Callers can
// reconcile partial-success cases: a worker that POSTs 100 events and
// gets accepted=98, dropped=2 has enough info to retry the dropped ones
// (we don't echo which ones failed; simplest API is "retry the whole
// batch" since Apply is idempotent at the projection level).
type ingestResponse struct {
	Accepted int `json:"accepted"`
	Dropped  int `json:"dropped"`
}

// handleIngestEvents receives a JSONL body and folds each event into the
// projection. One event per line — same shape as cronicle.jsonl on disk,
// so workers can literally POST tail bytes with no transformation.
//
// Auth: bearer token, same as other endpoints. Workers will use the same
// CRONICLE_LISTEN_TOKEN we already use for triggers; tighter per-worker
// scoping can land later if it ever matters.
//
// Body limit: 16 MiB. Empty body → 200 with accepted=0. Lines that don't
// parse or that DecodeEvent rejects (no entry_type, no run_id) are
// counted as dropped — they're benign for the projection but the count
// surfaces malformed shippers.
func (s *listenServer) handleIngestEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
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
	// liveSink may be nil when the state subsystem is wired without a
	// live plane (older test harnesses). Inject is a no-op on nil, so the
	// only cost of resolving it here is one accessor call per request.
	liveSink := s.currentLiveSink()
	body := http.MaxBytesReader(w, r.Body, maxEventBatchBytes)
	defer body.Close()

	var (
		accepted int
		dropped  int
	)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxEventBatchBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 || line[0] == '\n' {
			continue
		}
		ev, ok := state.DecodeEvent(line)
		if !ok {
			dropped++
			continue
		}
		if err := store.Apply(ev); err != nil {
			slog.Warn("event apply failed", "run_id", ev.RunID, "entry_type", ev.EntryType, "error", err.Error())
			dropped++
			continue
		}
		// Fan worker-shipped events out to SSE subscribers on the runner.
		// Without this, /v1/runs/{id}/events and /v1/schedules/{name}/events
		// only ever see records produced by the runner's own slog chain —
		// distributed-mode workers stay invisible to the live stream even
		// though their events are persisted just fine.
		//
		// Rehydrate the worker's JSON line back into a slog.Record and
		// route it through liveSink.Handle. This puts worker records on
		// the same encode path the runner's own slog chain uses, so SSE
		// subscribers see ONE wire format (whatever --live-format chose)
		// regardless of whether the work ran locally or on a worker. The
		// alternative — Inject with raw bytes — leaks JSON-on-the-wire
		// into a pretty subscriber's pane, which is jarring for agent
		// runs where the user expects the same ANSI-rendered output as
		// a local run.
		if rec, ok := rehydrateRecord(line); ok {
			_ = liveSink.Handle(r.Context(), rec)
		}
		accepted++
	}
	if err := scanner.Err(); err != nil {
		// MaxBytesReader returns http.MaxBytesError when over the limit.
		// bufio.ErrTooLong fires when a single line exceeds our scanner
		// buffer — same end-user fault (oversized payload), so map both
		// to 413 rather than spraying internal-error statuses.
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) || errors.Is(err, bufio.ErrTooLong) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		slog.Error("events ingest read failed", "error", err.Error())
		http.Error(w, "ingest read failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, ingestResponse{Accepted: accepted, Dropped: dropped})
}

// ---- /v1/jobs (queue dispatch) ---------------------------------------------

// maxLongPollBlock caps the duration we hold a /v1/jobs request open
// waiting for a job. Has to be < the http.Server WriteTimeout (90s) by
// a safety margin. Also: keeping it short bounds the worst-case latency
// for graceful shutdown.
const maxLongPollBlock = 60 * time.Second

// defaultClaimVisibility is how long a worker has to ack a claimed job
// before the reaper takes it back. 5 minutes covers slow agent runs;
// long-running tasks must heartbeat.
const defaultClaimVisibility = 5 * time.Minute

// handleClaimJob is GET /v1/jobs?worker=W1&block=30s
//
// Atomically claims one pending job and returns it. If none are
// available, blocks up to `block` seconds (default 30s, max 60s) and
// retries; returns 204 No Content if still empty after the wait.
//
// Query params:
//
//	worker=<id>      required, identifies the claimant for ack/heartbeat
//	block=<duration> Go duration syntax: "30s", "1m". Default 30s, max 60s.
func (s *listenServer) handleClaimJob(w http.ResponseWriter, r *http.Request) {
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
	q := r.URL.Query()
	workerID := q.Get("worker")
	if workerID == "" {
		http.Error(w, "worker query param required", http.StatusBadRequest)
		return
	}
	block := 30 * time.Second
	if v := q.Get("block"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			http.Error(w, "invalid block duration", http.StatusBadRequest)
			return
		}
		if d > maxLongPollBlock {
			d = maxLongPollBlock
		}
		block = d
	}

	deadline := time.Now().Add(block)
	for {
		job, err := store.Claim(workerID, defaultClaimVisibility)
		if err == nil {
			writeJSON(w, http.StatusOK, job)
			return
		}
		if !errors.Is(err, state.ErrNoJobs) {
			slog.Error("claim failed", "worker", workerID, "error", err.Error())
			http.Error(w, "claim failed", http.StatusInternalServerError)
			return
		}
		// No jobs right now. Wait for a wakeup or until the deadline.
		remaining := time.Until(deadline)
		if remaining <= 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		store.WaitForJob(r.Context(), remaining)
		// Loop and try Claim again. If the wait was a spurious wakeup
		// or the request context died, the next Claim returns ErrNoJobs
		// and the deadline-elapsed branch above closes the response.
		select {
		case <-r.Context().Done():
			// Client disconnected — don't try to write to a dead conn.
			return
		default:
		}
	}
}

// handleJobControl dispatches POSTs under /v1/jobs/. Two shapes:
//
//	POST /v1/jobs/{run_id}/ack
//	POST /v1/jobs/{run_id}/heartbeat
//
// JSON body for ack: {"worker": "W1", "success": true|false, "error": "..."}.
// JSON body for heartbeat: {"worker": "W1"}.
func (s *listenServer) handleJobControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
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
	rest := strings.TrimPrefix(r.URL.Path, "/v1/jobs/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	runID := parts[0]
	verb := parts[1]
	if runID == "" {
		http.Error(w, "missing run_id", http.StatusBadRequest)
		return
	}
	switch verb {
	case "ack":
		var body struct {
			Worker  string `json:"worker"`
			Success bool   `json:"success"`
			Error   string `json:"error,omitempty"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}
		if body.Worker == "" {
			http.Error(w, "worker required in body", http.StatusBadRequest)
			return
		}
		if err := store.Ack(runID, body.Worker, body.Success, body.Error); err != nil {
			slog.Error("ack failed", "run_id", runID, "error", err.Error())
			http.Error(w, "ack failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"acked": runID})
	case "heartbeat":
		var body struct {
			Worker string `json:"worker"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}
		if body.Worker == "" {
			http.Error(w, "worker required in body", http.StatusBadRequest)
			return
		}
		if err := store.Heartbeat(runID, body.Worker, defaultClaimVisibility); err != nil {
			// Lost-ownership is a 409, not 500 — the worker can stop
			// heartbeating and exit cleanly without escalating to oncall.
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"heartbeat": runID})
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

