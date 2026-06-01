package cronicle

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// TestSelfWorkerPool_RunsJobsConcurrently verifies the pool actually
// parallelizes: N sleepy jobs across N workers finish in roughly one
// sleep, not N sleeps. A single (serial) worker would take ~N× as long.
func TestSelfWorkerPool_RunsJobsConcurrently(t *testing.T) {
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	const n = 4
	const sleep = 400 * time.Millisecond
	dir := t.TempDir()
	for i := 0; i < n; i++ {
		out := filepath.Join(dir, fmt.Sprintf("job%d.done", i))
		runID := fmt.Sprintf("R%d", i)
		sch := Schedule{
			Name:  "demo",
			RunID: runID,
			Tasks: []Task{{Name: "t", Command: []string{"bash", "-c", "sleep 0.4; printf ok > " + out}}},
		}
		payload, _ := json.Marshal(sch)
		if err := store.Enqueue(runID, "demo", payload); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	start := time.Now()
	selfWorkerPool(ctx, store, "", n, &wg)

	// Wait for all jobs to finish (all marker files present).
	deadline := time.Now().Add(5 * time.Second)
	for {
		done := 0
		for i := 0; i < n; i++ {
			if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("job%d.done", i))); err == nil {
				done++
			}
		}
		if done == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d jobs finished before timeout", done, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	elapsed := time.Since(start)

	// Serial would be ~n*sleep (1.6s). Concurrent is ~sleep + overhead.
	// Assert comfortably below the serial floor.
	if elapsed > time.Duration(n)*sleep-200*time.Millisecond {
		t.Errorf("pool of %d ran in %v — looks serial (serial floor ~%v)", n, elapsed, time.Duration(n)*sleep)
	}
}
