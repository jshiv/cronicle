package cronicle

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// captureSlog routes slog records into a buffer for the duration of
// fn, then restores the previous default handler. Returns whatever the
// handler captured.
func captureSlog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	fn()
	_ = context.Background() // touch import for future use
	return buf.String()
}

// TestMCPStderrSlogWriter_EmitsOneRecordPerLine is the M14 invariant.
// Writing 3 newline-terminated lines must produce 3 slog records with
// entry_type=mcp_stderr and mcp_server set to the server name.
func TestMCPStderrSlogWriter_EmitsOneRecordPerLine(t *testing.T) {
	out := captureSlog(t, func() {
		w := newMCPStderrSlogWriter("my-mcp")
		w.Write([]byte("line one\nline two\nline three\n"))
	})

	for _, want := range []string{"line one", "line two", "line three"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected line %q in slog output; got:\n%s", want, out)
		}
	}
	// Server name must be tagged on every record.
	if strings.Count(out, `mcp_server=my-mcp`) != 3 {
		t.Errorf("expected mcp_server tag on each of 3 records; got:\n%s", out)
	}
}

// TestMCPStderrSlogWriter_HandlesPartialLines: bytes that don't end
// with '\n' must wait in the buffer until the newline arrives, then
// emit. No fragmenting of a single line into multiple records.
func TestMCPStderrSlogWriter_HandlesPartialLines(t *testing.T) {
	out := captureSlog(t, func() {
		w := newMCPStderrSlogWriter("partial")
		w.Write([]byte("hello "))
		w.Write([]byte("world\n"))
	})
	if !strings.Contains(out, "hello world") {
		t.Errorf("expected merged line in slog output; got:\n%s", out)
	}
	// Reject the bug shape: two separate records for one line.
	if strings.Count(out, "msg=") > 1 {
		t.Errorf("multiple slog records for one logical line; got:\n%s", out)
	}
}

// TestMCPStderrSlogWriter_TrimsCarriageReturn: progress bars or
// Windows-style line endings shouldn't leave \r in the captured msg.
func TestMCPStderrSlogWriter_TrimsCarriageReturn(t *testing.T) {
	out := captureSlog(t, func() {
		w := newMCPStderrSlogWriter("cr")
		w.Write([]byte("with cr\r\n"))
	})
	if strings.Contains(out, "\r") {
		t.Errorf("carriage return leaked into slog output: %q", out)
	}
}

// TestMCPStderrSlogWriter_BoundedBufferOnNoNewline: no-newline data
// past the cap must force-emit and reset, mirroring lineEmitter's
// M13 bound.
func TestMCPStderrSlogWriter_BoundedBufferOnNoNewline(t *testing.T) {
	w := newMCPStderrSlogWriter("bound")
	huge := bytes.Repeat([]byte("x"), mcpStderrMaxBufBytes+1024)
	w.Write(huge)
	w.mu.Lock()
	defer w.mu.Unlock()
	if got := len(w.buf); got > mcpStderrMaxBufBytes {
		t.Errorf("buf grew past cap; got %d bytes, cap %d", got, mcpStderrMaxBufBytes)
	}
}
