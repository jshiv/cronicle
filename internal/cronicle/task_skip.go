package cronicle

import "log/slog"

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
