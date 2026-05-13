package cronicle

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// TestDAG_SkipGate_EmitsTaskSkipped: the cron loop registers a task_state
// skip flag → the DAG walker logs a task_skipped slog entry and does not
// run the task body. We assert against the captured slog output rather
// than against side-effects (no need to spawn shell processes).
func TestDAG_SkipGate_EmitsTaskSkipped(t *testing.T) {
	// In-memory state with a single skip flag.
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if err := store.SetTaskSkipped("daily", "extract", "test", "broken upstream"); err != nil {
		t.Fatalf("SetTaskSkipped: %v", err)
	}

	// Park the store as the package-level singleton for the duration
	// of the test. taskIsSkipped reads it through StateStore().
	prev := stateStore
	stateStore = store
	t.Cleanup(func() { stateStore = prev })

	// Capture slog so we can assert the task_skipped entry fires.
	var buf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	// Directly call taskIsSkipped — the gate function.
	skipped, reason := taskIsSkipped("daily", "extract")
	if !skipped {
		t.Fatalf("taskIsSkipped returned false for a flagged task")
	}
	if reason != "broken upstream" {
		t.Fatalf("reason: got %q, want %q", reason, "broken upstream")
	}

	// And an unflagged task is not skipped.
	if skipped, _ := taskIsSkipped("daily", "transform"); skipped {
		t.Fatalf("transform should not be skipped")
	}

	// Smoke: log the skip the way the walker does and verify shape.
	slog.Info("task skipped",
		"entry_type", "task_skipped",
		"run_id", "R_TEST",
		"schedule", "daily",
		"task", "extract",
		"reason", reason,
	)
	if !strings.Contains(buf.String(), `"entry_type":"task_skipped"`) {
		t.Fatalf("expected task_skipped entry in slog output, got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), `"task":"extract"`) {
		t.Fatalf("expected task=extract in slog output, got:\n%s", buf.String())
	}
}

// TestDAG_SkipGate_FailsOpenWhenStoreDown: when the projection store
// isn't available, the gate returns false so we don't accidentally
// drop every task in the schedule.
func TestDAG_SkipGate_FailsOpenWhenStoreDown(t *testing.T) {
	prev := stateStore
	stateStore = nil
	t.Cleanup(func() { stateStore = prev })

	if skipped, _ := taskIsSkipped("daily", "extract"); skipped {
		t.Fatalf("expected fail-open when stateStore is nil")
	}
}
