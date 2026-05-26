package cronicle

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// scratchRepo creates a temporary git repository with two commits and an
// uncommitted change. Returns the workspace path. Used to exercise GitTool
// against a known shape without needing network access or a clone.
func scratchRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	commit := func(name, content, msg string) {
		full := filepath.Join(dir, name)
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := wt.Add(name); err != nil {
			t.Fatalf("add: %v", err)
		}
		_, err := wt.Commit(msg, &git.CommitOptions{
			Author: &object.Signature{
				Name:  "test",
				Email: "test@example.com",
				When:  time.Now(),
			},
		})
		if err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	commit("hello.txt", "first version\n", "Add hello.txt")
	commit("hello.txt", "second version\n", "Update hello.txt")

	// Leave one uncommitted change for status/diff tests.
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("third version\n"), 0o644); err != nil {
		t.Fatalf("dirty: %v", err)
	}
	return dir
}

// status reports modified files when there are uncommitted changes; clean
// when there aren't.
func TestGitToolStatus(t *testing.T) {
	ws := scratchRepo(t)
	tool := &GitTool{Workspace: ws}

	out, isErr := tool.Execute(context.Background(),
		json.RawMessage(`{"command":"status"}`))
	if isErr {
		t.Fatalf("status: %s", out)
	}
	if !strings.Contains(out, "hello.txt") {
		t.Fatalf("status missing changed file: %q", out)
	}
}

// log returns recent commits in oneline format. Default limit picks both
// commits we made in scratchRepo.
func TestGitToolLog(t *testing.T) {
	ws := scratchRepo(t)
	tool := &GitTool{Workspace: ws}

	out, isErr := tool.Execute(context.Background(),
		json.RawMessage(`{"command":"log","args":{"limit":5}}`))
	if isErr {
		t.Fatalf("log: %s", out)
	}
	if !strings.Contains(out, "Update hello.txt") || !strings.Contains(out, "Add hello.txt") {
		t.Fatalf("log missing commits: %q", out)
	}
}

// diff with from=HEAD~1 to=HEAD returns a unified diff containing the
// changed file's path. Real go-git Patch output starts with "diff --git".
func TestGitToolDiffRefs(t *testing.T) {
	ws := scratchRepo(t)
	tool := &GitTool{Workspace: ws}

	out, isErr := tool.Execute(context.Background(),
		json.RawMessage(`{"command":"diff","args":{"from":"HEAD~1","to":"HEAD"}}`))
	if isErr {
		t.Fatalf("diff: %s", out)
	}
	if !strings.Contains(out, "diff --git") {
		t.Fatalf("diff missing patch header: %q", out)
	}
	if !strings.Contains(out, "hello.txt") {
		t.Fatalf("diff missing file: %q", out)
	}
}

// branch creates and switches; subsequent status reports the same dirty
// state on the new branch.
func TestGitToolBranchAndCommit(t *testing.T) {
	ws := scratchRepo(t)
	tool := &GitTool{Workspace: ws}

	out, isErr := tool.Execute(context.Background(),
		json.RawMessage(`{"command":"branch","args":{"name":"feature/x"}}`))
	if isErr {
		t.Fatalf("branch: %s", out)
	}
	if !strings.Contains(out, "feature/x") {
		t.Fatalf("branch result: %q", out)
	}

	// Re-create branch should fail.
	_, isErr = tool.Execute(context.Background(),
		json.RawMessage(`{"command":"branch","args":{"name":"feature/x"}}`))
	if !isErr {
		t.Fatalf("duplicate branch should error")
	}

	// commit picks up the dirty file from scratchRepo.
	out, isErr = tool.Execute(context.Background(),
		json.RawMessage(`{"command":"commit","args":{"message":"Bump hello.txt to v3"}}`))
	if isErr {
		t.Fatalf("commit: %s", out)
	}
	if !strings.Contains(out, "Committed") {
		t.Fatalf("commit result: %q", out)
	}

	// commit on a clean tree returns the explicit nothing-to-commit error.
	_, isErr = tool.Execute(context.Background(),
		json.RawMessage(`{"command":"commit","args":{"message":"empty"}}`))
	if !isErr {
		t.Fatalf("empty commit should error")
	}
}

// Unknown subcommand and missing required args fail safely.
func TestGitToolErrors(t *testing.T) {
	ws := scratchRepo(t)
	tool := &GitTool{Workspace: ws}

	_, isErr := tool.Execute(context.Background(),
		json.RawMessage(`{"command":"frobnicate"}`))
	if !isErr {
		t.Fatalf("unknown command should error")
	}

	_, isErr = tool.Execute(context.Background(),
		json.RawMessage(`{"command":"branch","args":{}}`))
	if !isErr {
		t.Fatalf("branch without name should error")
	}

	_, isErr = tool.Execute(context.Background(),
		json.RawMessage(`{"command":""}`))
	if !isErr {
		t.Fatalf("empty command should error")
	}

	// Not a git repo at all.
	tool2 := &GitTool{Workspace: t.TempDir()}
	_, isErr = tool2.Execute(context.Background(),
		json.RawMessage(`{"command":"status"}`))
	if !isErr {
		t.Fatalf("non-repo should error")
	}
}
