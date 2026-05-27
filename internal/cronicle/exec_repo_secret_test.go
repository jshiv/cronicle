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
	resolveRepoSecrets(repo)

	if repo.Password != "ghp_test123" {
		t.Errorf("expected password %q, got %q", "ghp_test123", repo.Password)
	}
}

func TestResolveRepoSecrets_LeavesEnvSyntaxAlone(t *testing.T) {
	cleanup := setupSecretStore(t, map[string]string{})
	defer cleanup()

	repo := &Repo{Password: "${env.CRONICLE_TOKEN}"}
	resolveRepoSecrets(repo)

	if repo.Password != "${env.CRONICLE_TOKEN}" {
		t.Errorf("should not modify env syntax, got %q", repo.Password)
	}
}

func TestResolveRepoSecrets_NoopWhenNil(t *testing.T) {
	resolveRepoSecrets(nil)
}

func TestResolveRepoSecrets_NoopWhenEmpty(t *testing.T) {
	repo := &Repo{Password: ""}
	resolveRepoSecrets(repo)
	if repo.Password != "" {
		t.Errorf("expected empty, got %q", repo.Password)
	}
}

func TestResolveRepoSecrets_NoopWhenStoreUnconfigured(t *testing.T) {
	repo := &Repo{Password: "$secret.GITHUB_TOKEN"}
	resolveRepoSecrets(repo)
	if repo.Password != "$secret.GITHUB_TOKEN" {
		t.Errorf("should leave unresolved when store not configured, got %q", repo.Password)
	}
}

func TestResolveRepoSecrets_PlainPasswordUnchanged(t *testing.T) {
	cleanup := setupSecretStore(t, map[string]string{})
	defer cleanup()

	repo := &Repo{Password: "my-plain-token"}
	resolveRepoSecrets(repo)

	if repo.Password != "my-plain-token" {
		t.Errorf("plain password should be unchanged, got %q", repo.Password)
	}
}
