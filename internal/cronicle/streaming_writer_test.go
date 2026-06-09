package cronicle

import (
	"sync"
	"testing"
)

// TestStreamingWriter_ConcurrentClose_Leader is the M12 race
// regression: a leader's Close had to release streamLock exactly once.
// The previous unguarded `closed` field meant two concurrent Close
// calls could both pass the check and both call streamLock.Unlock,
// which panics on the second invocation.
//
// sync.Once now guards the cleanup. We can't reliably reproduce a
// panic in a single test run, but we CAN drive many concurrent Close
// calls and assert none panic — a regression would surface as a
// "sync: unlock of unlocked mutex" panic on at least one goroutine.
func TestStreamingWriter_ConcurrentClose_Leader(t *testing.T) {
	sw := NewStreamingWriter()
	if !sw.leader {
		// Only the first writer wins streamLock in a fresh process. If
		// another test left it held, skip — this test runs in isolation
		// in normal CI.
		t.Skip("streamLock held by a prior test; skipping leader-Close test")
	}

	const concurrentCloses = 50
	var wg sync.WaitGroup
	for i := 0; i < concurrentCloses; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Close panicked: %v", r)
				}
			}()
			sw.Close()
		}()
	}
	wg.Wait()
}

// TestStreamingWriter_CloseIsIdempotent: sequential repeated Close
// calls must succeed. Documents the intended contract.
func TestStreamingWriter_CloseIsIdempotent(t *testing.T) {
	sw := NewStreamingWriter()
	if !sw.leader {
		t.Skip("streamLock held by a prior test; skipping idempotency test")
	}
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("second Close panicked: %v", r)
		}
	}()
	sw.Close()
	sw.Close()
	sw.Close()
}
