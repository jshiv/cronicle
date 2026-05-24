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

// TestLiveSinkPrettyHandler_ColorByDefault: the SSE wire emits ANSI for
// pretty mode regardless of the producer's TTY state — the consumer is
// xterm.js, which can always render color. This is the entire reason
// the live wire's color decision is decoupled from stdout's. Used to
// require the separate LiveFormatPrettyColor; now baked in.
func TestLiveSinkPrettyHandler_ColorByDefault(t *testing.T) {
	// Simulate headless (non-TTY) producer state.
	prev := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = prev }()
	// Default ColorMode is auto; shouldColorLive returns true unless
	// NO_COLOR is in env. Tests don't set NO_COLOR so auto → color on.
	prevMode := currentColorMode
	currentColorMode = ColorModeAuto
	defer func() { currentColorMode = prevMode }()

	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "shell run start", 0)
	rec.AddAttrs(
		slog.String("entry_type", "shell_run_start"),
		slog.String("run_id", "R1"),
		slog.String("schedule", "daily"),
		slog.String("task", "cleanup"),
		slog.String("command", "echo hello && rm -rf /tmp/cache"),
	)

	pretty := newLiveEncoder(LiveFormatPretty)(rec)
	// pretty-color is the deprecated alias — SetLiveFormat normalises it,
	// but newLiveEncoder is called directly here; both should color.
	alias := newLiveEncoder(LiveFormatPrettyColor)(rec)

	for name, b := range map[string][]byte{"pretty": pretty, "pretty-color alias": alias} {
		if !bytes.Contains(b, []byte("\x1b[")) {
			t.Fatalf("%s: expected ANSI on the live wire; got:\n%q", name, b)
		}
		if !bytes.Contains(b, []byte("echo hello && rm -rf /tmp/cache")) {
			t.Fatalf("%s: missing resolved command in output\n%s", name, b)
		}
	}
}

// TestLiveSinkPrettyHandler_NoColorEnv: with NO_COLOR set, the live wire
// emits monochrome — even though the wire-default is color-on. Matches
// the no-color.org convention in auto mode.
func TestLiveSinkPrettyHandler_NoColorEnv(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	prevMode := currentColorMode
	currentColorMode = ColorModeAuto
	defer func() { currentColorMode = prevMode }()

	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "shell run start", 0)
	rec.AddAttrs(
		slog.String("entry_type", "shell_run_start"),
		slog.String("run_id", "R1"),
		slog.String("schedule", "daily"),
		slog.String("task", "cleanup"),
		slog.String("command", "echo hello"),
	)
	got := newLiveEncoder(LiveFormatPretty)(rec)
	if bytes.Contains(got, []byte("\x1b[")) {
		t.Fatalf("NO_COLOR set: expected monochrome on the live wire; got:\n%q", got)
	}
}

// TestLiveSinkPrettyHandler_RestoresGlobal: after the handler returns,
// the global color.NoColor must be restored so a concurrent stdout pretty
// handler isn't permanently flipped into color mode.
func TestLiveSinkPrettyHandler_RestoresGlobal(t *testing.T) {
	prev := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = prev }()
	prevMode := currentColorMode
	currentColorMode = ColorModeAuto
	defer func() { currentColorMode = prevMode }()

	h := &liveSinkPrettyHandler{
		fallback: newTintHandler(&bytes.Buffer{}),
		out:      &bytes.Buffer{},
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
