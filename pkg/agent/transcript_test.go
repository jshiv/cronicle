package agent

import (
	"testing"
	"time"
)

// TestOpenTranscript_FileMode0600 locks in the M5 fix: the per-run
// agent transcript on disk contains the full conversation (prompts,
// system prompt, tool inputs, model outputs, accounting) which can
// include secrets the agent surfaced from tool_results. The file must
// be created with 0o600 so even a permissive umask on a shared host
// can't widen it.
func TestOpenTranscript_FileMode0600(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		TranscriptDir: dir,
		RunID:         "test-run",
	}
	tw, err := openTranscript(cfg, "claude-opus-4-7", time.Now())
	if err != nil {
		t.Fatalf("openTranscript: %v", err)
	}
	defer tw.f.Close()

	info, err := tw.f.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Mask off file-type bits; only permission bits matter for this check.
	perm := info.Mode().Perm()
	if perm != 0o600 {
		t.Errorf("transcript perms: got %#o, want 0o600 (other users must not read agent conversation)", perm)
	}
}
