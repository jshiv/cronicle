package cronicle

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// TestDistributed_CancelOverControlSSE exercises the producer→worker
// cancel path that only the /tmp smoke test covered before: a running job
// is canceled via the producer API, and the cancel must reach the worker
// over its SSE control channel and abort the in-flight task.
//
//	POST /v1/runs/{id}/cancel
//	  -> store.Cancel + store.PushControl(workerID, {cancel})
//	  -> serveControlSSE streams it on /v1/workers/{id}/control
//	  -> httpQueueClient.consumeSSE -> handleControl -> worker.cancelRun
//	  -> the run's ctx is canceled -> the shell task (exec.CommandContext)
//	     is killed before it finishes.
func TestDistributed_CancelOverControlSSE(t *testing.T) {
	const token = "cancel-token"
	const workerID = "WC"

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

	// Task: marks "started", sleeps 3s, marks "finished". The cancel must
	// kill it (within ms) so "finished" never appears. The observation
	// window below is LONGER than the sleep, so a broken cancel — where the
	// task runs to completion — is caught when "finished" appears at ~3s.
	const taskSleep = 3 * time.Second
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	finished := filepath.Join(dir, "finished")
	sch := Schedule{
		Name:  "cancelme",
		RunID: "RC",
		Tasks: []Task{{Name: "long", Command: []string{"bash", "-c",
			"touch " + started + "; sleep 3; touch " + finished}}},
	}
	drainEnqueue(store, sch.JSON())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	hc := &httpQueueClient{
		producerURL: ts.URL,
		token:       token,
		workerID:    workerID,
		client:      &http.Client{Timeout: 35 * time.Second},
		ctx:         ctx,
	}
	w := newWorker(ctx, hc, workerID, time.Second, time.Hour)
	go hc.controlLoop(w.cancelRun) // subscribe to the control SSE
	go w.consume("")               // claim + execute the long job

	// Precondition for a deterministic test: the task must be running (so
	// there's something to cancel and the run is registered) AND the
	// control SSE must be subscribed (PushControl drops to an unsubscribed
	// worker). Poll-ping to detect subscription — pings are no-ops on the
	// worker.
	subscribed := false
	deadline := time.Now().Add(6 * time.Second)
	for {
		if !subscribed {
			subscribed = store.PushControl(workerID, state.ControlMsg{Type: "ping"})
		}
		_, startErr := os.Stat(started)
		if subscribed && startErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("preconditions not met (task started=%v, sse subscribed=%v)", startErr == nil, subscribed)
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Cancel through the real producer API.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/runs/RC/cancel", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("cancel POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel returned %d, want 200", resp.StatusCode)
	}

	// Observe for LONGER than the sleep: a working cancel kills the task in
	// ms (finished never appears); a broken cancel lets it run to completion
	// (finished appears at ~3s) and is caught here.
	observeUntil := time.Now().Add(taskSleep + 2*time.Second)
	for time.Now().Before(observeUntil) {
		if _, err := os.Stat(finished); err == nil {
			t.Fatal("task ran to completion — cancel over the control SSE did not abort it")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(started); err != nil {
		t.Fatalf("sanity: task never started: %v", err)
	}
	if _, err := os.Stat(finished); err == nil {
		t.Fatal("finished exists — task was not aborted by the cancel")
	}
}
