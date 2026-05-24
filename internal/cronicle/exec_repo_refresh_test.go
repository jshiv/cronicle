package cronicle_test

// Worker-side stale-checkout regression test.
//
// Before exec.go's taskPathIsCroniclePathWithGit branch added a
// Checkout step, the worker's first task exec would clone the repo
// fresh, but subsequent execs would short-circuit to "git open" with
// no fetch. Any push to the schedule's repo wouldn't reach the
// worker until the pod restarted. This test pins the fixed behavior:
// the SECOND exec must see the SECOND commit's content.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle"
)

// initSourceRepo creates a bare git repo with one file at `name` and
// the given content. Returns the bare repo's filesystem path (which
// is a valid file:// URL for go-git).
func initSourceRepo(t *testing.T, fileName, content string) string {
	t.Helper()
	tmp := t.TempDir()
	bareDir := filepath.Join(tmp, "src.git")
	wt := filepath.Join(tmp, "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		// Pin identity + skip global config so the test doesn't depend
		// on the developer's git config / signing setup.
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t",
			"GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t",
			"GIT_COMMITTER_EMAIL=t@e",
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}

	run(tmp, "init", "--bare", "--initial-branch=main", bareDir)
	run(wt, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(wt, fileName), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", fileName, err)
	}
	run(wt, "add", "-A")
	run(wt, "commit", "-m", "v1")
	run(wt, "remote", "add", "origin", bareDir)
	run(wt, "push", "origin", "main")

	return bareDir
}

// pushNewCommit overwrites `name` with `content` in a fresh clone of
// `bareDir`, commits, and pushes — simulating a user pushing a new
// version of the repo while the worker has an existing checkout.
func pushNewCommit(t *testing.T, bareDir, fileName, content string) {
	t.Helper()
	tmp := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = tmp
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	run("clone", bareDir, ".")
	if err := os.WriteFile(filepath.Join(tmp, fileName), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", fileName, err)
	}
	run("add", "-A")
	run("commit", "-m", "v2")
	run("push", "origin", "main")
}

// TestWorkerExec_ScheduleRepoFetchesOnEveryRun is the regression
// test. It mirrors the worker's task-exec flow:
//
//  1. Set up a bare source repo with version=v1
//  2. Build a Task that points at the bare via task.CronicleRepo,
//     with task.Path == task.CroniclePath (the "schedule-level repo"
//     case — the branch the original code didn't fetch on)
//  3. Execute task once → expect output "v1"
//  4. Push a NEW commit to the source repo (version=v2)
//  5. Execute the same task again → expect output "v2"
//
// Before the fix, step 5 returns "v1" because Clone() short-circuits
// to "git open" with no fetch, leaving the worker on the v1 commit.
func TestWorkerExec_ScheduleRepoFetchesOnEveryRun(t *testing.T) {
	srcRepo := initSourceRepo(t, "version.txt", "v1\n")
	workerWorkdir := t.TempDir()

	cronicleRepo := &cronicle.Repo{URL: srcRepo, Branch: "main"}

	t.Run("first exec gets v1", func(t *testing.T) {
		// task.CroniclePath = task.Path so taskPathIsCroniclePathWithGit
		// triggers the schedule-level repo code path (NOT the per-task
		// repo path, which already had Checkout).
		task := cronicle.Task{
			Name:         "read-version",
			Command:      []string{"cat", "version.txt"},
			Path:         workerWorkdir,
			CroniclePath: workerWorkdir,
			CronicleRepo: cronicleRepo,
			ScheduleName: "test-schedule",
		}
		r, err := task.Execute(time.Now())
		if err != nil {
			t.Fatalf("Execute v1: %v", err)
		}
		if got := r.Stdout; got != "v1\n" {
			t.Fatalf("first exec: got %q, want %q", got, "v1\n")
		}
	})

	// Simulate a push between runs.
	pushNewCommit(t, srcRepo, "version.txt", "v2\n")

	t.Run("second exec sees v2 (the fix)", func(t *testing.T) {
		task := cronicle.Task{
			Name:         "read-version",
			Command:      []string{"cat", "version.txt"},
			Path:         workerWorkdir,
			CroniclePath: workerWorkdir,
			CronicleRepo: cronicleRepo,
			ScheduleName: "test-schedule",
		}
		r, err := task.Execute(time.Now())
		if err != nil {
			t.Fatalf("Execute v2: %v", err)
		}
		got := r.Stdout
		if got == "v1\n" {
			t.Fatalf("stale-checkout regression: still seeing v1 after push to v2")
		}
		if got != "v2\n" {
			t.Fatalf("second exec: got %q, want %q", got, "v2\n")
		}
	})

	_ = fmt.Sprintf // silence import on potential future use
}
