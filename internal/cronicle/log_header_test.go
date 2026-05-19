package cronicle

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/fatih/color"
)

// TestWriteShellRunHeader_FullCommand: the previous renderer truncated
// the resolved command to 60 chars in the header line, hiding what
// actually got executed when commands referenced ${date}/${path}/${scratch}.
// The new layout puts the full resolved command on its own `command:` line
// so the SSE consumer can read it end-to-end.
func TestWriteShellRunHeader_FullCommand(t *testing.T) {
	cmd := "rm -rf /tmp/cache/2026-05-19 && touch /tmp/cleanup.done && echo done"
	var b bytes.Buffer
	WriteShellRunHeader(&b, "daily", "cleanup", cmd)
	out := b.String()
	if !strings.Contains(out, "shell run · schedule=daily · task=cleanup") {
		t.Fatalf("missing header line:\n%s", out)
	}
	if !strings.Contains(out, "command: "+cmd) {
		t.Fatalf("resolved command not emitted in full; got:\n%s", out)
	}
	if strings.Contains(out, "…") {
		t.Fatalf("command was truncated (saw ellipsis):\n%s", out)
	}
}

// TestWriteAgentRunHeader_PromptAndTools: the agent header must include
// the RESOLVED prompt and tools list so the SSE stream tells the whole
// story of what got dispatched. ${date}-style template expansion happens
// in execAgent before the slog record fires; the header just echoes it.
func TestWriteAgentRunHeader_PromptAndTools(t *testing.T) {
	prompt := "Compose a 3-bullet brief for May 19, 2026. Search the web for top news items."
	system := "You are a concise news summarizer."
	tools := []string{"web_search", "web_fetch"}
	mcps := []string{"github: npx -y @modelcontextprotocol/server-github"}
	var b bytes.Buffer
	WriteAgentRunHeader(&b, "morning_brief", "brief", "claude-haiku-4-5", nil,
		prompt, system, tools, mcps)
	out := b.String()
	for _, want := range []string{
		"agent run · schedule=morning_brief · task=brief · model=claude-haiku-4-5",
		"prompt: " + prompt,
		"system: " + system,
		"tools: web_search, web_fetch",
		"mcp: github: npx -y @modelcontextprotocol/server-github",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in header:\n%s", want, out)
		}
	}
}

// TestWriteAgentRunHeader_OmitsEmpty: when prompt/system/tools/mcps are
// all empty (e.g. skill-only run with no prompt body, no tools), the
// header collapses back to the original three-line block — no stray
// "prompt:" / "system:" / "tools:" rows.
func TestWriteAgentRunHeader_OmitsEmpty(t *testing.T) {
	var b bytes.Buffer
	WriteAgentRunHeader(&b, "s", "t", "claude-x", nil, "", "", nil, nil)
	out := b.String()
	for _, unwanted := range []string{"prompt:", "system:", "tools:", "mcp:"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("expected no %q row when value is empty; got:\n%s", unwanted, out)
		}
	}
}

// TestLiveSinkPrettyHandler_ForceColor: with the global color.NoColor=true
// (the case when cronicled runs as a headless service — no TTY on stdout),
// the default pretty handler emits monochrome bytes. PrettyColor mode must
// emit ANSI escapes anyway so the xterm.js consumer can render them.
func TestLiveSinkPrettyHandler_ForceColor(t *testing.T) {
	// Simulate headless (non-TTY) producer state.
	prev := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = prev }()

	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "shell run start", 0)
	rec.AddAttrs(
		slog.String("entry_type", "shell_run_start"),
		slog.String("run_id", "R1"),
		slog.String("schedule", "daily"),
		slog.String("task", "cleanup"),
		slog.String("command", "echo hello && rm -rf /tmp/cache"),
	)

	plainEnc := newLiveEncoder(LiveFormatPretty)
	colorEnc := newLiveEncoder(LiveFormatPrettyColor)

	plain := plainEnc(rec)
	colored := colorEnc(rec)

	if bytes.Contains(plain, []byte("\x1b[")) {
		t.Fatalf("LiveFormatPretty should not emit ANSI when color.NoColor=true; got:\n%q", plain)
	}
	if !bytes.Contains(colored, []byte("\x1b[")) {
		t.Fatalf("LiveFormatPrettyColor must emit ANSI even when color.NoColor=true; got:\n%q", colored)
	}
	// Both encodings should contain the resolved command — color choice
	// changes the bytes around it, not the content.
	if !bytes.Contains(plain, []byte("echo hello && rm -rf /tmp/cache")) ||
		!bytes.Contains(colored, []byte("echo hello && rm -rf /tmp/cache")) {
		t.Fatalf("resolved command missing from one of the encodings\nplain:\n%s\ncolored:\n%s", plain, colored)
	}
}

// TestLiveSinkPrettyHandler_ForceColor_RestoresGlobal: after a forced-
// color render, the global color.NoColor must return to its prior value
// so a stdout pretty handler that ran concurrently isn't permanently
// flipped into color mode.
func TestLiveSinkPrettyHandler_ForceColor_RestoresGlobal(t *testing.T) {
	prev := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = prev }()

	h := &liveSinkPrettyHandler{
		fallback:   newTintHandler(&bytes.Buffer{}),
		out:        &bytes.Buffer{},
		forceColor: true,
	}
	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "dim line", 0)
	rec.AddAttrs(
		slog.String("entry_type", "lifecycle"),
		slog.String("run_id", "R1"),
	)
	if err := h.Handle(context.Background(), rec); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if color.NoColor != true {
		t.Fatalf("global color.NoColor not restored: got %v, want true", color.NoColor)
	}
}
