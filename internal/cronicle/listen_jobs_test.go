package cronicle

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

func jobsHarness(t *testing.T) (*listenServer, *state.Store) {
	t.Helper()
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &listenServer{
		token:    "secret",
		stateSrc: func() state.Backend { return store },
	}, store
}

// TestJobs_ClaimAuthRequired: bearer auth gates the queue.
func TestJobs_ClaimAuthRequired(t *testing.T) {
	srv, _ := jobsHarness(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/jobs?worker=W1", nil)
	srv.handleClaimJob(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rr.Code)
	}
}

// TestJobs_MissingWorker: worker query param is required.
func TestJobs_MissingWorker(t *testing.T) {
	srv, _ := jobsHarness(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer secret")
	srv.handleClaimJob(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing worker: got %d, want 400", rr.Code)
	}
}

// TestJobs_ClaimReturnsJob: enqueue → claim returns 200 with the
// payload bytes, claim metadata, attempt count.
func TestJobs_ClaimReturnsJob(t *testing.T) {
	srv, store := jobsHarness(t)
	if err := store.Enqueue("R1", "daily", []byte(`{"name":"daily"}`)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/jobs?worker=W1&block=100ms", nil)
	req.Header.Set("Authorization", "Bearer secret")
	srv.handleClaimJob(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var j state.Job
	if err := json.Unmarshal(rr.Body.Bytes(), &j); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if j.RunID != "R1" || j.Schedule != "daily" || j.ClaimedBy != "W1" || j.Attempt != 1 {
		t.Fatalf("claim wrong: %+v", j)
	}
}

// TestJobs_LongPollWaitTimeout: with no enqueued jobs, the long-poll
// returns 204 after the block elapses.
func TestJobs_LongPollWaitTimeout(t *testing.T) {
	srv, _ := jobsHarness(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/jobs?worker=W1&block=50ms", nil)
	req.Header.Set("Authorization", "Bearer secret")
	t0 := time.Now()
	srv.handleClaimJob(rr, req)
	dur := time.Since(t0)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("got %d, want 204", rr.Code)
	}
	if dur < 40*time.Millisecond {
		t.Fatalf("returned too fast (%s) — long-poll didn't wait", dur)
	}
}

// TestJobs_LongPollWakesOnEnqueue: a long-poll in flight wakes when
// a job is enqueued during the wait.
func TestJobs_LongPollWakesOnEnqueue(t *testing.T) {
	srv, store := jobsHarness(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/jobs?worker=W1&block=2s", nil)
	req.Header.Set("Authorization", "Bearer secret")

	done := make(chan int, 1)
	go func() {
		srv.handleClaimJob(rr, req)
		done <- rr.Code
	}()
	time.Sleep(30 * time.Millisecond) // let the handler park
	_ = store.Enqueue("R1", "x", []byte(`{}`))

	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("got %d, want 200; body=%s", code, rr.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("long-poll did not wake on enqueue")
	}
}

// TestJobs_AckSuccess: POST /v1/jobs/{id}/ack with success=true marks
// the job done.
func TestJobs_AckSuccess(t *testing.T) {
	srv, store := jobsHarness(t)
	_ = store.Enqueue("R1", "x", []byte(`{}`))
	_, _ = store.Claim("W1", time.Minute)

	body := `{"worker":"W1","success":true}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs/R1/ack", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	srv.handleJobControl(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if n, _ := store.CountJobsByStatus(state.JobDone); n != 1 {
		t.Fatalf("expected 1 done, got %d", n)
	}
}

// TestJobs_AckFailure: ack with success=false marks the job failed +
// records the error.
func TestJobs_AckFailure(t *testing.T) {
	srv, store := jobsHarness(t)
	_ = store.Enqueue("R1", "x", []byte(`{}`))
	_, _ = store.Claim("W1", time.Minute)

	body := `{"worker":"W1","success":false,"error":"boom"}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs/R1/ack", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	srv.handleJobControl(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d", rr.Code)
	}
	if n, _ := store.CountJobsByStatus(state.JobFailedQ); n != 1 {
		t.Fatalf("expected 1 failed, got %d", n)
	}
}

// TestJobs_HeartbeatExtends: heartbeat keeps the claim alive and returns 200.
func TestJobs_HeartbeatExtends(t *testing.T) {
	srv, store := jobsHarness(t)
	_ = store.Enqueue("R1", "x", []byte(`{}`))
	_, _ = store.Claim("W1", time.Hour)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs/R1/heartbeat",
		strings.NewReader(`{"worker":"W1"}`))
	req.Header.Set("Authorization", "Bearer secret")
	srv.handleJobControl(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

// TestJobs_HeartbeatLost: a heartbeat against an expired claim returns 409.
func TestJobs_HeartbeatLost(t *testing.T) {
	srv, store := jobsHarness(t)
	_ = store.Enqueue("R1", "x", []byte(`{}`))
	_, _ = store.Claim("W1", 5*time.Millisecond)
	time.Sleep(15 * time.Millisecond)
	_, _ = store.ReapExpired()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs/R1/heartbeat",
		strings.NewReader(`{"worker":"W1"}`))
	req.Header.Set("Authorization", "Bearer secret")
	srv.handleJobControl(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 lost-claim, got %d", rr.Code)
	}
}

// TestJobs_AckBadJSON: malformed body is 400, not 500.
func TestJobs_AckBadJSON(t *testing.T) {
	srv, _ := jobsHarness(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs/R1/ack",
		strings.NewReader(`not json`))
	req.Header.Set("Authorization", "Bearer secret")
	srv.handleJobControl(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rr.Code)
	}
}

// TestJobs_LongPollClientDisconnect: client closes the connection mid-wait.
// The handler should return without panic and without deadlock.
func TestJobs_LongPollClientDisconnect(t *testing.T) {
	srv, _ := jobsHarness(t)
	rr := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/jobs?worker=W1&block=1s", nil)
	req.Header.Set("Authorization", "Bearer secret")
	done := make(chan struct{})
	go func() {
		srv.handleClaimJob(rr, req)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not return after client disconnect")
	}
}
