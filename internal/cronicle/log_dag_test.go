package cronicle

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/fatih/color"
)

// renderDAG runs renderScheduleStart over a schedule_start record built the
// same way ExecuteTasks builds it, with color off, and returns the tree lines.
func renderDAG(t *testing.T, tasks []string, deps map[string][]string) string {
	t.Helper()
	prev := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = prev }()

	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "schedule started", 0)
	rec.AddAttrs(
		slog.String("entry_type", "schedule_start"),
		slog.String("schedule", "etl"),
		slog.Any("tasks", tasks),
		slog.Any("depends", dependsAttr(tasks, deps)),
	)
	var b bytes.Buffer
	if err := renderScheduleStart(&b, rec); err != nil {
		t.Fatalf("renderScheduleStart: %v", err)
	}
	// Drop the "◆ etl …" title line; the rest is the tree.
	_, tree, _ := strings.Cut(b.String(), "\n")
	return tree
}

// TestRenderScheduleStart_DAGTree: the DAG used to render as a flat list of
// siblings regardless of `depends`. Children must nest under their deepest
// dependency, and fan-in nodes must name everything they wait on.
func TestRenderScheduleStart_DAGTree(t *testing.T) {
	tasks := []string{"extract", "transform_a", "transform_b", "load", "audit", "report"}
	deps := map[string][]string{
		"transform_a": {"extract"},
		"transform_b": {"extract"},
		"load":        {"transform_a", "transform_b"},
		"report":      {"load", "audit"},
	}
	want := strings.Join([]string{
		"  extract",
		"  ├─ transform_a",
		"  └─ transform_b",
		"     └─ load  ← transform_a, transform_b",
		"        └─ report  ← load, audit",
		"  audit",
		"",
		"",
	}, "\n")
	if got := renderDAG(t, tasks, deps); got != want {
		t.Fatalf("tree mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestRenderScheduleStart_DAGTreeRehydrated: worker events reach the runner
// as JSON and are rebuilt by rehydrateRecord; the depends attr must survive
// that trip so the SSE pane draws the same tree as local stdout.
func TestRenderScheduleStart_DAGTreeRehydrated(t *testing.T) {
	tasks := []string{"a", "b", "c"}
	line, _ := json.Marshal(map[string]any{
		"time": "2026-10-22T12:00:00Z", "level": "INFO", "msg": "schedule started",
		"entry_type": "schedule_start", "schedule": "etl",
		"tasks":   tasks,
		"depends": dependsAttr(tasks, map[string][]string{"b": {"a"}, "c": {"b"}}),
	})
	rec, ok := rehydrateRecord(line)
	if !ok {
		t.Fatal("rehydrateRecord failed")
	}
	prev := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = prev }()
	var b bytes.Buffer
	if err := renderScheduleStart(&b, rec); err != nil {
		t.Fatalf("renderScheduleStart: %v", err)
	}
	if !strings.Contains(b.String(), "  a\n  └─ b\n     └─ c\n") {
		t.Fatalf("chain not nested after rehydrate:\n%s", b.String())
	}
}

// TestRenderScheduleStart_DAGTreeEdgeCases: deps on tasks outside the run
// (filtered exec) are annotated rather than drawn, a cycle still lists every
// task exactly once, and a record without `depends` falls back to roots.
func TestRenderScheduleStart_DAGTreeEdgeCases(t *testing.T) {
	got := renderDAG(t, []string{"b"}, map[string][]string{"b": {"a"}})
	if !strings.Contains(got, "  b  ← a\n") {
		t.Errorf("unknown dep not annotated:\n%s", got)
	}

	got = renderDAG(t, []string{"x", "y"}, map[string][]string{"x": {"y"}, "y": {"x"}})
	for _, n := range []string{"x", "y"} {
		if c := strings.Count(got, " "+n+"\n") + strings.Count(got, " "+n+"  "); c != 1 {
			t.Errorf("cycle: %q rendered %d times:\n%s", n, c, got)
		}
	}

	if deps := dependsMap([]string{"a", "b"}, nil); len(deps) != 0 {
		t.Errorf("missing depends attr should yield no edges, got %v", deps)
	}
}
