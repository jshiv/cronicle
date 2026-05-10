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
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// listenServer is bound to the producer's send queue plus a thread-safe
// snapshot of the most recently loaded Config (the heartbeat hot-reloads
// it via confPriorGlobal). The token is captured at construction; rotate
// by restarting the process.
type listenServer struct {
	queue   chan<- []byte
	token   string
	confSrc func() *Config
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
		queue:   queue,
		token:   token,
		confSrc: func() *Config { return confPriorGlobal },
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/schedules", s.handleListSchedules)
	mux.HandleFunc("/v1/schedules/", s.handleScheduleRoute)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      10 * time.Second,
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

// handleScheduleRoute dispatches POSTs under /v1/schedules/. Two shapes:
//
//	POST /v1/schedules/{name}/trigger
//	POST /v1/schedules/{name}/tasks/{task}/trigger
//
// We split on "/" rather than mounting per-pattern routers because the
// path shape is fixed and tiny; bringing in a routing library for two
// endpoints is overkill.
func (s *listenServer) handleScheduleRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
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

	switch {
	case len(parts) == 2 && parts[1] == "trigger":
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
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
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
func (s *listenServer) triggerSchedule(sch Schedule) bool {
	sch.Now = nowInScheduleZone(sch)
	return s.send(sch.JSON(), sch.Name, "")
}

// triggerTask builds a sub-schedule containing only the requested task and
// pushes it. Depends are stripped from the task because the caller is
// asking specifically for this one to run; they're not asking for its
// upstream DAG. If the user wanted the full chain, they'd hit the
// schedule-level trigger.
func (s *listenServer) triggerTask(sch Schedule, taskName string) bool {
	sub := sch
	sub.Tasks = nil
	for _, t := range sch.Tasks {
		if t.Name == taskName {
			t.Depends = nil
			sub.Tasks = append(sub.Tasks, t)
		}
	}
	sub.Now = nowInScheduleZone(sub)
	return s.send(sub.JSON(), sch.Name, taskName)
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

