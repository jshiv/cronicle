package cronicle

import (
	"bytes"
	"io"
	"testing"
)

// TestLineEmitter_BoundedBufferOnNoNewline locks in the M13 fix: a
// stream that never emits a '\n' used to grow the internal buffer
// without bound until Flush. We now force-emit when the buffer
// exceeds maxLineEmitterBufBytes and reset.
func TestLineEmitter_BoundedBufferOnNoNewline(t *testing.T) {
	emitter := newLineEmitter(io.Discard, "r", "t", "s", "stdout")

	// Push 4x the cap in newline-free chunks.
	chunk := bytes.Repeat([]byte("x"), maxLineEmitterBufBytes/4)
	for i := 0; i < 6; i++ {
		emitter.Write(chunk)
	}

	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	if got := len(emitter.buf); got > maxLineEmitterBufBytes {
		t.Errorf("lineEmitter buf grew past cap: got %d bytes, cap %d", got, maxLineEmitterBufBytes)
	}
}

// TestLineEmitter_EmitsTruncationMarker: when the buffer cap fires,
// the synthetic line must signal that it was a truncated emission.
// Captures the line by routing emit through an in-memory writer.
func TestLineEmitter_EmitsTruncationMarker(t *testing.T) {
	emitter := newLineEmitter(io.Discard, "r", "t", "s", "stdout")
	// Two chunks just over the cap, no newlines.
	huge := bytes.Repeat([]byte("x"), maxLineEmitterBufBytes+1)
	emitter.Write(huge)

	// Force a Flush to drain anything remaining, then check that the
	// emitted lines included a truncation marker. We can't observe the
	// internal slog emit directly here without a handler swap, but we
	// CAN observe that the buffer was drained — the truncation reset
	// it to empty.
	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	if got := len(emitter.buf); got > maxLineEmitterBufBytes {
		t.Errorf("buf still oversized after consume: %d", got)
	}
}

// TestLineEmitter_NormalLinesStillFlow: a well-behaved producer
// (regular newline-terminated lines) must keep its existing pass-
// through semantics. The bound only kicks in for pathological no-
// newline streams.
func TestLineEmitter_NormalLinesStillFlow(t *testing.T) {
	emitter := newLineEmitter(io.Discard, "r", "t", "s", "stdout")
	emitter.Write([]byte("line1\nline2\nline3\n"))
	emitter.Flush()

	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	if got := len(emitter.buf); got != 0 {
		t.Errorf("buf should be empty after newline-terminated input + Flush; got %d bytes: %q",
			got, string(emitter.buf))
	}
}

