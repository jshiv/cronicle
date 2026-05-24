package cronicle_test

import (
	"os"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/hashicorp/hcl/v2/hclsimple"

	"github.com/jshiv/cronicle/internal/cronicle"
)

// Repo.Auth resolves HTTP Basic auth from the password field, which is
// the standard HCL string and so supports ${env.X} interpolation. These
// tests pin the behaviour platform pods (worker + producer) and OSS git
// hosts both depend on.

func TestRepoAuth_BasicFromPasswordField(t *testing.T) {
	r := &cronicle.Repo{
		URL:      "https://api.cronicle.dev/git/foo/bar",
		Username: "x",
		Password: "infra-token-value",
	}
	auth, err := r.Auth()
	if err != nil {
		t.Fatalf("Auth: %v", err)
	}
	ba, ok := auth.(*http.BasicAuth)
	if !ok {
		t.Fatalf("got %T, want *http.BasicAuth", auth)
	}
	if ba.Username != "x" {
		t.Errorf("Username = %q, want %q", ba.Username, "x")
	}
	if ba.Password != "infra-token-value" {
		t.Errorf("Password = %q, want literal value", ba.Password)
	}
}

func TestRepoAuth_UsernameDefaultsToX(t *testing.T) {
	r := &cronicle.Repo{
		URL:      "https://api.cronicle.dev/git/foo/bar",
		Password: "tok",
	}
	auth, err := r.Auth()
	if err != nil {
		t.Fatalf("Auth: %v", err)
	}
	ba := auth.(*http.BasicAuth)
	if ba.Username != "x" {
		t.Errorf("Username = %q, want default %q", ba.Username, "x")
	}
}

func TestRepoAuth_NilWhenAnonymous(t *testing.T) {
	r := &cronicle.Repo{URL: "https://github.com/jshiv/cronicle-sample.git"}
	auth, err := r.Auth()
	if err != nil {
		t.Fatalf("Auth: %v", err)
	}
	if auth != nil {
		t.Errorf("Auth = %v, want nil for anonymous", auth)
	}
}

// Pins the ${env.X} interpolation flow end-to-end: an HCL string with
// ${env.CRONICLE_TOKEN} parses to the env var's runtime value, and
// Repo.Auth then surfaces it as HTTP Basic auth. This is the canonical
// shape we want users (and Claude-generated HCL) to write.
func TestRepoAuth_EnvInterpolationInPassword(t *testing.T) {
	t.Setenv("CRONICLE_TOKEN", "interpolated-secret")
	// Re-snapshot the eval context's env namespace so t.Setenv above is
	// visible to gohcl. Production code calls this inside ParseFile /
	// ParseBytes; this test goes through hclsimple directly so it has
	// to refresh explicitly.
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
		t.Fatalf("Password after parse = %q, want %q (env interpolation didn't fire)", r.Password, "interpolated-secret")
	}
	auth, err := r.Auth()
	if err != nil {
		t.Fatalf("Auth: %v", err)
	}
	ba := auth.(*http.BasicAuth)
	if ba.Password != "interpolated-secret" {
		t.Errorf("Auth password = %q, want %q", ba.Password, "interpolated-secret")
	}
}

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}
