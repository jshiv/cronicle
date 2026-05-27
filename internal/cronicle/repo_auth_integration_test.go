package cronicle_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle"
	"github.com/jshiv/cronicle/internal/cronicle/secretstore"
	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// setupSecretStoreForTest creates an in-memory secret store with the
// given secrets and installs it as the default. Returns a cleanup
// function that restores the previous default.
func setupSecretStoreForTest(t *testing.T, secrets map[string]string) func() {
	t.Helper()
	db, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	for k, v := range secrets {
		if err := db.PutSecret(k, v, "test"); err != nil {
			t.Fatalf("PutSecret(%s): %v", k, err)
		}
	}
	store := secretstore.New().SetBackend(db).StaleTTL(0)
	if err := store.Start(context.Background(), nil, 0); err != nil {
		t.Fatalf("store.Start: %v", err)
	}
	prev := secretstore.SetDefault(store)
	return func() {
		secretstore.SetDefault(prev)
		db.Close()
	}
}

// TestRepoAuth_TaskLevel_PasswordFromSecret verifies that a task-level
// repo block with password = "$secret.GIT_TOKEN" clones successfully
// at execution time (not at Init time).
func TestRepoAuth_TaskLevel_PasswordFromSecret(t *testing.T) {
	gitRoot := t.TempDir()
	initBareRepo(t, gitRoot, "private-repo", "main")

	token := "test-token-123"
	serverURL, cleanup := startTestHTTPGitServer(t, gitRoot, "x", token)
	defer cleanup()

	secretCleanup := setupSecretStoreForTest(t, map[string]string{
		"GIT_TOKEN": token,
	})
	defer secretCleanup()

	// Build a task with a repo block using $secret.GIT_TOKEN
	cloneDir := filepath.Join(t.TempDir(), "work")
	os.MkdirAll(cloneDir, 0755)

	task := cronicle.Task{
		Name:    "test-task",
		Command: []string{"/bin/echo", "ok"},
		Path:    cloneDir,
		Repo: &cronicle.Repo{
			URL:      serverURL + "/private-repo.git",
			Password: "$secret.GIT_TOKEN",
			Branch:   "main",
		},
	}

	// Execute should clone the repo (resolving the secret) and run the command
	result, err := task.Execute(time.Now())
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result.ExitStatus != 0 {
		t.Fatalf("expected exit 0, got %d: %s", result.ExitStatus, result.Stderr)
	}

	// Verify the repo was cloned
	if !cronicle.DirExists(filepath.Join(cloneDir, ".git")) {
		t.Fatal(".git directory not created — clone didn't happen")
	}
	if _, err := os.Stat(filepath.Join(cloneDir, "hello.txt")); err != nil {
		t.Fatalf("hello.txt not found in clone: %v", err)
	}
}

// TestRepoAuth_TaskLevel_PasswordFromEnv verifies that ${env.GIT_TOKEN}
// works in a task repo block (resolved at HCL parse time).
func TestRepoAuth_TaskLevel_PasswordFromEnv(t *testing.T) {
	gitRoot := t.TempDir()
	initBareRepo(t, gitRoot, "env-repo", "main")

	token := "env-token-456"
	serverURL, cleanup := startTestHTTPGitServer(t, gitRoot, "x", token)
	defer cleanup()

	cloneDir := filepath.Join(t.TempDir(), "work")
	os.MkdirAll(cloneDir, 0755)

	// Simulate ${env.GIT_TOKEN} already resolved at parse time
	task := cronicle.Task{
		Name:    "test-task",
		Command: []string{"/bin/echo", "ok"},
		Path:    cloneDir,
		Repo: &cronicle.Repo{
			URL:      serverURL + "/env-repo.git",
			Password: token, // already resolved by HCL eval
			Branch:   "main",
		},
	}

	result, err := task.Execute(time.Now())
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result.ExitStatus != 0 {
		t.Fatalf("expected exit 0, got %d: %s", result.ExitStatus, result.Stderr)
	}

	if !cronicle.DirExists(filepath.Join(cloneDir, ".git")) {
		t.Fatal(".git directory not created")
	}
}

// TestRepoAuth_TaskLevel_WrongPassword verifies that a bad password
// produces a clear error rather than a silent failure.
func TestRepoAuth_TaskLevel_WrongPassword(t *testing.T) {
	gitRoot := t.TempDir()
	initBareRepo(t, gitRoot, "secured-repo", "main")

	serverURL, cleanup := startTestHTTPGitServer(t, gitRoot, "x", "correct-token")
	defer cleanup()

	secretCleanup := setupSecretStoreForTest(t, map[string]string{
		"GIT_TOKEN": "wrong-token",
	})
	defer secretCleanup()

	cloneDir := filepath.Join(t.TempDir(), "work")
	os.MkdirAll(cloneDir, 0755)

	task := cronicle.Task{
		Name:    "test-task",
		Command: []string{"/bin/echo", "ok"},
		Path:    cloneDir,
		Repo: &cronicle.Repo{
			URL:      serverURL + "/secured-repo.git",
			Password: "$secret.GIT_TOKEN",
			Branch:   "main",
		},
	}

	_, err := task.Execute(time.Now())
	if err == nil {
		t.Fatal("expected clone to fail with wrong password, got nil error")
	}
}

// TestRepoAuth_ConfigLevel_Init verifies that config-level repo clones
// still work at Init() time (these use ${env.VAR}, not $secret).
func TestRepoAuth_ConfigLevel_Init(t *testing.T) {
	gitRoot := t.TempDir()
	initBareRepo(t, gitRoot, "config-repo", "main")

	token := "config-token-789"
	serverURL, cleanup := startTestHTTPGitServer(t, gitRoot, "x", token)
	defer cleanup()

	workDir := t.TempDir()

	conf := cronicle.Default()
	conf.Repo = &cronicle.Repo{
		URL:      serverURL + "/config-repo.git",
		Password: token, // simulates ${env.VAR} already resolved
		Branch:   "main",
	}

	err := conf.Init(workDir)
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	if !cronicle.DirExists(filepath.Join(workDir, ".git")) {
		t.Fatal(".git directory not created — config-level clone didn't happen")
	}
}

// TestRepoAuth_InitDoesNotCloneTaskRepos verifies that Init() no
// longer eagerly clones task-level repos (the fix for $secret support).
func TestRepoAuth_InitDoesNotCloneTaskRepos(t *testing.T) {
	gitRoot := t.TempDir()
	initBareRepo(t, gitRoot, "task-repo", "main")

	// Use a server that rejects all auth — if Init tried to clone
	// the task repo, it would fail.
	serverURL, cleanup := startTestHTTPGitServer(t, gitRoot, "x", "real-token")
	defer cleanup()

	conf := cronicle.Default()
	conf.Schedules[0].Tasks[0].Repo = &cronicle.Repo{
		URL:      serverURL + "/task-repo.git",
		Password: "$secret.NONEXISTENT_TOKEN",
		Branch:   "main",
	}

	// Init should succeed — it validates tasks but doesn't clone task repos
	err := conf.Init(filepath.Dir(conf.Schedules[0].Tasks[0].Path))
	if err != nil {
		t.Fatalf("Init should not clone task repos, but got error: %v", err)
	}
}
