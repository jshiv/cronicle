package cronicle_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jshiv/cronicle/internal/cronicle"
)

// TestClone_NonEmptyDir verifies Clone succeeds when the target
// directory already has files (e.g. cronicled writes cronicle.hcl
// before cronicle run starts). go-git v6 rejects PlainClone into
// non-empty dirs; Clone handles this via init + fetch.
func TestClone_NonEmptyDir(t *testing.T) {
	dir := t.TempDir()

	// Pre-populate with a file (simulates cronicled writing HCL)
	if err := os.WriteFile(filepath.Join(dir, "cronicle.hcl"), []byte("# placeholder"), 0644); err != nil {
		t.Fatal(err)
	}

	opts, err := (&cronicle.Repo{URL: "https://github.com/jshiv/cronicle-sample.git"}).Auth()
	if err != nil {
		t.Fatal(err)
	}

	g, err := cronicle.Clone(dir, "https://github.com/jshiv/cronicle-sample.git", opts)
	if err != nil {
		t.Fatalf("Clone into non-empty dir failed: %v", err)
	}

	// Verify repo is functional
	if g.Head == nil {
		t.Fatal("Head is nil after clone")
	}
	if !cronicle.DirExists(filepath.Join(dir, ".git")) {
		t.Fatal(".git directory not created")
	}

	// Verify original file still exists (not clobbered)
	if _, err := os.Stat(filepath.Join(dir, "cronicle.hcl")); err != nil {
		t.Fatalf("pre-existing file was removed: %v", err)
	}
}

// TestClone_EmptyDir verifies Clone still works with an empty dir
// (the normal PlainClone path).
func TestClone_EmptyDir(t *testing.T) {
	dir := t.TempDir()

	opts, err := (&cronicle.Repo{URL: "https://github.com/jshiv/cronicle-sample.git"}).Auth()
	if err != nil {
		t.Fatal(err)
	}

	g, err := cronicle.Clone(dir, "https://github.com/jshiv/cronicle-sample.git", opts)
	if err != nil {
		t.Fatalf("Clone into empty dir failed: %v", err)
	}

	if g.Head == nil {
		t.Fatal("Head is nil after clone")
	}
}

// TestScratch_SurvivesCheckout verifies that files written to
// .cronicle/ persist across a Force:true checkout. This is the
// inter-task artifact passing mechanism — upstream tasks write to
// ${scratch}, downstream tasks read from it.
func TestScratch_SurvivesCheckout(t *testing.T) {
	dir := t.TempDir()

	opts, err := (&cronicle.Repo{URL: "https://github.com/jshiv/cronicle-sample.git"}).Auth()
	if err != nil {
		t.Fatal(err)
	}

	g, err := cronicle.Clone(dir, "https://github.com/jshiv/cronicle-sample.git", opts)
	if err != nil {
		t.Fatal(err)
	}

	// Write a file to .cronicle/scratch (simulates task output)
	scratchDir := filepath.Join(dir, ".cronicle", "scratch")
	if err := os.MkdirAll(scratchDir, 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(scratchDir, "data.txt")
	if err := os.WriteFile(sentinel, []byte("task-output"), 0644); err != nil {
		t.Fatal(err)
	}

	// Force checkout (same branch — simulates the per-task checkout)
	if err := g.Checkout("master", ""); err != nil {
		t.Fatalf("Checkout failed: %v", err)
	}

	// Verify sentinel survived
	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf(".cronicle/scratch/data.txt was wiped by checkout: %v", err)
	}
	if string(data) != "task-output" {
		t.Fatalf("got %q, want %q", string(data), "task-output")
	}
}

// TestClone_NonEmptyDir_WithSSH verifies Clone into a non-empty
// directory works with SSH auth (deploy key).
func TestClone_NonEmptyDir_WithSSH(t *testing.T) {
	if testSSHAddr == "" {
		t.Skip("SSH test server not available")
	}

	dir := t.TempDir()
	// Pre-populate
	if err := os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}

	sshURL := "ssh://git@" + testSSHAddr + "/test_repo"
	repo := cronicle.Repo{URL: sshURL, DeployKey: "./test/test_deploy_key"}
	opts, err := repo.Auth()
	if err != nil {
		t.Fatal(err)
	}

	g, err := cronicle.Clone(dir, sshURL, opts)
	if err != nil {
		t.Fatalf("SSH Clone into non-empty dir failed: %v", err)
	}
	if g.Head == nil {
		t.Fatal("Head is nil after SSH clone")
	}
}
