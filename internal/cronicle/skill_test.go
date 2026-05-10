package cronicle

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parseFrontmatter accepts files with proper YAML fences and rejects
// missing/unterminated fences.
func TestParseFrontmatter(t *testing.T) {
	good := []byte(`---
name: my-skill
description: Does the thing.
allowed-tools:
  - bash
---
# My Skill
Body content here.
`)
	fm, body, err := parseFrontmatter(good)
	if err != nil {
		t.Fatalf("good: %v", err)
	}
	if fm.Name != "my-skill" || fm.Description != "Does the thing." {
		t.Fatalf("frontmatter parse: got %+v", fm)
	}
	if len(fm.AllowedTools) != 1 || fm.AllowedTools[0] != "bash" {
		t.Fatalf("allowed-tools: got %v", fm.AllowedTools)
	}
	if !strings.HasPrefix(body, "# My Skill") {
		t.Fatalf("body trim: %q", body)
	}

	// CRLF tolerance
	crlf := []byte("---\r\nname: x\r\ndescription: y\r\n---\r\nbody\r\n")
	if _, _, err := parseFrontmatter(crlf); err != nil {
		t.Fatalf("crlf: %v", err)
	}

	// Missing opening fence
	if _, _, err := parseFrontmatter([]byte("no fence\nname: x\n")); err == nil {
		t.Fatalf("missing fence accepted")
	}

	// Unterminated frontmatter
	if _, _, err := parseFrontmatter([]byte("---\nname: x\nno-close\n")); err == nil {
		t.Fatalf("unterminated frontmatter accepted")
	}

	// Closing fence without trailing newline (file ends right after `---`)
	tail := []byte("---\nname: x\ndescription: y\n---")
	if fm, body, err := parseFrontmatter(tail); err != nil {
		t.Fatalf("trailing-fence-no-newline: %v", err)
	} else if fm.Name != "x" || body != "" {
		t.Fatalf("trailing-fence-no-newline: name=%q body=%q", fm.Name, body)
	}
}

// LoadSkill writes a skill to disk and reads it back; missing
// name/description are surfaced as load-time errors.
func TestLoadSkill(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "skills", "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte(`---
name: demo
description: A demo skill.
---
do the thing.
`)
	skillPath := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(skillPath, body, 0o644); err != nil {
		t.Fatal(err)
	}

	sk, err := LoadSkill(skillPath, root)
	if err != nil {
		t.Fatalf("LoadSkill: %v", err)
	}
	if sk.Name != "demo" || sk.Description != "A demo skill." {
		t.Fatalf("LoadSkill: got %+v", sk)
	}
	if sk.Dir != "skills/demo" {
		t.Fatalf("LoadSkill dir: got %q", sk.Dir)
	}

	// Missing name fails fast.
	bad := filepath.Join(dir, "BAD.md")
	_ = os.WriteFile(bad, []byte("---\ndescription: only desc\n---\nbody\n"), 0o644)
	if _, err := LoadSkill(bad, root); err == nil {
		t.Fatalf("LoadSkill accepted missing name")
	}
}

// LoadSkillsForTask resolves paths against the task root, rejecting
// `..` traversal and absolute paths.
func TestLoadSkillsForTask(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "skills", "x")
	_ = os.MkdirAll(dir, 0o755)
	body := []byte("---\nname: x\ndescription: x desc\n---\nbody\n")
	_ = os.WriteFile(filepath.Join(dir, "SKILL.md"), body, 0o644)

	skills, err := LoadSkillsForTask(root, root, []string{"skills/x/SKILL.md"})
	if err != nil {
		t.Fatalf("LoadSkillsForTask: %v", err)
	}
	if len(skills) != 1 || skills[0].Name != "x" {
		t.Fatalf("LoadSkillsForTask: got %+v", skills)
	}

	// `..` traversal blocked.
	if _, err := LoadSkillsForTask(root, root, []string{"../escape/SKILL.md"}); err == nil {
		t.Fatalf("`..` traversal allowed")
	}
}

// LoadSkillsForTask falls back to configRoot when the skill isn't co-located
// with the task workspace. This is the typical case when a `repo` block
// makes task.Path the cloned repo dir and the skill lives alongside
// cronicle.hcl instead.
func TestLoadSkillsForTaskFallbackToConfigRoot(t *testing.T) {
	cfg := t.TempDir()
	taskWS := t.TempDir() // separate workspace, like a cloned-repo dir

	// Skill lives under cfg, not under the task workspace.
	dir := filepath.Join(cfg, "skills", "report")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("---\nname: report\ndescription: a report skill\n---\nbody\n")
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	skills, err := LoadSkillsForTask(taskWS, cfg, []string{"skills/report/SKILL.md"})
	if err != nil {
		t.Fatalf("LoadSkillsForTask fallback: %v", err)
	}
	if len(skills) != 1 || skills[0].Name != "report" {
		t.Fatalf("expected report skill, got %+v", skills)
	}
	// Skill is outside the workspace, so Dir should be absolute.
	if !filepath.IsAbs(skills[0].Dir) {
		t.Fatalf("expected absolute Dir for skill outside workspace, got %q", skills[0].Dir)
	}
}

// FormatAvailableSkillsSection lists every skill on its own bulleted line.
func TestFormatAvailableSkillsSection(t *testing.T) {
	if got := FormatAvailableSkillsSection(nil); got != "" {
		t.Fatalf("nil skills produced %q", got)
	}
	skills := []*Skill{
		{Name: "a", Description: "first one"},
		{Name: "b", Description: "second one\nwith an embedded newline"},
	}
	out := FormatAvailableSkillsSection(skills)
	if !strings.Contains(out, "- a: first one") {
		t.Fatalf("missing skill a: %q", out)
	}
	if strings.Contains(out, "embedded\nwith") {
		t.Fatalf("oneLine didn't collapse newlines: %q", out)
	}
}

// SkillTool wraps a set of skills, dispatches load_skill, and tracks the
// names actually loaded so audit log can diff catalog vs use.
func TestSkillTool(t *testing.T) {
	skills := []*Skill{
		{Name: "alpha", Description: "first", Dir: "skills/alpha", Body: "alpha body"},
		{Name: "beta", Description: "second", Dir: "skills/beta", Body: "beta body"},
	}
	st, err := NewSkillTool(skills)
	if err != nil {
		t.Fatalf("NewSkillTool: %v", err)
	}

	// Available is sorted.
	got := st.Available()
	want := []string{"alpha", "beta"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Available: got %v, want %v", got, want)
	}

	out, isErr := st.Execute(context.Background(), json.RawMessage(`{"name":"alpha"}`))
	if isErr {
		t.Fatalf("Execute alpha: %s", out)
	}
	if !strings.Contains(out, "DIRECTORY: skills/alpha") {
		t.Fatalf("Execute output missing DIRECTORY: %q", out)
	}
	if !strings.Contains(out, "alpha body") {
		t.Fatalf("Execute output missing body: %q", out)
	}

	// Loaded is recorded once even on repeated calls.
	st.Execute(context.Background(), json.RawMessage(`{"name":"alpha"}`))
	loaded := st.Loaded()
	if len(loaded) != 1 || loaded[0] != "alpha" {
		t.Fatalf("Loaded: got %v", loaded)
	}

	// Unknown name returns isErr with a hint listing what's available.
	out, isErr = st.Execute(context.Background(), json.RawMessage(`{"name":"missing"}`))
	if !isErr {
		t.Fatalf("unknown name accepted: %q", out)
	}
	if !strings.Contains(out, "available: alpha, beta") {
		t.Fatalf("unknown name error missing hint: %q", out)
	}

	// Empty name is rejected.
	out, isErr = st.Execute(context.Background(), json.RawMessage(`{"name":""}`))
	if !isErr {
		t.Fatalf("empty name accepted: %q", out)
	}
}

// NewSkillTool rejects duplicate skill names — load_skill couldn't
// disambiguate.
func TestSkillToolRejectsDuplicates(t *testing.T) {
	_, err := NewSkillTool([]*Skill{
		{Name: "x", Description: "one"},
		{Name: "x", Description: "two"},
	})
	if err == nil {
		t.Fatalf("duplicate names accepted")
	}
}
