package cronicle

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// runnerStateResponse is the public shape of GET /v1/runner/state.
// Mirrors the schedule/run state responses for consistency.
type runnerStateResponse struct {
	Drained   bool   `json:"drained"`
	DrainedAt string `json:"drained_at,omitempty"`
	DrainedBy string `json:"drained_by,omitempty"`
	Reason    string `json:"reason,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// handleRunnerState answers GET /v1/runner/state — the runner-wide
// control row. A clean store reports {drained: false} so consumers
// can check this once and route based on the response.
func (s *listenServer) handleRunnerState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	st := s.stateSrc()
	if st == nil {
		writeJSON(w, http.StatusOK, runnerStateResponse{})
		return
	}
	writeJSON(w, http.StatusOK, runnerStateFromStore(st))
}

// handleRunnerDrain marks the runner drained. While drained:
//   - cron ticks are skipped (entry_type=schedule_skipped, reason=drained)
//   - listener triggers return 503
//   - DAG walkers block at task-launch boundaries
//
// In-flight tasks complete normally. Idempotent — re-draining preserves
// drained_at so "how long has the runner been off?" reads correctly.
//
// Body shape mirrors the pause endpoints: optional JSON {actor, reason}
// or X-Actor / X-Reason headers.
func (s *listenServer) handleRunnerDrain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	st := s.stateSrc()
	if st == nil {
		http.Error(w, "state store unavailable", http.StatusServiceUnavailable)
		return
	}
	actor, reason := readPauseBody(r)
	if err := st.SetDrained(actor, reason); err != nil {
		slog.Error("drain failed", "error", err.Error())
		http.Error(w, "drain failed", http.StatusInternalServerError)
		return
	}
	slog.Info("runner drained",
		"entry_type", "runner_drained",
		"actor", actor,
		"reason", reason)
	writeJSON(w, http.StatusOK, runnerStateFromStore(st))
}

// handleRunnerUndrain clears the drain flag. Idempotent.
func (s *listenServer) handleRunnerUndrain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	st := s.stateSrc()
	if st == nil {
		http.Error(w, "state store unavailable", http.StatusServiceUnavailable)
		return
	}
	actor, _ := readPauseBody(r)
	if err := st.ClearDrained(actor); err != nil {
		slog.Error("undrain failed", "error", err.Error())
		http.Error(w, "undrain failed", http.StatusInternalServerError)
		return
	}
	slog.Info("runner undrained",
		"entry_type", "runner_undrained",
		"actor", actor)
	writeJSON(w, http.StatusOK, runnerStateFromStore(st))
}

func runnerStateFromStore(st *state.Store) runnerStateResponse {
	out := runnerStateResponse{}
	row, err := st.GetRunnerState()
	if err != nil {
		slog.Warn("get runner state failed", "error", err.Error())
		return out
	}
	out.Drained = row.Drained
	out.DrainedBy = row.DrainedBy
	out.Reason = row.Reason
	if !row.DrainedAt.IsZero() {
		out.DrainedAt = row.DrainedAt.UTC().Format(time.RFC3339Nano)
	}
	if !row.UpdatedAt.IsZero() {
		out.UpdatedAt = row.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	return out
}
