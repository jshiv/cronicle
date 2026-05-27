package cronicle_test

import (
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// startTestHTTPGitServer launches an httptest.Server that serves
// git-http-backend over CGI. Only requests with the correct Basic
// auth credentials are accepted. Returns the server URL (including
// the username:password for convenience), and a cleanup function.
//
// The serveRoot must contain bare repos ({name}.git directories).
// Create them with initBareRepo.
func startTestHTTPGitServer(t *testing.T, serveRoot, username, password string) (baseURL string, cleanup func()) {
	t.Helper()

	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not on PATH: %v", err)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != username || p != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h := &cgi.Handler{
			Path: gitBin,
			Args: []string{"http-backend"},
			Env: []string{
				"GIT_PROJECT_ROOT=" + serveRoot,
				"GIT_HTTP_EXPORT_ALL=1",
			},
			InheritEnv: []string{"PATH"},
		}
		h.ServeHTTP(w, r)
	})

	ts := httptest.NewServer(handler)
	return ts.URL, ts.Close
}

// initBareRepo creates a bare git repo at root/{name}.git with one
// commit on the given branch containing a single file.
func initBareRepo(t *testing.T, root, name, branch string) string {
	t.Helper()

	barePath := filepath.Join(root, name+".git")
	workDir := t.TempDir()

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@test.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}

	run(workDir, "init", "-b", branch)
	if err := os.WriteFile(filepath.Join(workDir, "hello.txt"), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run(workDir, "add", ".")
	run(workDir, "commit", "-m", "initial")
	run("", "clone", "--bare", workDir, barePath)

	// Enable http push
	run(barePath, "config", "http.receivepack", "true")

	return barePath
}
