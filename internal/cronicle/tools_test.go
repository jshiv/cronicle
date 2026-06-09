package cronicle

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stripANSI strips ANSI CSI sequences. Tested with synthetic color
// sequences (\x1b[31m red, \x1b[0m reset) that are common in tool output.
func TestStripANSI(t *testing.T) {
	in := "\x1b[31mred\x1b[0m plain \x1b[1;33myellow\x1b[0m"
	got := stripANSI(in)
	want := "red plain yellow"
	if got != want {
		t.Fatalf("stripANSI: got %q, want %q", got, want)
	}
	// Idempotent on plain text — and faster (early-out path).
	if got := stripANSI("no ansi here"); got != "no ansi here" {
		t.Fatalf("stripANSI plain: got %q", got)
	}
}

// truncateOutput keeps short input verbatim and middle-truncates long input
// with a marker reporting bytes dropped.
func TestTruncateOutput(t *testing.T) {
	short := "abc"
	if got := truncateOutput(short, 100); got != short {
		t.Fatalf("short: got %q, want %q", got, short)
	}
	long := strings.Repeat("a", 200) + strings.Repeat("b", 200)
	got := truncateOutput(long, 100)
	if !strings.Contains(got, "[300 bytes truncated]") {
		t.Fatalf("truncateOutput: missing dropped-bytes marker: %q", got)
	}
	if !strings.HasPrefix(got, strings.Repeat("a", 50)) {
		t.Fatalf("truncateOutput: missing leading head")
	}
	if !strings.HasSuffix(got, strings.Repeat("b", 50)) {
		t.Fatalf("truncateOutput: missing trailing tail")
	}
}

// isBinary flags any null byte in the first 4KB. Text with high-bit chars
// (UTF-8 multi-byte) must still report false.
func TestIsBinary(t *testing.T) {
	if isBinary([]byte("just plain text")) {
		t.Fatalf("plain text reported binary")
	}
	if !isBinary([]byte{'a', 'b', 0, 'c'}) {
		t.Fatalf("null byte not detected")
	}
	// UTF-8 multibyte should not trip the heuristic.
	utf8 := []byte("café résumé naïve — π 🚀")
	if isBinary(utf8) {
		t.Fatalf("utf8 reported binary")
	}
}

// resolveInWorkspace blocks `..` traversal and absolute paths outside the
// workspace, allows relative paths inside, and resolves symlinks before
// the containment check so an agent can't escape via a planted symlink.
func TestResolveInWorkspace(t *testing.T) {
	ws := t.TempDir()
	// Resolve the workspace's own symlinks so HasPrefix checks below
	// compare against the canonical form. On macOS, t.TempDir() returns
	// a /var/folders/... path that resolves to /private/var/folders/...
	wsResolved, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatalf("workspace eval: %v", err)
	}

	abs, err := resolveInWorkspace(ws, "sub/file.txt")
	if err != nil {
		t.Fatalf("relative inside: %v", err)
	}
	if !strings.HasPrefix(abs, wsResolved) {
		t.Fatalf("resolved path %q outside workspace %q", abs, wsResolved)
	}

	if _, err := resolveInWorkspace(ws, "../escape.txt"); err == nil {
		t.Fatalf("`..` traversal allowed")
	}

	if _, err := resolveInWorkspace(ws, "/etc/passwd"); err == nil {
		t.Fatalf("absolute outside workspace allowed")
	}
	if _, err := resolveInWorkspace(ws, ""); err == nil {
		t.Fatalf("empty path allowed")
	}
}

// resolveInWorkspace rejects paths that traverse OUT of the workspace
// via a symlink. An attacker who can write inside the workspace (the
// agent's bash tool) must not be able to plant a link and then use
// text_editor to read or write the link target.
func TestResolveInWorkspace_SymlinkEscape(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	// Plant a target file in the outside directory and a symlink to it
	// inside the workspace.
	target := filepath.Join(outside, "stolen.txt")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(ws, "loot")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := resolveInWorkspace(ws, "loot"); err == nil {
		t.Fatalf("symlink escape to outside-workspace target was allowed")
	}
}

// A symlink that points to a sibling INSIDE the workspace is benign and
// must continue to resolve cleanly.
func TestResolveInWorkspace_SymlinkInside(t *testing.T) {
	ws := t.TempDir()
	wsResolved, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatalf("workspace eval: %v", err)
	}
	target := filepath.Join(ws, "real.txt")
	if err := os.WriteFile(target, []byte("ok"), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(ws, "alias")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	abs, err := resolveInWorkspace(ws, "alias")
	if err != nil {
		t.Fatalf("intra-workspace symlink rejected: %v", err)
	}
	if !strings.HasPrefix(abs, wsResolved) {
		t.Fatalf("resolved path %q escapes workspace %q", abs, wsResolved)
	}
}

// Create operations must work for a leaf path that doesn't exist yet —
// resolveInWorkspace can't EvalSymlinks a non-existent file, so it must
// walk up to the nearest existing ancestor.
func TestResolveInWorkspace_NonExistentLeaf(t *testing.T) {
	ws := t.TempDir()
	abs, err := resolveInWorkspace(ws, "new-dir/new-file.txt")
	if err != nil {
		t.Fatalf("create-new path rejected: %v", err)
	}
	if !strings.Contains(abs, "new-dir/new-file.txt") {
		t.Fatalf("leaf lost in resolution: got %q", abs)
	}
}

// TestTextEditorTool_NilOrEmptyWorkspaceRejected: the M15 fix.
// Previously, a nil receiver or empty Workspace fell through to
// resolveInWorkspace("", p) which used filepath.Abs("") → process CWD,
// silently breaking workspace containment. We now reject explicitly.
func TestTextEditorTool_NilOrEmptyWorkspaceRejected(t *testing.T) {
	// Nil receiver
	if _, err := (*TextEditorTool)(nil).resolvePath("a.txt"); err == nil {
		t.Errorf("nil receiver should return error, got nil")
	}
	// Empty workspace
	tool := &TextEditorTool{Workspace: ""}
	if _, err := tool.resolvePath("a.txt"); err == nil {
		t.Errorf("empty Workspace should return error, got nil")
	}
}

// defaultAgentToolNames is local-only by default (bash + text_editor + git).
// web_search / web_fetch bill per call and are opt-in — adding them by
// default would surprise unattended cron jobs.
func TestDefaultAgentToolNamesContents(t *testing.T) {
	names := defaultAgentToolNames()
	want := map[string]bool{"bash": true, "text_editor": true, "git": true}
	if len(names) != len(want) {
		t.Fatalf("default tools count: got %d, want %d (%v)", len(names), len(want), names)
	}
	for _, n := range names {
		if !want[n] {
			t.Fatalf("unexpected default tool %q (web_search and web_fetch should be opt-in)", n)
		}
	}
}

// buildAgentTools defaults to the native universe when names is empty.
// Empty list and nil should both expand. Explicit list narrows.
func TestBuildAgentTools(t *testing.T) {
	tools := buildAgentTools(nil, t.TempDir(), nil, nil, "", nil)
	if len(tools) != len(defaultAgentToolNames()) {
		t.Fatalf("nil tools: got %d, want %d", len(tools), len(defaultAgentToolNames()))
	}
	tools = buildAgentTools([]string{}, t.TempDir(), nil, nil, "", nil)
	if len(tools) != len(defaultAgentToolNames()) {
		t.Fatalf("empty slice tools: got %d, want %d", len(tools), len(defaultAgentToolNames()))
	}
	tools = buildAgentTools([]string{"bash"}, t.TempDir(), nil, nil, "", nil)
	if len(tools) != 1 {
		t.Fatalf("explicit narrow: got %d tools, want 1", len(tools))
	}
	if tools[0].Name() != "bash" {
		t.Fatalf("explicit narrow: got %q, want bash", tools[0].Name())
	}
}

// TextEditorTool view + create + str_replace + insert with workspace
// containment. Exercises the happy paths plus the unique-match guard
// on str_replace.
func TestTextEditorTool(t *testing.T) {
	ws := t.TempDir()
	tool := &TextEditorTool{Workspace: ws}
	ctx := context.Background()

	// create
	out, isErr := tool.Execute(ctx, json.RawMessage(`{"command":"create","path":"a.txt","file_text":"line1\nline2\nline3\n"}`))
	if isErr {
		t.Fatalf("create: %s", out)
	}

	// view
	out, isErr = tool.Execute(ctx, json.RawMessage(`{"command":"view","path":"a.txt"}`))
	if isErr {
		t.Fatalf("view: %s", out)
	}
	if !strings.Contains(out, "line2") {
		t.Fatalf("view missing content: %q", out)
	}

	// str_replace — happy path
	out, isErr = tool.Execute(ctx, json.RawMessage(`{"command":"str_replace","path":"a.txt","old_str":"line2","new_str":"LINE_TWO"}`))
	if isErr {
		t.Fatalf("str_replace: %s", out)
	}

	// str_replace — non-unique match should fail safely
	_, _ = tool.Execute(ctx, json.RawMessage(`{"command":"create","path":"dup.txt","file_text":"x\nx\n"}`))
	out, isErr = tool.Execute(ctx, json.RawMessage(`{"command":"str_replace","path":"dup.txt","old_str":"x","new_str":"y"}`))
	if !isErr {
		t.Fatalf("str_replace should reject non-unique match, got: %q", out)
	}

	// insert at line 1
	out, isErr = tool.Execute(ctx, json.RawMessage(`{"command":"insert","path":"a.txt","insert_line":1,"new_str":"INSERTED"}`))
	if isErr {
		t.Fatalf("insert: %s", out)
	}
	body, err := os.ReadFile(filepath.Join(ws, "a.txt"))
	if err != nil {
		t.Fatalf("read after insert: %v", err)
	}
	if !strings.Contains(string(body), "INSERTED") {
		t.Fatalf("insert didn't land: %q", body)
	}

	// view a directory
	out, isErr = tool.Execute(ctx, json.RawMessage(`{"command":"view","path":"."}`))
	if isErr {
		t.Fatalf("view dir: %s", out)
	}
	if !strings.Contains(out, "a.txt") {
		t.Fatalf("view dir missing file: %q", out)
	}

	// `..` escape blocked
	out, isErr = tool.Execute(ctx, json.RawMessage(`{"command":"view","path":"../etc/passwd"}`))
	if !isErr {
		t.Fatalf("`..` escape allowed: %q", out)
	}
}

// BashTool runs a real shell command and captures output. Skipped on
// hostile envs by checking /bin/sh exists.
func TestBashTool(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}
	ws := t.TempDir()
	tool := &BashTool{Workspace: ws}
	ctx := context.Background()

	out, isErr := tool.Execute(ctx, json.RawMessage(`{"command":"echo hello && echo world"}`))
	if isErr {
		t.Fatalf("bash succeeded path returned isErr: %s", out)
	}
	if !strings.Contains(out, "hello") || !strings.Contains(out, "world") {
		t.Fatalf("bash output missing: %q", out)
	}

	out, isErr = tool.Execute(ctx, json.RawMessage(`{"command":"exit 7"}`))
	if !isErr {
		t.Fatalf("non-zero exit not flagged as error")
	}
	if !strings.Contains(out, "exit_code: 7") {
		t.Fatalf("missing exit_code line: %q", out)
	}

	// Empty command is rejected.
	_, isErr = tool.Execute(ctx, json.RawMessage(`{"command":""}`))
	if !isErr {
		t.Fatalf("empty command accepted")
	}

	// Invalid JSON is rejected.
	_, isErr = tool.Execute(ctx, json.RawMessage(`not-json`))
	if !isErr {
		t.Fatalf("invalid JSON accepted")
	}
}
