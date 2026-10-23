package cronicle

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2/hclparse"
)

// TestStarter_Runs: the cronicle.hcl that `cronicle init` writes must
// parse and run green out of the box, since it's the first run a new
// user (or an AI reading AGENTS.md) sees.
func TestStarter_Runs(t *testing.T) {
	// ExecTasks wraps the default slog handler; slog's own default writes
	// back through log and deadlocks, so install a real one.
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(prev)

	dir := t.TempDir()
	file := filepath.Join(dir, "cronicle.hcl")
	if err := os.WriteFile(file, starterHCL, 0644); err != nil {
		t.Fatal(err)
	}
	if err := ExecTasks(file, "", "", time.Now()); err != nil {
		t.Fatalf("starter schedule failed: %v", err)
	}
}

// TestGuide_HCLExamplesParse: every ```hcl block in the guide must parse,
// so the examples an AI copies from AGENTS.md are valid cronicle.hcl.
func TestGuide_HCLExamplesParse(t *testing.T) {
	blocks := regexp.MustCompile("(?s)```hcl\n(.*?)```").FindAllStringSubmatch(Guide, -1)
	if len(blocks) == 0 {
		t.Fatal("no hcl examples found in the guide")
	}
	for i, b := range blocks {
		file := filepath.Join(t.TempDir(), "cronicle.hcl")
		if err := os.WriteFile(file, []byte(b[1]), 0644); err != nil {
			t.Fatal(err)
		}
		if _, diags := ParseFile(file, hclparse.NewParser()); diags.HasErrors() {
			t.Errorf("guide example %d does not parse: %v\n%s", i, diags, b[1])
		}
	}
}

// TestInitStarter_WritesFilesOnce: init writes the starter schedule, the
// guide and the CLAUDE.md import, and never overwrites files the project
// already has.
func TestInitStarter_WritesFilesOnce(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("mine\n"), 0644); err != nil {
		t.Fatal(err)
	}
	InitStarter(dir, "", "")

	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(b)
	}
	if got := read("cronicle.hcl"); got != string(starterHCL) {
		t.Errorf("cronicle.hcl is not the starter:\n%s", got)
	}
	if got := read("AGENTS.md"); got != "mine\n" {
		t.Errorf("existing AGENTS.md was overwritten: %q", got)
	}
	if got := read("CLAUDE.md"); got != "@AGENTS.md\n" {
		t.Errorf("CLAUDE.md = %q", got)
	}

	os.Remove(filepath.Join(dir, "AGENTS.md"))
	InitStarter(dir, "", "")
	if got := read("AGENTS.md"); got != Guide {
		t.Errorf("AGENTS.md is not the guide")
	}
}
