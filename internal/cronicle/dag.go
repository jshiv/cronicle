package cronicle

import (
	"fmt"
	"log/slog"
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

	startedAt := time.Now()
	slog.Info("schedule started",
		"entry_type", "schedule_start",
		"schedule", schedule.Name,
		"clock", now.Format(time.Kitchen),
		"date", now.Format(time.RFC850),
		"tasks", taskNames,
		"dag", dagString(deps),
	)

	walkErr := walkDAG(deps, func(name string) error {
		task := taskMap[name]
		_, err := task.Execute(now)
		return err
	})
	durationMs := time.Since(startedAt).Milliseconds()

	if walkErr != nil {
		slog.Error("schedule failed",
			"entry_type", "schedule_complete",
			"schedule", schedule.Name,
			"task_count", len(schedule.Tasks),
			"duration_ms", durationMs,
			"success", false,
			"error", walkErr.Error(),
		)
	} else {
		slog.Info("schedule complete",
			"entry_type", "schedule_complete",
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
