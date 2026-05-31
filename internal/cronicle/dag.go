package cronicle

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// lastSuccessfulRunStart returns the start time of the most recent
// SUCCEEDED run of the named schedule, excluding the in-flight run.
// Best-effort: ok=false when there's no state backend, no prior success,
// or the query errors — callers treat that as "no prior run".
func lastSuccessfulRunStart(schedule, currentRunID string) (time.Time, bool) {
	st := StateStore()
	if st == nil {
		return time.Time{}, false
	}
	runs, err := st.ListRuns(state.ListFilter{Schedule: schedule, Status: "succeeded", Limit: 1})
	if err != nil {
		slog.Warn("last-run lookup failed; ${last_run} will be empty",
			"schedule", schedule, "error", err.Error())
		return time.Time{}, false
	}
	for _, r := range runs {
		if r.RunID == currentRunID || r.StartedAt.IsZero() {
			continue
		}
		return r.StartedAt, true
	}
	return time.Time{}, false
}

// ExecuteTasks executes all tasks in dependency order, running independent tasks concurrently.
func (schedule Schedule) ExecuteTasks() {
	var now time.Time
	if (schedule.Now == time.Time{}) {
		now = time.Now().In(time.Local)
	} else {
		now = schedule.Now
	}

	taskMap := schedule.TaskMap()

	deps := make(map[string][]string)
	taskNames := make([]string, 0, len(schedule.Tasks))
	for _, task := range schedule.Tasks {
		deps[task.Name] = task.Depends
		taskNames = append(taskNames, task.Name)
	}

	// Ensure tasks know their parent run id so per-task events carry it.
	// Producers (ProduceSchedule, listener trigger handlers, ExecTasks) set
	// schedule.RunID before reaching here; if a caller didn't, mint one
	// now so the projection still gets a coherent grouping rather than
	// silently dropping events.
	if schedule.RunID == "" {
		schedule.RunID = newRunID()
	}
	for i := range schedule.Tasks {
		schedule.Tasks[i].RunID = schedule.RunID
		schedule.Tasks[i].RunCtx = schedule.RunCtx
	}
	taskMap = schedule.TaskMap()

	// Schedule-scoped scratch dir: a single dir per schedule run, shared by
	// every task within it via task.ScratchDir (substituted into prompts as
	// ${scratch}). The cronicle-native pattern for cross-task context: an
	// upstream task writes a file, a downstream task reads it. Survives the
	// run so transcripts and post-hoc audits can reference produced
	// artifacts. Created best-effort — if mkdir fails, tasks just see an
	// empty ScratchDir and ${scratch} substitution is a no-op.
	scratchDir := schedule.scratchDirFor(now)
	if scratchDir != "" {
		_ = os.MkdirAll(scratchDir, 0o755)
		for name, t := range taskMap {
			t.ScratchDir = scratchDir
			taskMap[name] = t
		}
	}

	// Last successful run start, surfaced to tasks as ${last_run} /
	// ${last_run_epoch} so "incremental since last run" jobs read the
	// real boundary instead of guessing a lookback window. Best-effort:
	// zero (→ empty token) on the first run or when state is unavailable.
	if lastRun, ok := lastSuccessfulRunStart(schedule.Name, schedule.RunID); ok {
		for name, t := range taskMap {
			t.LastRun = lastRun
			taskMap[name] = t
		}
	}

	startedAt := time.Now()
	startAttrs := []any{
		"entry_type", "schedule_start",
		"run_id", schedule.RunID,
		"schedule", schedule.Name,
		"source", schedule.Source,
		"clock", now.Format(time.Kitchen),
		"date", now.Format(time.RFC850),
		"tasks", taskNames,
		"dag", dagString(deps),
	}
	if scratchDir != "" {
		startAttrs = append(startAttrs, "scratch", scratchDir)
	}
	slog.Info("schedule started", startAttrs...)

	continueOnFailure := make(map[string]bool)
	for _, task := range schedule.Tasks {
		if task.ContinueOnFailure {
			continueOnFailure[task.Name] = true
		}
	}

	walkErr := walkDAG(deps, func(name string) error {
		task := taskMap[name]
		// Run-pause gate: a POST /v1/runs/{id}/pause may land mid-walk.
		// Block here before launching the task body. Resume (POST
		// /v1/runs/{id}/unpause) clears the flag; the poll loop picks
		// up on its next tick. Cancel of the whole run (via ctx) takes
		// precedence — a paused run that's then canceled exits the
		// loop immediately rather than waiting for unpause.
		awaitRunPauseClear(schedule.RunCtx, schedule.RunID, name, schedule.Name)
		// Per-task cancel gate: a POST /v1/runs/{id}/tasks/{task}/cancel
		// landed against this run before the walker reached this node.
		// The projection already shows status='canceled' for this task
		// (sticky); the walker emits the matching event and returns nil
		// so downstream cascade entries are also walked-and-skipped.
		// Cancel takes precedence over skip — if both flags are set,
		// the cancel record wins.
		if taskCanceledInRun(schedule.RunID, name) {
			slog.Info("task canceled",
				"entry_type", "task_canceled",
				"run_id", schedule.RunID,
				"schedule", schedule.Name,
				"task", name,
				"reason", "canceled via API before launch",
			)
			return nil
		}
		// Per-task skip gate. Operators set the flag via
		// POST /v1/schedules/{name}/tasks/{task}/skip; the DAG walker
		// records a task_skipped event and treats the node as a
		// vacuous success so dependents still run. Failing open on
		// store errors mirrors the schedule-pause gate.
		if skipped, why := taskIsSkipped(schedule.Name, name); skipped {
			slog.Info("task skipped",
				"entry_type", "task_skipped",
				"run_id", schedule.RunID,
				"schedule", schedule.Name,
				"task", name,
				"reason", why,
			)
			return nil
		}
		_, err := task.Execute(now)
		return err
	}, continueOnFailure, func(node, failedDep string) {
		slog.Warn("task skipped: upstream dependency failed",
			"entry_type", "task_dep_failed",
			"run_id", schedule.RunID,
			"schedule", schedule.Name,
			"task", node,
			"failed_dependency", failedDep,
		)
	})
	durationMs := time.Since(startedAt).Milliseconds()

	if walkErr != nil {
		slog.Error("schedule failed",
			"entry_type", "schedule_complete",
			"run_id", schedule.RunID,
			"schedule", schedule.Name,
			"task_count", len(schedule.Tasks),
			"duration_ms", durationMs,
			"success", false,
			"error", walkErr.Error(),
		)
	} else {
		slog.Info("schedule complete",
			"entry_type", "schedule_complete",
			"run_id", schedule.RunID,
			"schedule", schedule.Name,
			"task_count", len(schedule.Tasks),
			"duration_ms", durationMs,
			"success", true,
		)
	}
}

// walkDAG executes fn for each node in topological order, running independent
// nodes concurrently. When fn returns an error for a node, all transitive
// dependents are skipped (marked failed without calling fn) unless
// continueOnFailure[dependent] is true. onDepFailed is called for every
// skipped dependent with the name of the first failed upstream dependency.
func walkDAG(deps map[string][]string, fn func(string) error, continueOnFailure map[string]bool, onDepFailed func(node, failedDep string)) error {
	inDegree := make(map[string]int)
	dependents := make(map[string][]string)

	for node := range deps {
		if _, ok := inDegree[node]; !ok {
			inDegree[node] = 0
		}
		for _, dep := range deps[node] {
			inDegree[node]++
			dependents[dep] = append(dependents[dep], node)
		}
	}

	var mu sync.Mutex
	var firstErr error
	failed := make(map[string]bool)
	done := make(chan string, len(deps))

	launch := func(node string) {
		go func() {
			if err := fn(node); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				failed[node] = true
				mu.Unlock()
			}
			done <- node
		}()
	}

	running := 0
	for node, degree := range inDegree {
		if degree == 0 {
			running++
			launch(node)
		}
	}

	for running > 0 {
		completed := <-done
		running--
		for _, dependent := range dependents[completed] {
			inDegree[dependent]--
			if inDegree[dependent] == 0 {
				// Check whether any of this dependent's upstream deps failed.
				mu.Lock()
				var failedDep string
				if !continueOnFailure[dependent] {
					for _, dep := range deps[dependent] {
						if failed[dep] {
							failedDep = dep
							break
						}
					}
				}
				if failedDep != "" {
					// Skip this dependent: mark it failed for cascade and
					// signal completion without launching a goroutine.
					failed[dependent] = true
					mu.Unlock()
					if onDepFailed != nil {
						onDepFailed(dependent, failedDep)
					}
					running++
					done <- dependent
				} else {
					mu.Unlock()
					running++
					launch(dependent)
				}
			}
		}
	}

	return firstErr
}

// scratchDirFor computes the absolute scratch dir for one schedule run.
// Lives under <croniclePath>/.cronicle/scratch/<schedule>/<utc-runid>/ so
// concurrent runs of the same schedule don't collide and each run is
// addressable by timestamp. Returns "" if the schedule has no tasks (no
// croniclePath to resolve), in which case dag.go skips the mkdir and
// ${scratch} substitution becomes a no-op.
func (schedule *Schedule) scratchDirFor(now time.Time) string {
	if len(schedule.Tasks) == 0 {
		return ""
	}
	croniclePath := schedule.Tasks[0].CroniclePath
	if croniclePath == "" {
		return ""
	}
	runID := now.UTC().Format("20060102T150405Z")
	return filepath.Join(croniclePath, ".cronicle", "scratch", schedule.Name, runID)
}

// downstreamTasks returns the transitive set of tasks that depend on
// `root` within the schedule's DAG. Used by the task-cancel listener
// endpoint to compute the cascade set — every downstream task of the
// canceled task is also marked canceled in the projection so the DAG
// walker skips them at launch time.
//
// Returned in deterministic insertion order (BFS layer-by-layer) so
// downstream consumers (logs, audit lines) see a stable ordering.
// `root` itself is NOT included; callers prepend it explicitly when
// they want the full target set.
func (schedule Schedule) downstreamTasks(root string) []string {
	dependents := make(map[string][]string)
	for _, t := range schedule.Tasks {
		for _, dep := range t.Depends {
			dependents[dep] = append(dependents[dep], t.Name)
		}
	}
	var out []string
	seen := map[string]bool{root: true}
	queue := []string{root}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, child := range dependents[cur] {
			if seen[child] {
				continue
			}
			seen[child] = true
			out = append(out, child)
			queue = append(queue, child)
		}
	}
	return out
}

// dagString produces a human-readable representation of the DAG for logging.
func dagString(deps map[string][]string) string {
	var sb strings.Builder
	sb.WriteString("DAG:\n")
	for node, nodeDeps := range deps {
		if len(nodeDeps) == 0 {
			fmt.Fprintf(&sb, "  %s\n", node)
		} else {
			fmt.Fprintf(&sb, "  %s -> [%s]\n", node, strings.Join(nodeDeps, ", "))
		}
	}
	return sb.String()
}
