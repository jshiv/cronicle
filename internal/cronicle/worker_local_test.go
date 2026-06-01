package cronicle

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// TestLocalQueueClient_WorkerClaimsAndExecutes drives the SAME worker loop
// the remote path uses, but over a localQueueClient — proving single-node
// execution goes through the unified pathway with no socket. The worker
// claims an enqueued job straight from the store, executes it, and acks.
func TestLocalQueueClient_WorkerClaimsAndExecutes(t *testing.T) {
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	out := filepath.Join(t.TempDir(), "ran.txt")
	sch := Schedule{
		Name:  "demo",
		RunID: "R1",
		Tasks: []Task{{Name: "t1", Command: []string{"bash", "-c", "printf ok > " + out}}},
	}
	payload, _ := json.Marshal(sch)
	if err := store.Enqueue("R1", "demo", payload); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lc := &localQueueClient{store: store, workerID: "self", ctx: ctx}
	w := newWorker(ctx, lc, "self", 100*time.Millisecond, time.Hour)
	go w.consume("")

	// The task writes the file when it executes — proof the local client
	// claimed and ran the job through the shared worker loop.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(out); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("task never ran — local queue client did not claim/execute")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
