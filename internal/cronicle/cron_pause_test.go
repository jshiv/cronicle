package cronicle

import (
	"testing"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// TestProduceSchedule_SkippedWhenDrained verifies the runner-wide
// drain gate. Drain takes precedence over per-schedule pause: a
// drained runner skips ticks regardless of pause state.
func TestProduceSchedule_SkippedWhenDrained(t *testing.T) {
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	prev := stateStore
	stateStore = store
	t.Cleanup(func() {
		stateStore = prev
		_ = store.Close()
	})

	queue := make(chan []byte, 4)
	sch := Schedule{
		Name:      "d_test",
		Cron:      "@every 1m",
		StartDate: "2000-01-01",
		Tasks:     []Task{{Name: "t1"}},
	}

	// Drain → tick is silent.
	if err := store.SetDrained("test", "blocking"); err != nil {
		t.Fatalf("SetDrained: %v", err)
	}
	ProduceSchedule(sch, queue)()
	select {
	case payload := <-queue:
		t.Fatalf("drained runner enqueued: %s", payload)
	case <-time.After(50 * time.Millisecond):
	}

	// Undrain → tick fires.
	if err := store.ClearDrained("test"); err != nil {
		t.Fatalf("ClearDrained: %v", err)
	}
	ProduceSchedule(sch, queue)()
	select {
	case <-queue:
	case <-time.After(time.Second):
		t.Fatalf("undrained tick did not enqueue")
	}
}

// TestProduceSchedule_SkippedWhenPaused verifies the cron-loop gate: a
// schedule with an active pause row in the state store must not enqueue
// when its cron tick fires. Without state enabled the previous behavior
// is preserved.
//
// We set the package-private stateStore global directly rather than
// calling EnableStateStore — the latter rewires slog.Default across the
// process and interacts badly with test parallelism and stdout capture.
// The gate only reads StateStore(); the slog fan-out is incidental.
func TestProduceSchedule_SkippedWhenPaused(t *testing.T) {
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	prev := stateStore
	stateStore = store
	t.Cleanup(func() {
		stateStore = prev
		_ = store.Close()
	})

	queue := make(chan []byte, 4)
	sch := Schedule{
		Name:      "p_test",
		Cron:      "@every 1m",
		StartDate: "2000-01-01",
		Tasks:     []Task{{Name: "t1"}},
	}

	// Sanity: active schedule enqueues.
	ProduceSchedule(sch, queue)()
	select {
	case <-queue:
	case <-time.After(time.Second):
		t.Fatalf("expected active schedule to enqueue")
	}

	// Pause and confirm the next tick is silent.
	if err := store.SetSchedulePaused("p_test", "test", "blocking"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	ProduceSchedule(sch, queue)()
	select {
	case payload := <-queue:
		t.Fatalf("paused schedule enqueued: %s", payload)
	case <-time.After(50 * time.Millisecond):
		// good — nothing landed
	}

	// Resume and confirm the gate releases.
	if err := store.ClearSchedulePaused("p_test", "test"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	ProduceSchedule(sch, queue)()
	select {
	case <-queue:
	case <-time.After(time.Second):
		t.Fatalf("resumed schedule failed to enqueue")
	}
}
