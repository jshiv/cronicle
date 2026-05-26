package cronicle

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/go-git/go-git/v6/plumbing/client"
	"github.com/jshiv/cronicle/pkg/agent"
	"github.com/jshiv/cronicle/pkg/exec"
)

// MaxToolOutputBytes caps how much bash output we feed back to the model in
// a single tool_result. Outputs exceeding this are middle-truncated (head
// + tail preserved, middle dropped with a marker). Matches Claude Code's
// 30K default — keeps the next-turn context bounded against runaway commands.
const MaxToolOutputBytes = 30 * 1024

// ansiCSI matches ANSI CSI escape sequences (color codes, cursor moves).
// Stripped from bash output before truncation so the byte count reflects
// real content, not control bytes the model can't use anyway.
var ansiCSI = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// BashTool implements agent.Tool by wrapping pkg/exec.ExecuteWithStream so
// that bash invoked by an agent runs through the exact same execution path
// as a shell task: cwd=workspace, env, stream-aware writers. Output flows
// to the same stdoutW used by the agent's pretty-mode renderer when set,
// otherwise it's just captured into the result string.
type BashTool struct {
	Workspace string
	Env       []string
	StdoutW   io.Writer // optional; for live pretty-mode streaming of bash output
	StderrW   io.Writer // optional
}

// Name is "bash" (matches Anthropic-defined tool name).
func (b *BashTool) Name() string { return "bash" }

// Definition returns the SDK's bash_20250124 tool definition.
func (b *BashTool) Definition() anthropic.ToolUnionParam {
	return anthropic.ToolUnionParam{
		OfBashTool20250124: &anthropic.ToolBash20250124Param{},
	}
}

// Execute runs the requested command and returns a tool_result string the
// model can read on the next turn. Format:
//
//	exit_code: <n>          (only when non-zero)
//	<stdout>
//	--- stderr ---           (only when stderr non-empty)
//	<stderr>
//
// The combined output is ANSI-stripped and middle-truncated to
// MaxToolOutputBytes so a runaway command can't pollute the next turn's
// context. Cancellation propagates: when ctx is done (typically because the
// agent's wallclock fired), the bash process group is signalled SIGTERM and
// then SIGKILL.
func (b *BashTool) Execute(ctx context.Context, input json.RawMessage) (string, bool) {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return fmt.Sprintf("Error: invalid bash input: %v", err), true
	}
	if args.Command == "" {
		return "Error: empty command", true
	}

	cmd := []string{"/bin/sh", "-c", args.Command}
	// Scrubbed env: an agent-emitted `env`/`printenv` must not be able to
	// read worker-process state. The bash subprocess sees only the
	// allowlisted parent vars (PATH/HOME/USER/locale/SHELL) plus b.Env —
	// which the task dispatch path has already populated with task.Env
	// after $secret.NAME expansion. Secrets the task didn't explicitly
	// list never reach this process.
	res := exec.ExecuteScrubbedStreamContext(ctx, cmd, b.Workspace, b.Env, b.StdoutW, b.StderrW)

	stdout := stripANSI(res.Stdout)
	stderr := stripANSI(res.Stderr)

	var sb strings.Builder
	if res.ExitStatus != 0 {
		fmt.Fprintf(&sb, "exit_code: %d\n", res.ExitStatus)
	}
	if stdout != "" {
		sb.WriteString(stdout)
	}
	if stderr != "" {
		sb.WriteString("\n--- stderr ---\n")
		sb.WriteString(stderr)
	}
	if sb.Len() == 0 {
		sb.WriteString("(no output)")
	}
	return truncateOutput(sb.String(), MaxToolOutputBytes), res.ExitStatus != 0
}

// stripANSI removes ANSI CSI escape sequences from s.
func stripANSI(s string) string {
	if !strings.ContainsRune(s, 0x1b) {
		return s
	}
	return ansiCSI.ReplaceAllString(s, "")
}

// truncateOutput returns s unchanged when it fits in max bytes; otherwise it
// keeps the first and last halves of max with a marker noting how many
// bytes were dropped from the middle. Middle-keep matters for shell
// commands where both the start (the command framing) and the end (the
// final lines, exit context) are usually the most informative.
func truncateOutput(s string, max int) string {
	if len(s) <= max {
		return s
	}
	dropped := len(s) - max
	half := max / 2
	return fmt.Sprintf("%s\n\n... [%d bytes truncated] ...\n\n%s",
		s[:half], dropped, s[len(s)-half:])
}

// TextEditorTool implements agent.Tool for str_replace_based_edit_tool. All
// path operations are confined to Workspace via resolveInWorkspace; absolute
// paths or `..` traversal that escape the workspace are rejected. Supported
// commands match Anthropic's text_editor_20250728 schema:
//
//	view         — show file contents (with line numbers) or directory listing
//	create       — write a new file; refuses to overwrite
//	str_replace  — replace exact unique match in a file
//	insert       — splice text at a 0-based line position
//
// view output is ANSI-stripped and middle-truncated to MaxToolOutputBytes,
// same as bash output, so a `view` of a giant generated file can't poison
// the next turn's context.
type TextEditorTool struct {
	Workspace string
	// AllowedRoots is the set of additional dirs (besides Workspace) that
	// text_editor will accept paths under. Used to grant access to the
	// schedule-scoped scratch dir when the task's main workspace is a
	// cloned repo — the cross-task context pattern (${scratch}) needs the
	// composer to read crawler outputs that live outside the workspace.
	// Empty means "workspace only" (the historical behavior).
	AllowedRoots []string
}

func (t *TextEditorTool) Name() string { return "str_replace_based_edit_tool" }

func (t *TextEditorTool) Definition() anthropic.ToolUnionParam {
	return anthropic.ToolUnionParam{
		OfTextEditor20250728: &anthropic.ToolTextEditor20250728Param{},
	}
}

func (t *TextEditorTool) Execute(ctx context.Context, input json.RawMessage) (string, bool) {
	var args struct {
		Command    string `json:"command"`
		Path       string `json:"path"`
		FileText   string `json:"file_text,omitempty"`
		OldStr     string `json:"old_str,omitempty"`
		NewStr     string `json:"new_str,omitempty"`
		InsertLine int    `json:"insert_line,omitempty"`
		ViewRange  []int  `json:"view_range,omitempty"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return fmt.Sprintf("Error: invalid editor input: %v", err), true
	}
	abs, err := t.resolvePath(args.Path)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	switch args.Command {
	case "view":
		return t.view(abs, args.ViewRange)
	case "create":
		return t.create(abs, args.FileText)
	case "str_replace":
		return t.strReplace(abs, args.OldStr, args.NewStr)
	case "insert":
		return t.insert(abs, args.InsertLine, args.NewStr)
	default:
		return fmt.Sprintf("Error: unknown editor command %q", args.Command), true
	}
}

func (t *TextEditorTool) view(absPath string, viewRange []int) (string, bool) {
	info, err := os.Stat(absPath)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	if info.IsDir() {
		entries, err := os.ReadDir(absPath)
		if err != nil {
			return fmt.Sprintf("Error: %v", err), true
		}
		var sb strings.Builder
		rel, _ := filepath.Rel(t.Workspace, absPath)
		if rel == "" {
			rel = "."
		}
		fmt.Fprintf(&sb, "Directory %s:\n", rel)
		for _, e := range entries {
			marker := ""
			if e.IsDir() {
				marker = "/"
			}
			fmt.Fprintf(&sb, "  %s%s\n", e.Name(), marker)
		}
		return truncateOutput(sb.String(), MaxToolOutputBytes), false
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	if isBinary(data) {
		return fmt.Sprintf("Error: %s appears to be a binary file", filepath.Base(absPath)), true
	}
	lines := strings.Split(string(data), "\n")
	start, end := 1, len(lines)
	if len(viewRange) == 2 {
		if viewRange[0] >= 1 {
			start = viewRange[0]
		}
		if viewRange[1] >= 1 && viewRange[1] <= len(lines) {
			end = viewRange[1]
		}
	}
	var sb strings.Builder
	for i := start - 1; i < end && i < len(lines); i++ {
		fmt.Fprintf(&sb, "%6d\t%s\n", i+1, lines[i])
	}
	return truncateOutput(stripANSI(sb.String()), MaxToolOutputBytes), false
}

func (t *TextEditorTool) create(absPath, content string) (string, bool) {
	if _, err := os.Stat(absPath); err == nil {
		return fmt.Sprintf("Error: %s already exists; use str_replace or insert to modify",
			filepath.Base(absPath)), true
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return fmt.Sprintf("Created %s (%d bytes)", filepath.Base(absPath), len(content)), false
}

func (t *TextEditorTool) strReplace(absPath, oldStr, newStr string) (string, bool) {
	if oldStr == "" {
		return "Error: old_str must not be empty", true
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	content := string(data)
	count := strings.Count(content, oldStr)
	if count == 0 {
		return fmt.Sprintf("Error: old_str not found in %s", filepath.Base(absPath)), true
	}
	if count > 1 {
		return fmt.Sprintf("Error: old_str matches %d times in %s; must be unique",
			count, filepath.Base(absPath)), true
	}
	newContent := strings.Replace(content, oldStr, newStr, 1)
	if err := os.WriteFile(absPath, []byte(newContent), 0o644); err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return fmt.Sprintf("Edited %s", filepath.Base(absPath)), false
}

func (t *TextEditorTool) insert(absPath string, insertLine int, newStr string) (string, bool) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	lines := strings.Split(string(data), "\n")
	if insertLine < 0 || insertLine > len(lines) {
		return fmt.Sprintf("Error: insert_line %d out of range (file has %d lines)",
			insertLine, len(lines)), true
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:insertLine]...)
	out = append(out, strings.Split(newStr, "\n")...)
	out = append(out, lines[insertLine:]...)
	if err := os.WriteFile(absPath, []byte(strings.Join(out, "\n")), 0o644); err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return fmt.Sprintf("Inserted at line %d in %s", insertLine, filepath.Base(absPath)), false
}

// resolvePath gates each text_editor operation. It accepts paths under the
// primary workspace OR any of the additional AllowedRoots (scratch dir is
// the canonical extra). The first root that contains the resolved path
// wins; rejection means no root matched. Path-traversal `..` is checked
// against each root, so the agent can't break out via the additional
// roots either.
func (t *TextEditorTool) resolvePath(p string) (string, error) {
	if t == nil || t.Workspace == "" {
		return resolveInWorkspace(t.Workspace, p)
	}
	if abs, err := resolveInWorkspace(t.Workspace, p); err == nil {
		return abs, nil
	}
	for _, root := range t.AllowedRoots {
		if root == "" {
			continue
		}
		if abs, err := resolveInWorkspace(root, p); err == nil {
			return abs, nil
		}
	}
	return "", fmt.Errorf("path %s outside workspace and allowed roots", p)
}

// resolveInWorkspace turns a model-supplied path (relative or absolute) into
// an absolute path *guaranteed* to live under workspace, or an error. Used
// by every text_editor operation as the workspace-containment gate.
func resolveInWorkspace(workspace, p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("path required")
	}
	wsAbs, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("workspace abs: %w", err)
	}
	var abs string
	if filepath.IsAbs(p) {
		abs = p
	} else {
		abs = filepath.Join(wsAbs, p)
	}
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(wsAbs, abs)
	if err != nil {
		return "", fmt.Errorf("path %s outside workspace", p)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %s escapes workspace", p)
	}
	return abs, nil
}

// isBinary reports whether the data looks like binary (any null byte in the
// first 4KB). Cheap heuristic that catches most binary files (executables,
// images, archives) without false positives on normal text/UTF-8.
func isBinary(data []byte) bool {
	sample := data
	if len(sample) > 4096 {
		sample = sample[:4096]
	}
	for _, b := range sample {
		if b == 0 {
			return true
		}
	}
	return false
}

// WebSearchTool exposes Anthropic's server-side web_search tool. The model
// invokes it; Anthropic runs it on their infrastructure and returns results
// as content blocks in the assistant message. Execute is never called by
// the cronicle dispatcher because server_tool_use blocks aren't routed
// through the client-side tool map — defining Execute here is a defensive
// stub for the agent.Tool contract.
type WebSearchTool struct{}

func (WebSearchTool) Name() string { return "web_search" }

func (WebSearchTool) Definition() anthropic.ToolUnionParam {
	return anthropic.ToolUnionParam{
		OfWebSearchTool20260209: &anthropic.WebSearchTool20260209Param{
			// "direct" lets the model invoke this tool without needing
			// the code_execution wrapper. Required for models without
			// programmatic tool calling (e.g. claude-haiku-4-5).
			AllowedCallers: []string{"direct"},
		},
	}
}

func (WebSearchTool) Execute(_ context.Context, _ json.RawMessage) (string, bool) {
	return "Error: web_search is a server-side tool; should not be dispatched client-side", true
}

// WebFetchTool exposes Anthropic's server-side web_fetch tool. Same model
// as WebSearchTool — server-side, no client dispatch.
type WebFetchTool struct{}

func (WebFetchTool) Name() string { return "web_fetch" }

func (WebFetchTool) Definition() anthropic.ToolUnionParam {
	return anthropic.ToolUnionParam{
		OfWebFetchTool20260309: &anthropic.WebFetchTool20260309Param{
			AllowedCallers: []string{"direct"},
		},
	}
}

func (WebFetchTool) Execute(_ context.Context, _ json.RawMessage) (string, bool) {
	return "Error: web_fetch is a server-side tool; should not be dispatched client-side", true
}

// defaultAgentToolNames is the tool universe an agent gets when the HCL
// `tools` field is omitted. Local-only tools by default — bash,
// text_editor, and git run inside the task workspace and don't add per-call
// cost beyond the model itself.
//
// git is included by default because cronicle ships with go-git embedded;
// the agent gets read+write git access without the host needing the git
// CLI installed. This preserves cronicle's "single binary on a bare
// machine" property for any agent task that already has a `repo` block.
//
// web_search and web_fetch are deliberately excluded: they bill on
// Anthropic's side per call, which is surprising for an unattended cron
// job that "just runs" with the default toolkit. Add them explicitly:
//
//	tools = ["bash", "text_editor", "git", "web_search", "web_fetch"]
func defaultAgentToolNames() []string {
	return []string{"bash", "text_editor", "git"}
}

// buildAgentTools converts the HCL `tools` field into a slice of agent.Tool
// implementations bound to the given workspace and stream writer (when in
// pretty streaming mode). Empty/nil names defaults to the full native
// toolkit (defaultAgentToolNames). Unknown names are filtered out (Validate
// has already rejected them at parse time, so this is defensive).
func buildAgentTools(names []string, workspace string, env []string, gitAuth []client.Option, scratchDir string, w io.Writer) []agent.Tool {
	if len(names) == 0 {
		names = defaultAgentToolNames()
	}
	var editorAllowed []string
	if scratchDir != "" {
		editorAllowed = append(editorAllowed, scratchDir)
	}
	out := make([]agent.Tool, 0, len(names))
	for _, name := range names {
		switch name {
		case "bash":
			out = append(out, &BashTool{
				Workspace: workspace,
				Env:       env,
				StdoutW:   w,
				StderrW:   w,
			})
		case "text_editor":
			out = append(out, &TextEditorTool{
				Workspace:    workspace,
				AllowedRoots: editorAllowed,
			})
		case "git":
			out = append(out, &GitTool{
				Workspace: workspace,
				Auth:      gitAuth,
			})
		case "web_search":
			out = append(out, WebSearchTool{})
		case "web_fetch":
			out = append(out, WebFetchTool{})
		}
	}
	return out
}
