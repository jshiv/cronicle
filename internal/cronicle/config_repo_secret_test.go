package cronicle_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle"
	"github.com/jshiv/cronicle/internal/cronicle/secretstore"
	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// Issue #177: a top-level repo block (the repo holding cronicle.hcl) with
// password = "$secret.GITHUB_TOKEN" must authenticate with the stored
// token at startup and on config refresh.

const configRepoToken = "s3cret-token"

// configRepoFixture serves a private (basic-auth) config repo whose
// cronicle.hcl names itself as the top-level repo, and returns a fresh
// project dir holding a copy of that file, plus a function that pushes a
// new version of the remote cronicle.hcl.
func configRepoFixture(t *testing.T) (projectDir string, push func(hcl string)) {
	t.Helper()
	serveRoot := t.TempDir()
	baseURL, cleanup := startTestHTTPGitServer(t, serveRoot, "x", configRepoToken)
	t.Cleanup(cleanup)

	hclFor := func(schedule string) string {
		return fmt.Sprintf(`repo {
  url      = "%s/cfg.git"
  branch   = "main"
  password = "$secret.GITHUB_TOKEN"
}

schedule %q {
  task "noop" {
    command = ["true"]
  }
}
`, baseURL, schedule)
	}
	work := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(work, "init", "-b", "main")
	write := func(hcl string) {
		if err := os.WriteFile(filepath.Join(work, "cronicle.hcl"), []byte(hcl), 0o644); err != nil {
			t.Fatal(err)
		}
		git(work, "add", ".")
		git(work, "commit", "-m", "config")
	}
	write(hclFor("first"))
	git("", "clone", "--bare", work, filepath.Join(serveRoot, "cfg.git"))
	git(filepath.Join(serveRoot, "cfg.git"), "config", "http.receivepack", "true")

	projectDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "cronicle.hcl"), []byte(hclFor("first")), 0o644); err != nil {
		t.Fatal(err)
	}
	push = func(schedule string) {
		write(hclFor(schedule))
		git(work, "push", filepath.Join(serveRoot, "cfg.git"), "main")
	}
	return projectDir, push
}

// useSecretStore binds the process secret store to a fresh state.db in
// dir holding the given secrets, as cronicle run/exec do at startup.
func useSecretStore(t *testing.T, dir string, secrets map[string]string) {
	t.Helper()
	db, err := state.Open(filepath.Join(dir, "secrets.db"))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range secrets {
		if err := db.PutSecret(k, v, "test"); err != nil {
			t.Fatal(err)
		}
	}
	store := secretstore.New().SetBackend(db).StaleTTL(0)
	if err := store.Start(context.Background(), nil, 0); err != nil {
		t.Fatal(err)
	}
	prev := secretstore.Default()
	secretstore.SetDefault(store)
	t.Cleanup(func() { secretstore.SetDefault(prev); db.Close() })
}

func quietLogs(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

func TestConfigRepo_StoredSecret_LoadAndRefresh(t *testing.T) {
	quietLogs(t)
	project, push := configRepoFixture(t)
	file := filepath.Join(project, "cronicle.hcl")
	useSecretStore(t, t.TempDir(), map[string]string{"GITHUB_TOKEN": configRepoToken})

	conf, err := cronicle.GetConfig(file)
	if err != nil {
		t.Fatalf("startup load with a stored token: %v", err)
	}
	if conf.Repo.Password != "$secret.GITHUB_TOKEN" {
		t.Errorf("the config must keep the reference, not the token: %q", conf.Repo.Password)
	}
	if _, err := os.Stat(filepath.Join(project, ".git")); err != nil {
		t.Errorf("the project dir should now be a checkout of the config repo: %v", err)
	}

	// A refresh fetches with the same stored token. GetConfig parses the
	// file on disk and then syncs the checkout, so a pushed change lands
	// on this load and takes effect on the next one (the next
	// config_refresh tick).
	push("second")
	if _, err := cronicle.GetConfig(file); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if b, _ := os.ReadFile(file); !strings.Contains(string(b), `"second"`) {
		t.Fatalf("the authenticated fetch should have brought down the pushed config:\n%s", b)
	}
	conf, err = cronicle.GetConfig(file)
	if err != nil || conf.Schedules[0].Name != "second" {
		t.Fatalf("the next load should use the pushed config: %v %+v", err, conf.Schedules)
	}

	// A task that runs in the project dir re-fetches the config repo
	// before it runs (task.CronicleRepo); that fetch uses the token too.
	sched := conf.Schedules[0]
	sched.RunID = "R1"
	if err := sched.ExecuteTasks(); err != nil {
		t.Errorf("a task in the config repo checkout: %v", err)
	}
}

func TestConfigRepo_MissingOrUnavailableSecret(t *testing.T) {
	quietLogs(t)
	project, _ := configRepoFixture(t)
	file := filepath.Join(project, "cronicle.hcl")

	prev := secretstore.Default()
	secretstore.SetDefault(secretstore.New())
	_, err := cronicle.GetConfig(file)
	secretstore.SetDefault(prev)
	if err == nil || !strings.Contains(err.Error(), "no secret store is set up") {
		t.Errorf("without a secret store: want a clear error, got %v", err)
	}

	useSecretStore(t, t.TempDir(), map[string]string{})
	_, err = cronicle.GetConfig(file)
	if err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN") || !strings.Contains(err.Error(), "cronicle secret set GITHUB_TOKEN") {
		t.Errorf("an unset secret: want its name and how to set it, got %v", err)
	}
	if err != nil && strings.Contains(err.Error(), configRepoToken) {
		t.Error("the token leaked into an error")
	}
}

// TestRun_ResolvesConfigRepoSecretAtStartup: cronicle run sets up the
// state store and secret source (PostStateStoreHook) before loading the
// config, so the config repo's first fetch already has the token. Before
// the fix, Run loaded the config first and Fatal'd on auth.
func TestRun_ResolvesConfigRepoSecretAtStartup(t *testing.T) {
	quietLogs(t)
	project, _ := configRepoFixture(t)
	prev := secretstore.Default()
	t.Cleanup(func() { secretstore.SetDefault(prev) })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cronicle.Run(ctx, filepath.Join(project, "cronicle.hcl"), cronicle.RunOptions{
		PostStateStoreHook: func() error {
			// What `cronicle secret set` stored earlier, and what the CLI's
			// hook binds: the project's own state.db.
			be := cronicle.StateStore()
			if be == nil {
				return fmt.Errorf("state store not open before the hook")
			}
			if err := be.PutSecret("GITHUB_TOKEN", configRepoToken, "test"); err != nil {
				return err
			}
			store := secretstore.New().SetBackend(be).StaleTTL(0)
			if err := store.Start(ctx, nil, 0); err != nil {
				return err
			}
			secretstore.SetDefault(store)
			return nil
		},
	})
	if _, err := os.Stat(filepath.Join(project, ".git")); err != nil {
		t.Errorf("Run should have checked out the config repo with the stored token: %v", err)
	}
}
