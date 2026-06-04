package cronicle

import (
	"context"
	"testing"

	"github.com/jshiv/cronicle/internal/cronicle/secretstore"
	"github.com/jshiv/cronicle/internal/cronicle/state"
)

func setupSecretStore(t *testing.T, secrets map[string]string) func() {
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
	prev := secretstore.Default()
	secretstore.SetDefault(store)
	return func() {
		secretstore.SetDefault(prev)
		db.Close()
	}
}

func TestResolveRepoSecrets_ExpandsSecretRef(t *testing.T) {
	cleanup := setupSecretStore(t, map[string]string{
		"GITHUB_TOKEN": "ghp_test123",
	})
	defer cleanup()

	repo := &Repo{Password: "$secret.GITHUB_TOKEN"}
	out := resolveRepoSecrets(repo)

	if out.Password != "ghp_test123" {
		t.Errorf("expected resolved password %q, got %q", "ghp_test123", out.Password)
	}
	// The original *Repo must be unchanged — that's the whole point of
	// returning a clone instead of mutating in place. Tasks sharing the
	// schedule-level *Repo must not see each other's resolutions.
	if repo.Password != "$secret.GITHUB_TOKEN" {
		t.Errorf("input repo mutated; got Password=%q", repo.Password)
	}
	if out == repo {
		t.Errorf("expected a fresh *Repo, got the same pointer back")
	}
}

func TestResolveRepoSecrets_LeavesEnvSyntaxAlone(t *testing.T) {
	cleanup := setupSecretStore(t, map[string]string{})
	defer cleanup()

	repo := &Repo{Password: "${env.CRONICLE_TOKEN}"}
	out := resolveRepoSecrets(repo)

	if out.Password != "${env.CRONICLE_TOKEN}" {
		t.Errorf("should not modify env syntax, got %q", out.Password)
	}
	// No expansion needed → same pointer returned, no allocation.
	if out != repo {
		t.Errorf("expected same pointer when no expansion, got fresh clone")
	}
}

func TestResolveRepoSecrets_NoopWhenNil(t *testing.T) {
	if out := resolveRepoSecrets(nil); out != nil {
		t.Errorf("expected nil for nil input, got %v", out)
	}
}

func TestResolveRepoSecrets_NoopWhenEmpty(t *testing.T) {
	repo := &Repo{Password: ""}
	out := resolveRepoSecrets(repo)
	if out.Password != "" {
		t.Errorf("expected empty, got %q", out.Password)
	}
	if out != repo {
		t.Errorf("expected same pointer for noop, got fresh clone")
	}
}

func TestResolveRepoSecrets_NoopWhenStoreUnconfigured(t *testing.T) {
	repo := &Repo{Password: "$secret.GITHUB_TOKEN"}
	out := resolveRepoSecrets(repo)
	if out.Password != "$secret.GITHUB_TOKEN" {
		t.Errorf("should leave unresolved when store not configured, got %q", out.Password)
	}
	if out != repo {
		t.Errorf("expected same pointer when store unconfigured, got fresh clone")
	}
}

func TestResolveRepoSecrets_PlainPasswordUnchanged(t *testing.T) {
	cleanup := setupSecretStore(t, map[string]string{})
	defer cleanup()

	repo := &Repo{Password: "my-plain-token"}
	out := resolveRepoSecrets(repo)

	if out.Password != "my-plain-token" {
		t.Errorf("plain password should be unchanged, got %q", out.Password)
	}
	if out != repo {
		t.Errorf("expected same pointer for plain password, got fresh clone")
	}
}

// Two tasks sharing a single schedule-level *Repo pointer must each
// observe their own resolved password without racing on the shared
// memory. This is the H8 invariant: resolveRepoSecrets must not mutate
// the input.
func TestResolveRepoSecrets_ConcurrentSharedRepo(t *testing.T) {
	cleanup := setupSecretStore(t, map[string]string{
		"GITHUB_TOKEN": "ghp_concurrent",
	})
	defer cleanup()

	shared := &Repo{Password: "$secret.GITHUB_TOKEN"}
	const goroutines = 50
	done := make(chan *Repo, goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			done <- resolveRepoSecrets(shared)
		}()
	}
	for i := 0; i < goroutines; i++ {
		out := <-done
		if out.Password != "ghp_concurrent" {
			t.Errorf("goroutine %d got password %q, want %q", i, out.Password, "ghp_concurrent")
		}
	}
	// Original must still be untouched.
	if shared.Password != "$secret.GITHUB_TOKEN" {
		t.Errorf("shared repo mutated under concurrency; got %q", shared.Password)
	}
}
