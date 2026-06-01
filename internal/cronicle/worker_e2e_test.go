package cronicle

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// TestDistributed_ProducerWorkerEndToEnd exercises the real distributed
// loop the production worker pods run, which had NO test coverage before:
//
//	producer enqueue (enqueueAdapter, authoritative store)
//	  -> HTTP claim   (GET /v1/jobs, handleClaimJob)
//	  -> worker execute (httpWorker.execute -> ExecuteTasks)
//	  -> event shipping (POST /v1/events -> producer projection)
//	  -> HTTP ack     (POST /v1/jobs/{id}/ack)
//
// It also asserts the producer/worker ${last_run} contract end-to-end:
// the boundary is resolved on the producer and must survive the wire to
// the worker — the exact regression that shipped in #126, where the
// worker re-read its own empty in-memory store and got nothing.
func TestDistributed_ProducerWorkerEndToEnd(t *testing.T) {
	const token = "e2e-token"

	// --- Producer: authoritative store behind the real listener mux. ---
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	srv := &listenServer{
		token:       token,
		confSrc:     func() *Config { return nil },
		stateSrc:    func() state.Backend { return store },
		liveSinkSrc: func() *state.LiveSink { return nil },
	}
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)

	// A prior successful run — the boundary ${last_run} should resolve to.
	prior := time.Now().Add(-26 * time.Hour).Truncate(time.Second)
	seedRun(t, store, "R1", "daily", "succeeded", prior)

	// Producer resolves ${last_run} at enqueue and writes the job to the
	// store's queue (the same path cron ticks / triggers take).
	out := filepath.Join(t.TempDir(), "last_run.txt")
	sch := Schedule{
		Name:  "daily",
		RunID: "R2",
		Tasks: []Task{writeLastRunTask(out)},
	}
	drainEnqueue(store, sch.JSON())

	// --- Worker: ship run events back to the producer over HTTP. ---
	// Installing the shipper as the slog default is what StartHTTPWorker
	// does; here we do it directly so the worker's projection events reach
	// the producer's /v1/events and update its run history.
	prevLog := slog.Default()
	shipper := newEventShipper(ts.URL, token)
	slog.SetDefault(slog.New(shipper))
	t.Cleanup(func() { slog.SetDefault(prevLog) })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w := &httpWorker{
		opts: HTTPWorkerOptions{
			ProducerURL:    ts.URL,
			Token:          token,
			WorkerID:       "W1",
			PollBlock:      2 * time.Second,
			HeartbeatEvery: time.Hour, // never fires during the test
		},
		client:     &http.Client{Timeout: 10 * time.Second},
		ctx:        ctx,
		activeRuns: map[string]context.CancelFunc{},
	}

	// Claim the job over HTTP and run it.
	job, ok, err := w.pollOnce()
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if !ok {
		t.Fatal("pollOnce returned no job, want R2")
	}
	if job.RunID != "R2" || job.Schedule != "daily" {
		t.Fatalf("claimed job = (%q,%q), want (R2,daily)", job.RunID, job.Schedule)
	}
	w.execute(w.opts.Path, job)

	// Flush shipped events into the producer projection.
	shipper.stop()
	slog.SetDefault(prevLog)

	// Assert 1: the worker task saw the PRODUCER-resolved ${last_run},
	// proving the boundary crossed the wire (not re-read from the worker's
	// own store, which has no history).
	if got := readFile(t, out); got != prior.UTC().Format(time.RFC3339) {
		t.Errorf("worker task ${last_run} = %q, want producer-resolved %q", got, prior.UTC().Format(time.RFC3339))
	}

	// Assert 2: the producer projection reflects the run as succeeded,
	// via events shipped back over HTTP. Apply is synchronous on ingest,
	// but allow a brief window for the final flush POST to land.
	if !eventuallyRunStatus(t, store, "R2", "succeeded", 2*time.Second) {
		t.Errorf("producer projection never showed R2 succeeded")
	}
}

// eventuallyRunStatus polls the store until run runID reaches want or the
// timeout elapses.
func eventuallyRunStatus(t *testing.T, store state.Backend, runID, want string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		runs, err := store.ListRuns(state.ListFilter{Limit: 50})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		for _, r := range runs {
			if r.RunID == runID && r.Status == want {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}
