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

// TestWriteShellRunHeader_FullCommand: the header shows the full resolved
// command inside the frame, never truncated.
func TestWriteShellRunHeader_FullCommand(t *testing.T) {
	argv := []string{"sh", "-c", "rm -rf /tmp/cache/2026-05-19 && touch /tmp/cleanup.done && echo done"}
	var b bytes.Buffer
	WriteShellRunHeader(&b, "daily", "cleanup", argv)
	out := b.String()
	if !strings.Contains(out, "╭─ cleanup  shell · daily") {
		t.Fatalf("missing header line:\n%s", out)
	}
	if !strings.Contains(out, "│ $ sh -c 'rm -rf /tmp/cache/2026-05-19 && touch /tmp/cleanup.done && echo done'\n") {
		t.Fatalf("command row missing or truncated:\n%s", out)
	}
}

// TestWriteShellRunFooter_Rerun: a failed block ends with the pasteable
// command alone on a flush-left line; a passing block doesn't.
func TestWriteShellRunFooter_Rerun(t *testing.T) {
	prev := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = prev }()

	rerun := shellQuote([]string{"bash", "-c", "echo hi\nexit 2"})
	var b bytes.Buffer
	WriteShellRunFooter(&b, "load", rerun, 2, 40, "")
	want := "╰─ load  failed · exit 2 · 40ms\n↻ rerun load:\n" + rerun + "\n\n"
	if b.String() != want {
		t.Fatalf("got  %q\nwant %q", b.String(), want)
	}

	b.Reset()
	WriteShellRunFooter(&b, "load", rerun, 0, 40, "")
	if strings.Contains(b.String(), "rerun") {
		t.Errorf("passing block shows a rerun line:\n%s", b.String())
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
		prompt, system, "", tools, mcps)
	out := b.String()
	for _, want := range []string{
		"╭─ brief  agent · morning_brief · claude-haiku-4-5",
		"prompt " + prompt,
		"system " + system,
		"tools  web_search, web_fetch",
		"mcp    github: npx -y @modelcontextprotocol/server-github",
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
	WriteAgentRunHeader(&b, "s", "t", "claude-x", nil, "", "", "", nil, nil)
	out := b.String()
	for _, unwanted := range []string{"prompt", "system", "tools", "mcp"} {
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

// TestGutterWriter_SplitWrites: subprocess output reaches the gutter in
// arbitrary chunks. Every line must get exactly one gutter regardless of
// where the chunk boundaries fall, and EndLine must close a partial line.
func TestGutterWriter_SplitWrites(t *testing.T) {
	prev := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = prev }()

	var b bytes.Buffer
	g := newGutterWriter(&b, shellAccent)
	for _, chunk := range []string{"fir", "st\nsec", "ond\n", "\n", "no newline"} {
		_, _ = g.Write([]byte(chunk))
	}
	g.EndLine()
	g.EndLine() // idempotent
	want := "│ first\n│ second\n│ \n│ no newline\n"
	if got := b.String(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestRenderLogLine_Lifecycle: routine lines are reworded for pretty mode,
// lead with the schedule name, drop internal tags, and show self-describing
// values without their keys. Warnings keep their original message.
func TestRenderLogLine_Lifecycle(t *testing.T) {
	prev := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = prev }()

	at := time.Date(2026, 10, 22, 22, 41, 6, 0, time.Local)
	render := func(level slog.Level, msg string, attrs ...slog.Attr) string {
		r := slog.NewRecord(at, level, msg, 0)
		r.AddAttrs(attrs...)
		var b bytes.Buffer
		if err := renderLogLine(&b, r); err != nil {
			t.Fatalf("renderLogLine: %v", err)
		}
		return b.String()
	}

	cases := []struct{ got, want string }{
		{render(slog.LevelInfo, "Starting cron...",
			slog.String("schedule", "nightly"), slog.String("cron", "@every 30s"),
			slog.Int64("seq", 3), slog.String("lifetime", "abc")),
			"22:41:06 · scheduled nightly  @every 30s\n"},
		{render(slog.LevelInfo, "config loaded",
			slog.String("path", "/nonexistent/cronicle.hcl"), slog.Int("schedules", 1), slog.Int("tasks", 5)),
			"22:41:06 · config loaded  /nonexistent/cronicle.hcl · 1 schedule · 5 tasks\n"},
		{render(slog.LevelInfo, "heartbeat", slog.String("cronicle", "alive"), slog.Int("schedules", 2)),
			"22:41:06 · alive  2 schedules\n"},
		{render(slog.LevelWarn, "config refresh failed; keeping previous schedules in place",
			slog.String("error", "boom")),
			"22:41:06 ▲ config refresh failed; keeping previous schedules in place  error=boom\n"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got  %q\nwant %q", c.got, c.want)
		}
	}
}

// TestWriteBetweenBlocks_WaitsForStreamingBlock: a routine line (heartbeat,
// "queued") logged while a task block is streaming must not land inside
// that block — it prints right after the block closes, and immediately
// when nothing is streaming.
func TestWriteBetweenBlocks_WaitsForStreamingBlock(t *testing.T) {
	var out bytes.Buffer
	sw := &StreamingWriter{out: &out}
	if !streamLock.TryLock() {
		t.Fatal("streamLock unexpectedly held")
	}
	sw.leader = true

	_, _ = sw.Write([]byte("╭─ task\n"))
	_ = writeBetweenBlocks(&out, []byte("· alive\n"))
	_, _ = sw.Write([]byte("╰─ task\n"))
	sw.Close()

	if got, want := out.String(), "╭─ task\n╰─ task\n· alive\n"; got != want {
		t.Fatalf("line leaked into the block:\ngot  %q\nwant %q", got, want)
	}

	_ = writeBetweenBlocks(&out, []byte("· stopped\n"))
	if !strings.HasSuffix(out.String(), "· stopped\n") {
		t.Fatalf("line not written when no block is streaming: %q", out.String())
	}
}

// TestWriteAgentRunHeader_MultilinePrompt: prompts keep their line breaks,
// indented under the label and capped so a long prompt can't bury output.
func TestWriteAgentRunHeader_MultilinePrompt(t *testing.T) {
	prev := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = prev }()

	var b bytes.Buffer
	WriteAgentRunHeader(&b, "s", "t", "m", nil, "line one\nline two", "", "", nil, nil)
	if !strings.Contains(b.String(), "│ prompt line one\n│        line two\n") {
		t.Fatalf("prompt lines not aligned:\n%s", b.String())
	}

	b.Reset()
	WriteAgentRunHeader(&b, "s", "t", "m", nil, strings.Repeat("more\n", 30), "", "", nil, nil)
	if got := strings.Count(b.String(), "more"); got != frameRowMaxLines-1+1 { // rows + "more lines" marker
		t.Errorf("expected %d prompt rows before the cap, got %d", frameRowMaxLines-1, got-1)
	}
	if !strings.Contains(b.String(), "… 11 more lines") {
		t.Errorf("missing overflow marker:\n%s", b.String())
	}
}
