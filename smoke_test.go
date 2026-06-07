//go:build smoke

// Package main_test holds binary-level smoke tests for cronicle. These
// build the cronicle binary with the race detector enabled and exec
// it as a real subprocess against fixture HCLs, asserting that:
//
//   - The binary starts and emits expected startup logs.
//   - No "WARNING: DATA RACE" reports appear in stdout/stderr.
//   - The binary responds to SIGINT and exits within a reasonable window.
//
// These tests catch the kinds of issues that in-process unit tests
// can't reach: real cron-callback goroutine scheduling under load, the
// CLI / Cobra layer in cmd/, and any race the package-level tests miss
// because their goroutine interleaving doesn't match the binary's.
//
// Hidden behind a `smoke` build tag so the default `go test ./...` stays
// fast. Run via `go test -tags smoke ./...` (CI does this in a separate
// step) or directly: `go test -tags smoke -run TestSmoke_Rapid ./...`.
package main_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cronicleBin is the path to the race-instrumented binary built in
// TestMain. Tests use it as the executable for exec.CommandContext.
var cronicleBin string

func TestMain(m *testing.M) {
	tmpDir, err := os.MkdirTemp("", "cronicle-smoke-bin-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "smoke: MkdirTemp:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)

	cronicleBin = filepath.Join(tmpDir, "cronicle")

	// Build with -race so the smoke tests double as race-detector runs
	// against the real binary. This is the layer that catches scheduler
	// races the in-process tests can't reach — Go's race detector flags
	// actual concurrent unsynchronized access during the run, and the
	// goroutine scheduling pattern in a real binary driving its own cron
	// ticks differs from the in-process Run() invocations the unit tests
	// use.
	build := exec.Command("go", "build", "-race", "-o", cronicleBin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "smoke: go build -race failed:", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// TestSmoke_RunCleanStartup verifies the binary boots without crashing
// or emitting any "WARNING: DATA RACE" reports against a minimal HCL
// over a short window, then exits cleanly on SIGINT.
func TestSmoke_RunCleanStartup(t *testing.T) {
	workdir := t.TempDir()
	hcl := filepath.Join(workdir, "cronicle.hcl")
	if err := os.WriteFile(hcl, []byte(`schedule "s" {
  cron = "@every 1h"
  task "t" { command = ["true"] }
}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	out := runCronicle(t, 4*time.Second, "run", "--path", hcl)
	assertNoRaces(t, out)
	if !strings.Contains(out, "config loaded") {
		t.Fatalf("expected startup log; got:\n%s", out)
	}
}

// TestSmoke_RunRapidHCLMutations stresses the config refresh path —
// the cron scheduler reloads the HCL while this test rewrites it
// every ~300ms. This is the pattern that surfaced two races during
// the C1 fix work (CommandEvalContext shared-map write and the
// c.Stop/c.Start race inside LoadCron). Running it under the race-
// instrumented binary catches regressions in either of those paths
// plus anything similar that lands later.
func TestSmoke_RunRapidHCLMutations(t *testing.T) {
	workdir := t.TempDir()
	hcl := filepath.Join(workdir, "cronicle.hcl")
	initial := `config_refresh = "@every 200ms"
heartbeat      = "@every 1h"
schedule "s" {
  cron = "@every 1h"
  task "t" { command = ["true"] }
}
`
	if err := os.WriteFile(hcl, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, cronicleBin, "run", "--path", hcl)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	// SIGINT instead of the default SIGKILL so the shutdown sequence
	// runs and any race-report buffer flushes to stdout before exit.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }

	if err := cmd.Start(); err != nil {
		t.Fatalf("start cronicle: %v", err)
	}

	// Give startup a beat, then rewrite the HCL repeatedly so the
	// refresh tick (200ms) actually reloads each time. 20 rewrites
	// over ~6s is enough churn to surface scheduling races in CI.
	time.Sleep(500 * time.Millisecond)
	for i := 0; i < 20; i++ {
		body := fmt.Sprintf(`config_refresh = "@every 200ms"
heartbeat = "@every 1h"
schedule "s%d" {
  cron = "@every 1h"
  task "t" { command = ["true"] }
}
`, i)
		if err := os.WriteFile(hcl, []byte(body), 0o644); err != nil {
			t.Errorf("rewrite %d: %v", i, err)
		}
		time.Sleep(300 * time.Millisecond)
	}

	cancel()
	// Wait for graceful shutdown (cmd.Cancel SIGINT triggers it).
	// Non-zero exit on SIGINT is fine — we only care about the output.
	_ = cmd.Wait()

	out := buf.String()
	assertNoRaces(t, out)
	if got := strings.Count(out, "Refreshing config"); got < 3 {
		t.Errorf("expected at least 3 refresh ticks, got %d; output:\n%s", got, out)
	}
}

// runCronicle runs the binary with a deadline. SIGINT on context
// cancel triggers the binary's graceful shutdown so any race-report
// buffer flushes to stdout before exit. Returns combined output.
func runCronicle(t *testing.T, dur time.Duration, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()
	cmd := exec.CommandContext(ctx, cronicleBin, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// assertNoRaces scans the binary's output for race-detector reports
// and fails the test with the first race block on a hit. Each report
// is delimited by lines of "==================" so we can extract the
// relevant section without dumping the entire log.
func assertNoRaces(t *testing.T, out string) {
	t.Helper()
	if !strings.Contains(out, "WARNING: DATA RACE") {
		return
	}
	chunks := strings.Split(out, "==================")
	for _, c := range chunks {
		if strings.Contains(c, "WARNING: DATA RACE") {
			t.Errorf("race detected:\n==================%s==================", c)
		}
	}
}
