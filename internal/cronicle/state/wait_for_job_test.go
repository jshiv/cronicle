package state

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestWaitForJob_NoGoroutineLeakOnTimeout is the M8 regression: the
// previous Cond-based implementation spawned a goroutine per call
// that could be permanently stranded if Broadcast fired before the
// goroutine reached Wait. The chan-close broadcast pattern eliminates
// that spawn entirely. Verify by snapshotting goroutine counts before
// and after a batch of timed-out waits.
func TestWaitForJob_NoGoroutineLeakOnTimeout(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.db.Close()

	// Warm up the lazy waiters() init so the first call isn't an outlier.
	_ = s.WaitForJob(context.Background(), time.Millisecond)
	// Settle any background goroutines from setup before snapshotting.
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	before := runtime.NumGoroutine()

	const N = 100
	for i := 0; i < N; i++ {
		if got := s.WaitForJob(context.Background(), 5*time.Millisecond); got != false {
			t.Fatalf("expected timeout (false), got true")
		}
	}

	// Give any short-lived goroutines a moment to die before measuring.
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()

	// Allow a small slop for any incidental scheduler / GC goroutines.
	// The old leak grew 1:1 with N=100, so any value below ~10 means the
	// leak is gone.
	if after-before > 5 {
		t.Errorf("goroutine count grew by %d after %d WaitForJob timeouts (before=%d, after=%d)",
			after-before, N, before, after)
	}
}

// TestWaitForJob_WokenByEnqueue: the channel-broadcast must propagate
// the wakeup to every waiter the same way the old Cond.Broadcast did.
func TestWaitForJob_WokenByEnqueue(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.db.Close()

	const N = 5
	var wg sync.WaitGroup
	woken := make(chan bool, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			woken <- s.WaitForJob(context.Background(), 2*time.Second)
		}()
	}

	// Give waiters a beat to register on the current channel snapshot.
	time.Sleep(50 * time.Millisecond)
	if err := s.Enqueue("r1", "sched", []byte(`{}`)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("waiters did not wake within 3s of enqueue")
	}

	close(woken)
	wakeups := 0
	for w := range woken {
		if w {
			wakeups++
		}
	}
	if wakeups != N {
		t.Errorf("expected all %d waiters to receive the wakeup, got %d", N, wakeups)
	}
}
