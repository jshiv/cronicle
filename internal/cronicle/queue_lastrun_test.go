package cronicle

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// drainEnqueue pushes one payload through enqueueAdapter synchronously:
// send, close, then run the adapter (which ranges until the channel is
// closed and returns). The resolved job is then claimable from store.
func drainEnqueue(store state.Backend, payload []byte) {
	in := make(chan []byte, 1)
	in <- payload
	close(in)
	enqueueAdapter(in, store)
}

// TestEnqueueAdapter_ResolvesLastRunOnProducer locks in the producer-side
// resolution of ${last_run}: enqueueAdapter must read the authoritative
// store and bake the prior successful run's start into each Task.LastRun
// in the payload, so the worker executes from a fully-resolved job.
func TestEnqueueAdapter_ResolvesLastRunOnProducer(t *testing.T) {
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	// A prior successful run of "daily" — the boundary ${last_run} should
	// resolve to.
	prior := time.Now().Add(-26 * time.Hour).Truncate(time.Second)
	seedRun(t, store, "R1", "daily", "succeeded", prior)

	sch := Schedule{
		Name:  "daily",
		RunID: "R2",
		Tasks: []Task{{Name: "t1", Command: []string{"/bin/echo", "${last_run}"}}},
	}
	drainEnqueue(store, sch.JSON())

	job, err := store.Claim("W1", time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	var got Schedule
	if err := json.Unmarshal(job.Payload, &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	if len(got.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(got.Tasks))
	}
	if !got.Tasks[0].LastRun.Equal(prior) {
		t.Errorf("Task.LastRun = %v, want %v", got.Tasks[0].LastRun, prior)
	}
}

// TestEnqueueAdapter_NoPriorRunLeavesLastRunZero: with no prior success,
// LastRun stays zero (→ ${last_run} empty → "full backfill"). The worker
// consumes that value as-is and never reads its own store.
func TestEnqueueAdapter_NoPriorRunLeavesLastRunZero(t *testing.T) {
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	sch := Schedule{
		Name:  "fresh",
		RunID: "R1",
		Tasks: []Task{{Name: "t1", Command: []string{"/bin/echo", "${last_run}"}}},
	}
	drainEnqueue(store, sch.JSON())

	job, err := store.Claim("W1", time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	var got Schedule
	if err := json.Unmarshal(job.Payload, &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	if !got.Tasks[0].LastRun.IsZero() {
		t.Errorf("Task.LastRun = %v, want zero on first run", got.Tasks[0].LastRun)
	}
}
