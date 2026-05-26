package cronicle_test

import (
	"os"
	"testing"

	"github.com/hashicorp/hcl/v2/hclsimple"

	"github.com/jshiv/cronicle/internal/cronicle"
)

// Repo.Auth resolves auth from the repo block fields (password, key).
// These tests pin the behaviour platform pods and OSS git hosts depend on.

func TestRepoAuth_BasicFromPasswordField(t *testing.T) {
	r := &cronicle.Repo{
		URL:      "https://api.cronicle.dev/git/foo/bar",
		Username: "x",
		Password: "infra-token-value",
	}
	opts, err := r.Auth()
	if err != nil {
		t.Fatalf("Auth: %v", err)
	}
	if len(opts) == 0 {
		t.Fatal("Auth returned empty options for authenticated repo")
	}
}

func TestRepoAuth_UsernameDefaultsToX(t *testing.T) {
	r := &cronicle.Repo{
		URL:      "https://api.cronicle.dev/git/foo/bar",
		Password: "tok",
	}
	opts, err := r.Auth()
	if err != nil {
		t.Fatalf("Auth: %v", err)
	}
	if len(opts) == 0 {
		t.Fatal("Auth returned empty options")
	}
}

func TestRepoAuth_NilWhenAnonymous(t *testing.T) {
	r := &cronicle.Repo{URL: "https://github.com/jshiv/cronicle-sample.git"}
	opts, err := r.Auth()
	if err != nil {
		t.Fatalf("Auth: %v", err)
	}
	if len(opts) != 0 {
		t.Errorf("Auth returned %d options, want 0 for anonymous", len(opts))
	}
}

func TestRepoAuth_EnvInterpolationInPassword(t *testing.T) {
	t.Setenv("CRONICLE_TOKEN", "interpolated-secret")
	cronicle.RefreshEvalContext()

	hcl := `
schedule "s" {
  cron = ""
  task "t" {
    command = ["echo", "x"]
    repo {
      url      = "https://api.cronicle.dev/git/foo/bar"
      password = "${env.CRONICLE_TOKEN}"
    }
  }
}
`
	var conf cronicle.Config
	tmp := t.TempDir() + "/c.hcl"
	if err := writeFile(tmp, hcl); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	if err := hclsimple.DecodeFile(tmp, &cronicle.CommandEvalContext, &conf); err != nil {
		t.Fatalf("DecodeFile: %v", err)
	}
	r := conf.Schedules[0].Tasks[0].Repo
	if r == nil {
		t.Fatal("Repo nil after decode")
	}
	if r.Password != "interpolated-secret" {
		t.Fatalf("Password after parse = %q, want %q", r.Password, "interpolated-secret")
	}
	opts, err := r.Auth()
	if err != nil {
		t.Fatalf("Auth: %v", err)
	}
	if len(opts) == 0 {
		t.Fatal("Auth returned empty options for authenticated repo")
	}
}

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}
