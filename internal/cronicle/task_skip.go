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

// awaitRunPauseClear blocks while either the per-run pause flag or the
// runner-wide drain flag is set. Returns immediately when:
//   - both flags are clear (typical fast path; two single-PK SQL reads)
//   - ctx is canceled (run was canceled while waiting — caller proceeds
//     and downstream cancel/skip gates take over)
//   - the projection store is unavailable (fail open, mirrors other gates)
//
// Logs a one-shot info entry the first time it observes a block so the
// operator can correlate "why isn't the next task launching?" in
// transcripts without flooding the log every poll. The block reason
// (pause vs drain) is included so the operator knows which verb to
// reverse.
func awaitRunPauseClear(ctx context.Context, runID, task, schedule string) {
	if runID == "" {
		return
	}
	st := StateStore()
	if st == nil {
		return
	}
	blocked, reason := launchBlockedReason(st, runID)
	if !blocked {
		return
	}
	slog.Info("task launch blocked",
		"entry_type", "task_pause_blocked",
		"run_id", runID,
		"task", task,
		"schedule", schedule,
		"reason", reason,
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
				slog.Info("run launch wait aborted by cancel",
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
		blocked, _ = launchBlockedReason(st, runID)
		if !blocked {
			slog.Info("run launch wait cleared",
				"entry_type", "task_pause_cleared",
				"run_id", runID,
				"task", task,
			)
			return
		}
	}
}

// launchBlockedReason returns (blocked, reason) — true when either the
// run is paused or the runner is drained. Drain takes precedence in the
// reason string because it's the more global signal. Read-only; the
// gate calls this on the hot path.
func launchBlockedReason(st storeWithGates, runID string) (bool, string) {
	if drained, err := st.IsDrained(); err == nil && drained {
		return true, "drained"
	}
	if paused, err := st.IsRunPaused(runID); err == nil && paused {
		return true, "paused"
	}
	return false, ""
}

// storeWithGates is the narrow interface awaitRunPauseClear needs. Pulled
// out so the gate is testable without a real *state.Store and so future
// gate flags (per-schedule pause? cancel?) can be added without churning
// every call site.
type storeWithGates interface {
	IsDrained() (bool, error)
	IsRunPaused(runID string) (bool, error)
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
