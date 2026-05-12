package cronicle

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

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

	walkErr := walkDAG(deps, func(name string) error {
		task := taskMap[name]
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

// walkDAG executes fn for each node in topological order, running independent nodes concurrently.
func walkDAG(deps map[string][]string, fn func(string) error) error {
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
	done := make(chan string, len(deps))

	launch := func(node string) {
		go func() {
			if err := fn(node); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
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
				running++
				launch(dependent)
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
