package state

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newQueueTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestQueue_EnqueueClaim: happy path. One enqueued job is claimed by
// a worker; the wire shape carries the schedule + payload.
func TestQueue_EnqueueClaim(t *testing.T) {
	s := newQueueTestStore(t)
	if err := s.Enqueue("R1", "daily", []byte(`{"schedule":"daily"}`)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if n, _ := s.CountJobsByStatus(JobPending); n != 1 {
		t.Fatalf("pending count: got %d, want 1", n)
	}
	j, err := s.Claim("W_A", time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if j.RunID != "R1" || j.Schedule != "daily" || string(j.Payload) != `{"schedule":"daily"}` {
		t.Fatalf("claimed wrong: %+v", j)
	}
	if j.ClaimedBy != "W_A" {
		t.Fatalf("claimed_by: got %q, want W_A", j.ClaimedBy)
	}
	if j.Attempt != 1 {
		t.Fatalf("attempt: got %d, want 1", j.Attempt)
	}
	if n, _ := s.CountJobsByStatus(JobClaimed); n != 1 {
		t.Fatalf("claimed count: got %d, want 1", n)
	}
}

// TestQueue_NoJobs: Claim returns ErrNoJobs when the queue is empty.
func TestQueue_NoJobs(t *testing.T) {
	s := newQueueTestStore(t)
	_, err := s.Claim("W_A", time.Minute)
	if !errors.Is(err, ErrNoJobs) {
		t.Fatalf("expected ErrNoJobs, got %v", err)
	}
}

// TestQueue_ConcurrentClaim: two workers race on a single job. SQLite
// WAL serializes them; only one wins. The other gets ErrNoJobs.
func TestQueue_ConcurrentClaim(t *testing.T) {
	s := newQueueTestStore(t)
	if err := s.Enqueue("R1", "x", []byte("{}")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	var wg sync.WaitGroup
	var wins atomic.Int32
	var noJobs atomic.Int32
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.Claim("W"+string(rune('A'+i)), time.Minute); err == nil {
				wins.Add(1)
			} else if errors.Is(err, ErrNoJobs) {
				noJobs.Add(1)
			} else {
				t.Errorf("unexpected: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 || noJobs.Load() != 1 {
		t.Fatalf("expected 1 win + 1 no-jobs, got wins=%d noJobs=%d", wins.Load(), noJobs.Load())
	}
}

// TestQueue_AckDone: after Ack(success=true), the row is status=done
// and re-Claim returns nothing.
func TestQueue_AckDone(t *testing.T) {
	s := newQueueTestStore(t)
	_ = s.Enqueue("R1", "x", []byte("{}"))
	j, _ := s.Claim("W_A", time.Minute)

	if err := s.Ack(j.RunID, "W_A", true, ""); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if n, _ := s.CountJobsByStatus(JobDone); n != 1 {
		t.Fatalf("done count: %d", n)
	}
	if _, err := s.Claim("W_B", time.Minute); !errors.Is(err, ErrNoJobs) {
		t.Fatalf("after ack: expected ErrNoJobs, got %v", err)
	}
}

// TestQueue_AckFailed: Ack(success=false, errMsg) records the failure
// reason on the row and marks it failed (not pending — failure is
// terminal; retries are an explicit re-enqueue, not implicit).
func TestQueue_AckFailed(t *testing.T) {
	s := newQueueTestStore(t)
	_ = s.Enqueue("R1", "x", []byte("{}"))
	j, _ := s.Claim("W_A", time.Minute)

	if err := s.Ack(j.RunID, "W_A", false, "boom"); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if n, _ := s.CountJobsByStatus(JobFailedQ); n != 1 {
		t.Fatalf("failed count: %d", n)
	}
}

// TestQueue_AckWrongWorker: a worker can't ack a job it didn't claim.
// No error returned (idempotent path), but the row stays claimed.
func TestQueue_AckWrongWorker(t *testing.T) {
	s := newQueueTestStore(t)
	_ = s.Enqueue("R1", "x", []byte("{}"))
	_, _ = s.Claim("W_A", time.Minute)

	if err := s.Ack("R1", "W_B", true, ""); err != nil {
		t.Fatalf("Ack mismatched worker: %v", err)
	}
	if n, _ := s.CountJobsByStatus(JobClaimed); n != 1 {
		t.Fatalf("claimed should still be 1, got %d", n)
	}
}

// TestQueue_VisibilityTimeout: a worker dies (never acks). Time passes,
// the reaper moves the row back to pending. Another worker picks it up.
func TestQueue_VisibilityTimeout(t *testing.T) {
	s := newQueueTestStore(t)
	_ = s.Enqueue("R1", "x", []byte("{}"))

	// Claim with a tiny visibility so we don't have to wait minutes.
	j1, err := s.Claim("W_A", 10*time.Millisecond)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if j1.Attempt != 1 {
		t.Fatalf("attempt 1: got %d", j1.Attempt)
	}
	time.Sleep(20 * time.Millisecond)

	// Reaper sweep.
	moved, err := s.ReapExpired()
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if moved != 1 {
		t.Fatalf("expected 1 reaped, got %d", moved)
	}

	// Worker B picks it up. Attempt counter advances.
	j2, err := s.Claim("W_B", time.Minute)
	if err != nil {
		t.Fatalf("re-Claim: %v", err)
	}
	if j2.RunID != "R1" || j2.ClaimedBy != "W_B" {
		t.Fatalf("re-claim wrong: %+v", j2)
	}
	if j2.Attempt != 2 {
		t.Fatalf("attempt 2: got %d", j2.Attempt)
	}
}

// TestQueue_HeartbeatExtends: a long-running task heartbeats, the
// reaper does NOT take its job back.
func TestQueue_HeartbeatExtends(t *testing.T) {
	s := newQueueTestStore(t)
	_ = s.Enqueue("R1", "x", []byte("{}"))
	_, _ = s.Claim("W_A", 50*time.Millisecond)

	// Halfway through visibility, heartbeat to extend by 1 minute.
	time.Sleep(25 * time.Millisecond)
	if err := s.Heartbeat("R1", "W_A", time.Minute); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	// Wait past the original visibility deadline.
	time.Sleep(40 * time.Millisecond)
	moved, _ := s.ReapExpired()
	if moved != 0 {
		t.Fatalf("heartbeat didn't protect: reaped %d", moved)
	}
	// Original claim still owns it.
	if _, err := s.Claim("W_B", time.Minute); !errors.Is(err, ErrNoJobs) {
		t.Fatalf("W_B should not claim a heartbeated job, got %v", err)
	}
}

// TestQueue_HeartbeatLost: when the claim has lapsed, heartbeat fails
// so the worker knows it's been preempted.
func TestQueue_HeartbeatLost(t *testing.T) {
	s := newQueueTestStore(t)
	_ = s.Enqueue("R1", "x", []byte("{}"))
	_, _ = s.Claim("W_A", 5*time.Millisecond)
	time.Sleep(15 * time.Millisecond)
	_, _ = s.ReapExpired()

	err := s.Heartbeat("R1", "W_A", time.Minute)
	if err == nil {
		t.Fatalf("expected heartbeat to fail after preemption")
	}
}

// TestQueue_EnqueueIdempotent: re-enqueuing the same run_id is a no-op.
// The same job stays at attempt=1 with original status.
func TestQueue_EnqueueIdempotent(t *testing.T) {
	s := newQueueTestStore(t)
	_ = s.Enqueue("R1", "x", []byte(`{"v":1}`))
	_ = s.Enqueue("R1", "x", []byte(`{"v":2}`))
	if n, _ := s.CountJobsByStatus(JobPending); n != 1 {
		t.Fatalf("pending after re-enqueue: %d", n)
	}
	j, _ := s.Claim("W_A", time.Minute)
	if string(j.Payload) != `{"v":1}` {
		t.Fatalf("payload changed under re-enqueue: %s", j.Payload)
	}
}

// TestQueue_WaitForJob_Wakes: a long-poll waiter unblocks when
// Enqueue broadcasts.
func TestQueue_WaitForJob_Wakes(t *testing.T) {
	s := newQueueTestStore(t)
	woke := make(chan bool, 1)
	go func() {
		woke <- s.WaitForJob(context.Background(), 2*time.Second)
	}()
	time.Sleep(20 * time.Millisecond) // let the waiter park
	_ = s.Enqueue("R1", "x", []byte("{}"))
	select {
	case got := <-woke:
		if !got {
			t.Fatalf("WaitForJob: got false (timed out), wanted true")
		}
	case <-time.After(time.Second):
		t.Fatal("WaitForJob did not wake on Enqueue")
	}
}

// TestQueue_WaitForJob_Timeout: with no enqueue, the wait returns
// false after the deadline.
func TestQueue_WaitForJob_Timeout(t *testing.T) {
	s := newQueueTestStore(t)
	got := s.WaitForJob(context.Background(), 30*time.Millisecond)
	if got {
		t.Fatalf("expected timeout to return false")
	}
}

// TestQueue_WaitForJob_CtxCancel: cancellation also unblocks the wait.
func TestQueue_WaitForJob_CtxCancel(t *testing.T) {
	s := newQueueTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	woke := make(chan bool, 1)
	go func() {
		woke <- s.WaitForJob(ctx, time.Hour)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case got := <-woke:
		if got {
			t.Fatalf("expected ctx cancel false return")
		}
	case <-time.After(time.Second):
		t.Fatal("WaitForJob did not unblock on ctx cancel")
	}
}
