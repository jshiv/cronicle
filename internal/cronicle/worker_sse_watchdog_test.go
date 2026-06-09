package cronicle

import (
	"bytes"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

// closeCountReadCloser is a ReadCloser whose Close call increments a
// counter so tests can verify that the watchdog closed the underlying
// body after idling out.
type closeCountReadCloser struct {
	r      io.Reader
	closes atomic.Int32
}

func (c *closeCountReadCloser) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c *closeCountReadCloser) Close() error {
	c.closes.Add(1)
	return nil
}

// blockingReader Read always blocks until ch is closed. Simulates a
// TCP connection that's stuck on a read that never returns — the
// failure mode the watchdog is supposed to recover from.
type blockingReader struct{ ch chan struct{} }

func (b *blockingReader) Read(p []byte) (int, error) {
	<-b.ch
	return 0, io.EOF
}

// TestSSEWatchdog_ClosesBodyOnIdle is the M11 invariant: when no bytes
// arrive within the watchdog's timeout, it must close the underlying
// body so the SSE consumer's bufio.Scanner unblocks (Scan returns false
// after a body close) and the outer reconnect loop fires.
func TestSSEWatchdog_ClosesBodyOnIdle(t *testing.T) {
	block := make(chan struct{})
	defer close(block) // let any lingering Read return after the test

	inner := &closeCountReadCloser{r: &blockingReader{ch: block}}
	wd := newSSEWatchdog(inner, 50*time.Millisecond)
	defer wd.stop()

	// The watchdog timer should fire within ~50ms and close inner.
	time.Sleep(150 * time.Millisecond)

	if got := inner.closes.Load(); got != 1 {
		t.Errorf("watchdog must close idle body exactly once; got %d closes", got)
	}
}

// TestSSEWatchdog_HealthyStreamSurvives: when bytes (including
// heartbeats) arrive within each timeout window, the watchdog must NOT
// close the body. Reads should keep extending the deadline.
func TestSSEWatchdog_HealthyStreamSurvives(t *testing.T) {
	// 100ms of heartbeats at 30ms intervals, then EOF. The watchdog
	// timeout (60ms) is longer than the interval so a healthy stream
	// always refreshes the deadline before it expires.
	body := bytes.NewBufferString("event: ping\n\n")
	inner := &closeCountReadCloser{r: body}
	wd := newSSEWatchdog(inner, 60*time.Millisecond)
	defer wd.stop()

	// Read once — that's enough to reset the timer in the watchdog.
	buf := make([]byte, 32)
	if _, err := wd.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}

	// Sleep less than the timeout. Watchdog must NOT have fired yet.
	time.Sleep(40 * time.Millisecond)
	if got := inner.closes.Load(); got != 0 {
		t.Errorf("watchdog closed body during active read window; closes=%d", got)
	}
}

// TestSSEWatchdog_StopIsIdempotent: stop() may be called multiple
// times (e.g., defer plus explicit cleanup) without panicking.
func TestSSEWatchdog_StopIsIdempotent(t *testing.T) {
	inner := &closeCountReadCloser{r: bytes.NewBufferString("")}
	wd := newSSEWatchdog(inner, time.Second)
	wd.stop()
	wd.stop() // must not panic
}
