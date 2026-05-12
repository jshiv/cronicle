package cronicle

import "log/slog"

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
