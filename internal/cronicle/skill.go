// Skill loading + the load_skill meta-tool. cronicle implements Anthropic's
// Agent Skills open standard with progressive disclosure: SKILL.md frontmatter
// (name + description) is injected into the system prompt as a catalog; the
// body is fetched on demand by the agent calling the load_skill tool.
//
// File layout convention (per the standard):
//
//	<task workspace>/
//	  skills/
//	    <skill-name>/
//	      SKILL.md              ← frontmatter + body
//	      scripts/...           ← supporting executables / templates
//
// Each entry in agent.skills HCL is the path to a SKILL.md (workspace-
// relative). The skill's directory is filepath.Dir(path) and bundled files
// resolve against that — load_skill returns a DIRECTORY header so the agent
// can compose paths to the skill's scripts when invoking bash/text_editor.

package cronicle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"
	"gopkg.in/yaml.v3"
)

// Skill is a parsed SKILL.md plus the workspace-relative directory it lives
// in (used to compose paths to bundled scripts/templates).
type Skill struct {
	// Name is the skill identifier (frontmatter `name`). Required.
	Name string
	// Description tells the agent when to use the skill (frontmatter
	// `description`). Required — without it the agent has nothing to match
	// the task against.
	Description string
	// Dir is the skill's directory, workspace-relative (e.g. "skills/data-
	// analysis"). The agent receives this in the load_skill response so it
	// can resolve paths to scripts/foo.py against this prefix.
	Dir string
	// AllowedTools is the frontmatter `allowed-tools` list. Informational
	// — the agent's actual tool universe is set by Agent.Tools at config
	// time. Surfaced in the load_skill response so the agent knows what
	// the skill expects to use.
	AllowedTools []string
	// Body is the markdown content after the YAML frontmatter, verbatim.
	Body string
}

// skillFrontmatter mirrors the YAML frontmatter fields cronicle understands.
// Other keys are accepted but ignored (forward-compatibility with the
// evolving Skills standard).
type skillFrontmatter struct {
	Name         string   `yaml:"name"`
	Description  string   `yaml:"description"`
	AllowedTools []string `yaml:"allowed-tools"`
}

// LoadSkill reads the SKILL.md at skillAbs (an absolute path under taskRoot)
// and returns the parsed Skill. Errors are surfaced verbatim — a misconfigured
// skill should fail the run loudly, not silently produce a degraded prompt.
func LoadSkill(skillAbs, taskRoot string) (*Skill, error) {
	data, err := os.ReadFile(skillAbs)
	if err != nil {
		return nil, fmt.Errorf("read skill %s: %w", skillAbs, err)
	}
	fm, body, err := parseFrontmatter(data)
	if err != nil {
		return nil, fmt.Errorf("parse skill %s: %w", skillAbs, err)
	}
	if fm.Name == "" {
		return nil, fmt.Errorf("skill %s: frontmatter `name` is required", skillAbs)
	}
	if fm.Description == "" {
		return nil, fmt.Errorf("skill %s: frontmatter `description` is required", skillAbs)
	}
	dirAbs := filepath.Dir(skillAbs)
	dirRel, err := filepath.Rel(taskRoot, dirAbs)
	if err != nil {
		return nil, fmt.Errorf("skill %s: dir not under workspace: %w", skillAbs, err)
	}
	return &Skill{
		Name:         fm.Name,
		Description:  fm.Description,
		Dir:          filepath.ToSlash(dirRel),
		AllowedTools: fm.AllowedTools,
		Body:         body,
	}, nil
}

// parseFrontmatter splits a SKILL.md body into its YAML frontmatter and
// markdown body. The frontmatter is delimited by `---` lines at the very
// start of the file. Files without frontmatter return an error: skills
// without name+description can't be advertised to the agent.
func parseFrontmatter(data []byte) (skillFrontmatter, string, error) {
	var fm skillFrontmatter
	s := string(data)
	// Tolerate a leading UTF-8 BOM (U+FEFF) and CRLF endings that come from
	// editors on Windows or copy-paste from web pages.
	s = strings.TrimPrefix(s, "\uFEFF")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return fm, "", errors.New("missing YAML frontmatter (file must start with `---`)")
	}
	rest := s[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	endLen := len("\n---\n")
	if end < 0 {
		// Allow a closing fence without trailing newline (file ends right
		// after `---`).
		if strings.HasSuffix(rest, "\n---") {
			end = len(rest) - len("\n---")
			endLen = len("\n---")
		} else {
			return fm, "", errors.New("unterminated YAML frontmatter (missing closing `---`)")
		}
	}
	fmText := rest[:end]
	body := ""
	if end+endLen <= len(rest) {
		body = rest[end+endLen:]
	}
	body = strings.TrimLeft(body, "\n")
	if err := yaml.Unmarshal([]byte(fmText), &fm); err != nil {
		return fm, "", fmt.Errorf("yaml: %w", err)
	}
	return fm, body, nil
}

// SkillTool is the load_skill meta-tool: a custom Anthropic tool that takes
// a skill name and returns the body + DIRECTORY header so the agent can read
// instructions and resolve paths to bundled scripts. Progressive disclosure:
// the system prompt only carries name+description for each skill; the agent
// invokes load_skill when it decides a skill is relevant.
//
// SkillTool is bound to a specific run (a frozen map of available skills) and
// records which skills the agent fetched, so the run's slog event can carry
// skills_loaded for audit ("did the 3am job actually use the report-writer
// skill, or did it just have it available?").
type SkillTool struct {
	skills map[string]*Skill // by Name
	mu     sync.Mutex
	loaded []string // names, in load order
}

// NewSkillTool constructs a load_skill tool from a slice of pre-parsed skills.
// Duplicate names are rejected — having two skills with the same name in
// available-skills would make load_skill ambiguous.
func NewSkillTool(skills []*Skill) (*SkillTool, error) {
	m := make(map[string]*Skill, len(skills))
	for _, sk := range skills {
		if _, dup := m[sk.Name]; dup {
			return nil, fmt.Errorf("duplicate skill name %q (each skill must have a unique name)", sk.Name)
		}
		m[sk.Name] = sk
	}
	return &SkillTool{skills: m}, nil
}

// Loaded returns the names of skills the agent fetched via load_skill, in
// the order they were first loaded. Safe to call after the run finishes.
func (s *SkillTool) Loaded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.loaded))
	copy(out, s.loaded)
	return out
}

// Available returns the names of all skills registered for this run, sorted
// for deterministic logging.
func (s *SkillTool) Available() []string {
	out := make([]string, 0, len(s.skills))
	for name := range s.skills {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Name returns "load_skill". Stable across runs so transcripts compare cleanly.
func (s *SkillTool) Name() string { return "load_skill" }

// Definition declares load_skill as a custom (Type=custom) tool with a single
// required string input `name`. The description is intentionally directive —
// it tells the model exactly when to use this tool, since the available-
// skills section in the system prompt is the only signal the model gets that
// these are the names to pass.
func (s *SkillTool) Definition() anthropic.ToolUnionParam {
	return anthropic.ToolUnionParam{
		OfTool: &anthropic.ToolParam{
			Name:        "load_skill",
			Type:        anthropic.ToolTypeCustom,
			Description: anthropic.String("Load the full instructions for a skill listed in the Available Skills section of the system prompt. Returns the skill's body, working directory (for resolving paths to bundled scripts), and any allowed-tools metadata. Call this once per skill you decide to use."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "The skill name as listed in Available Skills.",
					},
				},
				Required: []string{"name"},
			},
		},
	}
}

// Execute resolves the requested skill name and returns the formatted body.
// Records the load in s.loaded for the run-level audit field.
func (s *SkillTool) Execute(_ context.Context, input json.RawMessage) (string, bool) {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return fmt.Sprintf("Error: invalid load_skill input: %v", err), true
	}
	if args.Name == "" {
		return "Error: load_skill requires a skill name", true
	}
	sk, ok := s.skills[args.Name]
	if !ok {
		return fmt.Sprintf("Error: no skill named %q is available for this run (available: %s)",
			args.Name, strings.Join(s.Available(), ", ")), true
	}
	s.recordLoaded(args.Name)
	return s.format(sk), false
}

func (s *SkillTool) recordLoaded(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.loaded {
		if n == name {
			return
		}
	}
	s.loaded = append(s.loaded, name)
}

// format returns the load_skill response text the model receives. The
// DIRECTORY header is the load-bearing piece for multi-skill runs: each
// skill's response carries its own directory so paths in different skill
// bodies don't collide when both bodies are in the conversation.
func (s *SkillTool) format(sk *Skill) string {
	var b strings.Builder
	fmt.Fprintf(&b, "SKILL: %s\n", sk.Name)
	fmt.Fprintf(&b, "DIRECTORY: %s\n", sk.Dir)
	if len(sk.AllowedTools) > 0 {
		fmt.Fprintf(&b, "ALLOWED_TOOLS: %s\n", strings.Join(sk.AllowedTools, ", "))
	}
	b.WriteString("\n")
	b.WriteString(sk.Body)
	if !strings.HasSuffix(sk.Body, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("\nNote: paths in this skill are relative to DIRECTORY above. When invoking scripts, prefix the path (e.g. ")
	b.WriteString(sk.Dir)
	b.WriteString("/scripts/foo.py).\n")
	return b.String()
}

// LoadSkillsForTask resolves and loads each path in agent.Skills.
// Resolution order, first-hit wins:
//
//  1. taskRoot/<path> — co-located with the task workspace (typical when
//     no `repo` block is set, so taskRoot == configRoot).
//  2. configRoot/<path> — alongside cronicle.hcl. Lets skills be operational
//     config that doesn't live inside a cloned repo.
//
// The skill's Dir is computed relative to whichever root resolved it. If the
// resolution root differs from taskRoot, Dir is recorded as an absolute path
// — the agent's bash tool can use it directly; text_editor stays workspace-
// confined to taskRoot, so bundled-file editing requires the skill to live
// inside the workspace.
//
// Path traversal is re-checked here as defense in depth (Validate rejected
// `..`/abs paths at config time, but the resolution root may have changed
// between Validate and Execute if a repo was cloned).
func LoadSkillsForTask(taskRoot, configRoot string, paths []string) ([]*Skill, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	wsAbs, err := filepath.Abs(taskRoot)
	if err != nil {
		return nil, fmt.Errorf("task root abs: %w", err)
	}
	cfgAbs := wsAbs
	if configRoot != "" {
		cfgAbs, err = filepath.Abs(configRoot)
		if err != nil {
			return nil, fmt.Errorf("config root abs: %w", err)
		}
	}

	out := make([]*Skill, 0, len(paths))
	for _, p := range paths {
		abs, root, err := resolveSkillPath(wsAbs, cfgAbs, p)
		if err != nil {
			return nil, err
		}
		sk, err := LoadSkill(abs, root)
		if err != nil {
			return nil, err
		}
		// When the skill lives outside the task workspace, the relative
		// Dir computed in LoadSkill (from `root`) makes sense only if the
		// agent knows that root. Easier: switch Dir to absolute so the
		// agent always has a usable path for bash invocations of bundled
		// scripts.
		if root != wsAbs {
			sk.Dir = filepath.Dir(abs)
		}
		out = append(out, sk)
	}
	return out, nil
}

// resolveSkillPath tries taskRoot first, then configRoot. Either root can
// reject paths that try to `..` out of it — a skill must end up under one
// of the two registered roots.
func resolveSkillPath(taskRoot, configRoot, p string) (abs, root string, err error) {
	for _, candidate := range []string{taskRoot, configRoot} {
		if candidate == "" {
			continue
		}
		full := filepath.Clean(filepath.Join(candidate, p))
		rel, relErr := filepath.Rel(candidate, full)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if _, statErr := os.Stat(full); statErr == nil {
			return full, candidate, nil
		}
	}
	if taskRoot == configRoot || configRoot == "" {
		return "", "", fmt.Errorf("skill path %q not found under task workspace %s", p, taskRoot)
	}
	return "", "", fmt.Errorf("skill path %q not found under task workspace (%s) or config dir (%s)", p, taskRoot, configRoot)
}

// FormatAvailableSkillsSection returns the Available Skills block to append
// to the system prompt. Empty input yields the empty string so the caller
// can unconditionally concatenate. The format is intentionally compact —
// long descriptions inflate every-turn context without proportional value.
func FormatAvailableSkillsSection(skills []*Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Available Skills (call load_skill with the skill name to fetch full instructions):\n")
	for _, sk := range skills {
		fmt.Fprintf(&b, "  - %s: %s\n", sk.Name, oneLine(sk.Description))
	}
	return b.String()
}

// oneLine collapses a description to a single line so the available-skills
// section stays a tight catalog. Skill bodies (the load_skill response)
// preserve original formatting.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}
