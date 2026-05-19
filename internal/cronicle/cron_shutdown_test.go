package cronicle

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRun_ShutdownOnCtxCancel: Run blocks until ctx is canceled and
// then exits within a small grace period. Regression for the SIGINT
// hang where Run's wg.Wait() blocked forever and only `kill -9` worked.
//
// Calls SetupLogging first to install a TextHandler — otherwise
// slog.Default() is still the stdlib bridge handler which routes
// through log.Default(), and when EnableStateStore wraps slog.Default's
// handler, the log → slog handlerWriter cycle deadlocks on log's
// output mutex (the binary path doesn't hit this because `cronicle run`
// installs the TextHandler before Run via SetupLogging).
func TestRun_ShutdownOnCtxCancel(t *testing.T) {
	SetupLogging(LogFormatText)
	dir := t.TempDir()
	hcl := filepath.Join(dir, "cronicle.hcl")
	// Cron set far enough out that the test doesn't accidentally fire
	// a task during the shutdown window — we want to measure the
	// pure shutdown latency, not the time to drain in-flight work.
	body := `schedule "smoke" {
  cron = "@every 1h"
  task "noop" { command = ["true"] }
}
`
	if err := os.WriteFile(hcl, []byte(body), 0o644); err != nil {
		t.Fatalf("write hcl: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		// Run with the listener disabled — keeps the test off any
		// network ports. The shutdown sequence still runs:
		// startListener gets a closed `done` immediately, the cron
		// loop stops on ctx cancel, the in-memory queue closes, the
		// state store closes.
		Run(ctx, hcl, RunOptions{})
		close(done)
	}()

	// Give the scheduler a moment to spin up so we're not racing the
	// initial config load.
	time.Sleep(200 * time.Millisecond)
	cancel()

	start := time.Now()
	select {
	case <-done:
		t.Logf("Run returned %v after ctx cancel", time.Since(start))
	case <-time.After(45 * time.Second):
		t.Fatal("Run did not return within 45s of ctx cancel — shutdown is hung")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("Run took too long to shut down (%v); ought to be sub-second", time.Since(start))
	}
}
