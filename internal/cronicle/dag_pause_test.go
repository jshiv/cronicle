package cronicle

import (
	"context"
	"testing"
	"time"
)

// TestAwaitRunPauseClear_FastPathWhenUnpaused: the gate returns
// immediately when no pause flag is set. The first call is a single
// SQL read, no goroutine work.
func TestAwaitRunPauseClear_FastPathWhenUnpaused(t *testing.T) {
	store, _ := openState(t)
	prev := stateStore
	stateStore = store
	t.Cleanup(func() { stateStore = prev })

	done := make(chan struct{})
	go func() {
		awaitRunPauseClear(context.Background(), "R_X", "a", "daily")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("expected immediate return on unpaused run")
	}
}

// TestAwaitRunPauseClear_BlocksUntilUnpause: pausing a run holds the
// gate; unpausing releases it. Uses a short poll interval to keep the
// test fast.
func TestAwaitRunPauseClear_BlocksUntilUnpause(t *testing.T) {
	store, _ := openState(t)
	prev := stateStore
	stateStore = store
	t.Cleanup(func() { stateStore = prev })

	// Speed up the poll for testing.
	prevInterval := runPausePollInterval
	runPausePollInterval = 20 * time.Millisecond
	t.Cleanup(func() { runPausePollInterval = prevInterval })

	if err := store.PauseRun("R_BLK", "test", "test pause"); err != nil {
		t.Fatalf("PauseRun: %v", err)
	}

	released := make(chan struct{})
	start := time.Now()
	go func() {
		awaitRunPauseClear(context.Background(), "R_BLK", "a", "daily")
		close(released)
	}()

	// Make sure the goroutine is actually blocked.
	select {
	case <-released:
		t.Fatalf("gate released before unpause (%v elapsed)", time.Since(start))
	case <-time.After(100 * time.Millisecond):
	}

	// Unpause and confirm release within a few poll ticks.
	if err := store.ResumeRun("R_BLK", "test"); err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatalf("gate did not release within 1s of unpause")
	}
}

// TestAwaitRunPauseClear_AbortsOnCtxCancel: a canceled run context
// exits the wait loop immediately so cancel of a paused run isn't
// stuck behind unpause.
func TestAwaitRunPauseClear_AbortsOnCtxCancel(t *testing.T) {
	store, _ := openState(t)
	prev := stateStore
	stateStore = store
	t.Cleanup(func() { stateStore = prev })

	prevInterval := runPausePollInterval
	runPausePollInterval = 20 * time.Millisecond
	t.Cleanup(func() { runPausePollInterval = prevInterval })

	if err := store.PauseRun("R_CX", "test", ""); err != nil {
		t.Fatalf("PauseRun: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	released := make(chan struct{})
	go func() {
		awaitRunPauseClear(ctx, "R_CX", "a", "daily")
		close(released)
	}()

	// Wait a moment to ensure we're inside the poll loop.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatalf("ctx cancel did not abort the gate")
	}

	// And the run is still flagged paused — gate just exits, doesn't
	// alter projection state.
	if paused, _ := store.IsRunPaused("R_CX"); !paused {
		t.Fatalf("expected run still paused after gate abort")
	}
}

// TestAwaitRunPauseClear_BlocksOnDrain: the runner-wide drain flag
// blocks task launches the same way per-run pause does. Undraining
// releases the gate.
func TestAwaitRunPauseClear_BlocksOnDrain(t *testing.T) {
	store, _ := openState(t)
	prev := stateStore
	stateStore = store
	t.Cleanup(func() { stateStore = prev })

	prevInterval := runPausePollInterval
	runPausePollInterval = 20 * time.Millisecond
	t.Cleanup(func() { runPausePollInterval = prevInterval })

	if err := store.SetDrained("test", "global"); err != nil {
		t.Fatalf("SetDrained: %v", err)
	}

	released := make(chan struct{})
	go func() {
		awaitRunPauseClear(context.Background(), "R_DRAIN", "a", "daily")
		close(released)
	}()

	select {
	case <-released:
		t.Fatalf("gate released while drained")
	case <-time.After(100 * time.Millisecond):
	}

	if err := store.ClearDrained("test"); err != nil {
		t.Fatalf("ClearDrained: %v", err)
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatalf("gate did not release within 1s of undrain")
	}
}

// TestAwaitRunPauseClear_NoStoreFailsOpen: with stateStore nil the
// gate returns immediately so a missing projection doesn't freeze
// every run.
func TestAwaitRunPauseClear_NoStoreFailsOpen(t *testing.T) {
	prev := stateStore
	stateStore = nil
	t.Cleanup(func() { stateStore = prev })

	done := make(chan struct{})
	go func() {
		awaitRunPauseClear(context.Background(), "R_NO_STORE", "a", "daily")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("expected immediate return when stateStore is nil")
	}
}
