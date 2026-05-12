package cronicle

import (
	"context"
	"log/slog"
	"time"
)

// runPausePollInterval is the cadence at which the DAG walker re-checks
// the run_state.paused flag while a run is paused. Short enough that
// unpause feels responsive (< 1s typical), long enough not to thrash
// the projection store under load.
var runPausePollInterval = 500 * time.Millisecond

// awaitRunPauseClear blocks while run_state.paused = 1 for the given
// run. Returns immediately when:
//   - the flag is clear (typical fast path; no allocation, single SQL read)
//   - ctx is canceled (run was canceled while paused — caller proceeds
//     and downstream cancel/skip gates take over)
//   - the projection store is unavailable (fail open, mirrors other gates)
//
// Logs a one-shot info entry the first time it observes paused=1 so the
// operator can correlate "why isn't the next task launching?" in
// transcripts without flooding the log every poll.
func awaitRunPauseClear(ctx context.Context, runID, task, schedule string) {
	if runID == "" {
		return
	}
	st := StateStore()
	if st == nil {
		return
	}
	paused, err := st.IsRunPaused(runID)
	if err != nil {
		slog.Warn("run pause check failed; launching task",
			"run_id", runID, "task", task, "error", err.Error())
		return
	}
	if !paused {
		return
	}
	// Emit a single "blocked on pause" entry per (task, pause-epoch).
	// Repeated polls don't re-log; the next pause epoch will re-log on
	// its first poll thanks to the StateStore round-trip.
	slog.Info("task launch blocked by run pause",
		"entry_type", "task_pause_blocked",
		"run_id", runID,
		"task", task,
		"schedule", schedule,
	)
	ticker := time.NewTicker(runPausePollInterval)
	defer ticker.Stop()
	for {
		// ctx may be nil in test harnesses that didn't plumb a RunCtx
		// onto the schedule. Treat nil as "no cancel signal possible"
		// and keep polling.
		if ctx != nil {
			select {
			case <-ctx.Done():
				slog.Info("run pause aborted by cancel",
					"entry_type", "task_pause_aborted",
					"run_id", runID,
					"task", task,
				)
				return
			case <-ticker.C:
			}
		} else {
			<-ticker.C
		}
		paused, err := st.IsRunPaused(runID)
		if err != nil {
			slog.Warn("run pause re-check failed; launching task",
				"run_id", runID, "task", task, "error", err.Error())
			return
		}
		if !paused {
			slog.Info("run unpaused; launching task",
				"entry_type", "task_pause_cleared",
				"run_id", runID,
				"task", task,
			)
			return
		}
	}
}

// taskCanceledInRun returns true when the projection has the (run, task)
// row in status='canceled'. The walker calls this per-node to honor a
// retroactive task cancel from POST /v1/runs/{id}/tasks/{task}/cancel.
// Fails open on store errors — losing a cancel is preferable to
// silently freezing the whole run.
func taskCanceledInRun(runID, task string) bool {
	st := StateStore()
	if st == nil {
		return false
	}
	canceled, err := st.IsTaskCanceledInRun(runID, task)
	if err != nil {
		slog.Warn("task cancel check failed; running task",
			"run_id", runID, "task", task, "error", err.Error())
		return false
	}
	return canceled
}

// taskIsSkipped consults the projection store for a per-task skip flag.
// Returns (skipped, reason). When the store is unavailable the gate
// fails open (returns false) — the projection is a derived view, not
// the source of truth.
//
// Called per-node on the DAG walker hot path; keep allocations and
// query cost minimal. The underlying state lookup is a single PK
// SELECT against an in-memory or WAL-mode SQLite table.
func taskIsSkipped(schedule, task string) (bool, string) {
	st := StateStore()
	if st == nil {
		return false, ""
	}
	skipped, err := st.IsTaskSkipped(schedule, task)
	if err != nil {
		slog.Warn("task skip check failed; running task",
			"schedule", schedule, "task", task, "error", err.Error())
		return false, ""
	}
	if !skipped {
		return false, ""
	}
	// Reason lookup is best-effort — if it fails we still skip, just
	// without the explanatory string.
	state, err := st.GetTaskState(schedule, task)
	if err != nil {
		return true, ""
	}
	return true, state.Reason
}
