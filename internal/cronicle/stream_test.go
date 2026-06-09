package cronicle

import (
	"strings"
	"testing"
)

// TestRedactToolInput_ShortPassesThrough: inputs at or below the
// threshold must be untouched so normal tool calls (text_editor paths,
// short bash commands) keep full slog fidelity.
func TestRedactToolInput_ShortPassesThrough(t *testing.T) {
	in := `{"command":"ls -la"}`
	if got := redactToolInput(in); got != in {
		t.Errorf("short input mutated: got %q, want %q", got, in)
	}
}

// TestRedactToolInput_BoundaryExact: an input exactly at the threshold
// must still pass through unchanged — off-by-one error would surface
// here.
func TestRedactToolInput_BoundaryExact(t *testing.T) {
	in := strings.Repeat("a", maxLoggedToolInputBytes)
	if got := redactToolInput(in); got != in {
		t.Errorf("threshold-length input mutated; len(got)=%d", len(got))
	}
}

// TestRedactToolInput_LongTruncated: oversized inputs must lose their
// tail to the truncation marker, and the marker must report the
// dropped byte count so operators can identify what's missing.
func TestRedactToolInput_LongTruncated(t *testing.T) {
	const padBytes = 1000
	in := strings.Repeat("h", maxLoggedToolInputBytes) + strings.Repeat("t", padBytes)
	got := redactToolInput(in)

	if len(got) == len(in) {
		t.Fatalf("oversized input passed through unchanged")
	}
	// Head must be intact — operators rely on it to recognize the call.
	wantHead := strings.Repeat("h", maxLoggedToolInputBytes)
	if !strings.HasPrefix(got, wantHead) {
		t.Errorf("truncated output missing head; got prefix %q", got[:50])
	}
	// Marker must mention truncation + the byte count.
	if !strings.Contains(got, "truncated") {
		t.Errorf("truncation marker missing: %q", got)
	}
	if !strings.Contains(got, "1000") {
		t.Errorf("truncation marker should report dropped count (1000); got: %q", got)
	}
	// The tail (which would have carried a secret in the leak scenario)
	// must NOT appear in the output. Sample 50 tail bytes.
	tailSample := strings.Repeat("t", 50)
	if strings.Contains(got, tailSample) {
		t.Errorf("tail bytes leaked through truncation: %q", got)
	}
}

// TestRedactToolInput_EmptyAndNil: defensive cases — empty and
// zero-length inputs must produce empty output without panicking.
func TestRedactToolInput_EmptyAndNil(t *testing.T) {
	if got := redactToolInput(""); got != "" {
		t.Errorf("empty input: got %q, want \"\"", got)
	}
}
