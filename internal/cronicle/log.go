package cronicle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fatih/color"
	"github.com/jshiv/cronicle/pkg/agent"
	"github.com/mattn/go-isatty"
	"gopkg.in/natefinch/lumberjack.v2"
)

// LogFormat selects the stdout log rendering.
type LogFormat string

const (
	LogFormatAuto   LogFormat = "auto"
	LogFormatPretty LogFormat = "pretty"
	LogFormatText   LogFormat = "text"
	LogFormatJSON   LogFormat = "json"
)

// resolvedLogFormat tracks what format SetupLogging settled on after auto-
// resolution. Other subsystems (the agent streaming path) probe it via
// IsStreamingPretty to decide whether to render in real time or buffer.
var resolvedLogFormat LogFormat

// SetupLogging configures slog's default logger according to format.
// "auto" picks pretty when stdout is a TTY, text otherwise.
func SetupLogging(format LogFormat) {
	resolved := format
	if resolved == "" || resolved == LogFormatAuto {
		if isatty.IsTerminal(os.Stdout.Fd()) {
			resolved = LogFormatPretty
		} else {
			resolved = LogFormatText
		}
	}
	resolvedLogFormat = resolved

	var handler slog.Handler
	switch resolved {
	case LogFormatPretty:
		handler = &prettyHandler{
			fallback: newTintHandler(os.Stdout),
			out:      os.Stdout,
		}
	case LogFormatJSON:
		handler = slog.NewJSONHandler(os.Stdout, nil)
	default:
		handler = slog.NewTextHandler(os.Stdout, nil)
	}
	slog.SetDefault(slog.New(handler))
}

// IsStreamingPretty reports whether stdout is configured for pretty rendering.
// Used by the agent dispatch path to decide whether to render text deltas in
// real time vs buffer them into the consolidated slog event.
func IsStreamingPretty() bool {
	return resolvedLogFormat == LogFormatPretty
}

// streamLock coordinates per-task streaming writers so concurrent tasks don't
// interleave their blocks on stdout. The first task to start streaming
// acquires the lock and streams live; others buffer their output and dump it
// atomically when the leader finishes. Compute parallelism is preserved —
// tasks run concurrently regardless of who holds the lock.
var streamLock sync.Mutex

// StreamingWriter is the per-task io.Writer used during streaming pretty
// mode. The first one to be constructed acquires the streamLock and becomes
// the "leader": all writes pass straight to stdout, giving live streaming UX
// for whichever task got there first. Concurrent tasks become followers —
// their writes accumulate in an internal buffer, and Close flushes the buffer
// to stdout atomically once the leader (or any earlier follower) releases
// the lock.
//
// Use newStreamingWriter to construct, and call Close exactly once via defer.
type StreamingWriter struct {
	leader bool
	buf    bytes.Buffer
	out    io.Writer
	closed bool
	mu     sync.Mutex // guards buf for concurrent Write calls within one task
}

// NewStreamingWriter returns a writer for one task's streaming output.
func NewStreamingWriter() *StreamingWriter {
	sw := &StreamingWriter{out: os.Stdout}
	if streamLock.TryLock() {
		sw.leader = true
	}
	return sw
}

// Write either passes the bytes through to stdout (if leader) or appends them
// to the buffer (if follower).
func (s *StreamingWriter) Write(p []byte) (int, error) {
	if s.leader {
		return s.out.Write(p)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// Close releases the leader lock, or — for followers — blocks to acquire the
// lock, flushes the accumulated buffer atomically, and releases. Idempotent.
func (s *StreamingWriter) Close() {
	if s.closed {
		return
	}
	s.closed = true
	if s.leader {
		streamLock.Unlock()
		return
	}
	streamLock.Lock()
	s.mu.Lock()
	_, _ = s.out.Write(s.buf.Bytes())
	s.mu.Unlock()
	streamLock.Unlock()
}

// FileLoggingEnabled reports whether --log-to-file was passed on the
// current invocation. It's the master switch for on-disk artifacts:
// cronicle.jsonl AND per-run transcripts. Set by EnableFileLog.
var FileLoggingEnabled bool

// CroniclePath is the directory under which on-disk artifacts (.cronicle/log,
// .cronicle/runs) are rooted. Set by EnableFileLog.
var CroniclePath string

// EnableFileLog composes the current default handler with a JSON-mirroring
// handler that writes to .cronicle/log/cronicle.jsonl, rotated by lumberjack.
// Stdout output is unaffected. Also flips FileLoggingEnabled so other
// subsystems (agent transcripts, shell transcripts) can opt in.
func EnableFileLog(croniclePath string) error {
	logDir := filepath.Join(croniclePath, ".cronicle", "log")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	file := &lumberjack.Logger{
		Filename:   filepath.Join(logDir, "cronicle.jsonl"),
		MaxSize:    500, // MB per file before rotation
		MaxBackups: 3,   // keep up to 3 rotated files
		MaxAge:     28,  // days; older files deleted
		Compress:   true,
	}
	fileHandler := slog.NewJSONHandler(file, nil)
	current := slog.Default().Handler()
	slog.SetDefault(slog.New(&multiHandler{handlers: []slog.Handler{current, fileHandler}}))
	FileLoggingEnabled = true
	CroniclePath = croniclePath
	return nil
}

// TranscriptDir returns the directory where per-run transcripts should be
// written, or "" if file logging is disabled.
func TranscriptDir() string {
	if !FileLoggingEnabled {
		return ""
	}
	return filepath.Join(CroniclePath, ".cronicle", "runs")
}

// ApplyTimezone wraps the default logger's current handler in a tzHandler so
// timestamps render in loc. Idempotent — won't double-wrap.
func ApplyTimezone(loc *time.Location) {
	current := slog.Default().Handler()
	if _, alreadyTZ := current.(*tzHandler); alreadyTZ {
		return
	}
	slog.SetDefault(slog.New(&tzHandler{inner: current, loc: loc}))
}

// Fatal logs an error-level message via slog and exits with status 1.
// Accepts either a single error, a single string message, or a message
// followed by slog-style key/value pairs.
func Fatal(args ...any) {
	switch len(args) {
	case 0:
		slog.Error("fatal")
	case 1:
		switch v := args[0].(type) {
		case error:
			slog.Error("fatal", "error", v.Error())
		case string:
			slog.Error(v)
		default:
			slog.Error(fmt.Sprint(v))
		}
	default:
		if msg, ok := args[0].(string); ok {
			slog.Error(msg, args[1:]...)
		} else {
			slog.Error(fmt.Sprint(args...))
		}
	}
	os.Exit(1)
}

// ---- multiHandler ----------------------------------------------------------

// multiHandler fans Handle out to multiple slog.Handlers, replicating the
// "logrus hook" pattern for slog. Used to mirror stdout to the file.
type multiHandler struct{ handlers []slog.Handler }

func (m *multiHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	for _, h := range m.handlers {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		// Each handler gets its own clone since handlers may mutate Time.
		if err := h.Handle(ctx, r.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		out[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{handlers: out}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	out := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		out[i] = h.WithGroup(name)
	}
	return &multiHandler{handlers: out}
}

// ---- tzHandler -------------------------------------------------------------

// tzHandler adjusts each record's Time to a fixed location before delegating.
type tzHandler struct {
	inner slog.Handler
	loc   *time.Location
}

func (h *tzHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}
func (h *tzHandler) Handle(ctx context.Context, r slog.Record) error {
	r.Time = r.Time.In(h.loc)
	return h.inner.Handle(ctx, r)
}
func (h *tzHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &tzHandler{inner: h.inner.WithAttrs(attrs), loc: h.loc}
}
func (h *tzHandler) WithGroup(name string) slog.Handler {
	return &tzHandler{inner: h.inner.WithGroup(name), loc: h.loc}
}

// ---- tintHandler -----------------------------------------------------------

// tintHandler renders records in a colored "INFO[ts] msg key=val" style
// reminiscent of logrus's default text output. Used as the fallback for
// non-agent records in pretty mode.
type tintHandler struct {
	out   io.Writer
	attrs []slog.Attr
}

func newTintHandler(out io.Writer) *tintHandler { return &tintHandler{out: out} }

func (h *tintHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= slog.LevelInfo
}

func (h *tintHandler) Handle(_ context.Context, r slog.Record) error {
	var lc, kc func(a ...any) string
	switch r.Level {
	case slog.LevelDebug:
		lc = color.New(color.FgBlue).SprintFunc()
	case slog.LevelWarn:
		lc = color.New(color.FgYellow, color.Bold).SprintFunc()
	case slog.LevelError:
		lc = color.New(color.FgRed, color.Bold).SprintFunc()
	default:
		lc = color.New(color.FgCyan, color.Bold).SprintFunc()
	}
	kc = color.New(color.FgCyan).SprintFunc()

	var b bytes.Buffer
	levelTag := strings.ToUpper(r.Level.String())
	if len(levelTag) > 4 {
		levelTag = levelTag[:4]
	}
	fmt.Fprintf(&b, "%s[%s] %s", lc(levelTag), r.Time.Format(time.RFC3339), r.Message)

	for _, a := range h.attrs {
		fmt.Fprintf(&b, " %s=%s", kc(a.Key), formatValue(a.Value))
	}
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%s", kc(a.Key), formatValue(a.Value))
		return true
	})
	b.WriteByte('\n')
	_, err := h.out.Write(b.Bytes())
	return err
}

func (h *tintHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	cp := *h
	cp.attrs = append(cp.attrs[:len(cp.attrs):len(cp.attrs)], attrs...)
	return &cp
}

func (h *tintHandler) WithGroup(_ string) slog.Handler { return h }

// formatValue renders a slog value in a logfmt-ish style: bare for safe
// scalars, quoted when it contains whitespace or quotes.
func formatValue(v slog.Value) string {
	s := v.String()
	if strings.ContainsAny(s, " \t\n\"") {
		return fmt.Sprintf("%q", s)
	}
	return s
}

// ---- prettyHandler ---------------------------------------------------------

// prettyHandler renders structural records (entry_type=…) as multi-line blocks
// or compact section headers, and everything else as a dim single line. The
// fallback handler exists for nested compositions but isn't used for direct
// rendering — pretty mode always renders something itself.
type prettyHandler struct {
	fallback slog.Handler
	out      io.Writer
}

func (p *prettyHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return p.fallback.Enabled(ctx, l)
}

func (p *prettyHandler) Handle(_ context.Context, r slog.Record) error {
	switch entryType(r) {
	case "agent_run":
		return p.renderAgentRun(r)
	case "agent_run_streamed":
		// The streaming dispatch path already wrote the block to stdout in
		// real time; the slog event exists only for the file mirror and
		// non-pretty consumers. Suppress here.
		return nil
	case "shell_run":
		return p.renderShellRun(r)
	case "shell_run_streamed":
		// Already rendered to stdout in real time; the slog event exists
		// only for the file mirror.
		return nil
	case "schedule_start":
		return p.renderScheduleStart(r)
	case "schedule_complete":
		return p.renderScheduleComplete(r)
	case "task_start":
		// Block headers (agent_run / shell_run) subsume the start signal,
		// so suppress task_start in pretty mode. The file mirror still
		// gets the event.
		return nil
	default:
		return p.renderDimLine(r)
	}
}

func (p *prettyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &prettyHandler{fallback: p.fallback.WithAttrs(attrs), out: p.out}
}

func (p *prettyHandler) WithGroup(name string) slog.Handler {
	return &prettyHandler{fallback: p.fallback.WithGroup(name), out: p.out}
}

// entryType extracts the entry_type attr from a record, returning "" if absent.
func entryType(r slog.Record) string {
	var et string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "entry_type" {
			et = a.Value.String()
			return false
		}
		return true
	})
	return et
}

// attrString fetches a string-valued attribute by key, returning "" if missing.
func attrString(r slog.Record, key string) string {
	var s string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			s = a.Value.String()
			return false
		}
		return true
	})
	return s
}

// attrInt64 fetches an int64-valued attribute by key, returning 0 if missing.
func attrInt64(r slog.Record, key string) int64 {
	var n int64
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			n = a.Value.Int64()
			return false
		}
		return true
	})
	return n
}

// attrBool fetches a bool-valued attribute by key, returning false if missing.
func attrBool(r slog.Record, key string) bool {
	var v bool
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v = a.Value.Bool()
			return false
		}
		return true
	})
	return v
}

// attrStrings fetches a []string-valued attribute by key. Used for the
// schedule_start "tasks" attr, which slog.Any-wraps a string slice.
func attrStrings(r slog.Record, key string) []string {
	var out []string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key != key {
			return true
		}
		if v, ok := a.Value.Any().([]string); ok {
			out = v
		}
		return false
	})
	return out
}

func (p *prettyHandler) renderAgentRun(r slog.Record) error {
	var (
		schedule, task, model, response, costStr string
		stop, transcript, errMsg                  string
		durationMs, in, out, cacheRead            int64
		success                                   bool
		skills                                    []string
	)
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "schedule":
			schedule = a.Value.String()
		case "task":
			task = a.Value.String()
		case "model":
			model = a.Value.String()
		case "response":
			response = a.Value.String()
		case "cost_usd":
			costStr = a.Value.String()
		case "duration_ms":
			durationMs = a.Value.Int64()
		case "input_tokens":
			in = a.Value.Int64()
		case "output_tokens":
			out = a.Value.Int64()
		case "cache_read":
			cacheRead = a.Value.Int64()
		case "transcript":
			transcript = a.Value.String()
		case "stop_reason":
			stop = a.Value.String()
		case "error":
			errMsg = a.Value.String()
		case "success":
			success = a.Value.Bool()
		case "skills_available":
			if v, ok := a.Value.Any().([]string); ok {
				skills = v
			}
		}
		return true
	})

	var b bytes.Buffer
	WriteAgentRunHeader(&b, schedule, task, model, skills)
	b.WriteByte('\n')

	footerColor := color.New(color.Faint).SprintFunc()
	errc := color.New(color.FgRed, color.Bold).SprintFunc()
	if success {
		if response == "" {
			b.WriteString(footerColor("(no text response)\n"))
		} else {
			b.WriteString(strings.TrimRight(response, "\n"))
			b.WriteByte('\n')
		}
	} else {
		b.WriteString(errc("ERROR: "))
		b.WriteString(errMsg)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	WriteAgentRunFooter(&b, in, out, cacheRead, costStr, durationMs, stop, transcript)

	_, err := p.out.Write(b.Bytes())
	return err
}

// WriteAgentRunHeader writes the bordered header for an agent run block.
// Shared by the slog renderAgentRun path and the streaming dispatch path.
// `skills` is the available-skills list to surface in the header (empty/nil
// omits the segment).
func WriteAgentRunHeader(w io.Writer, schedule, task, model string, skills []string) {
	header := color.New(color.FgCyan, color.Bold).SprintFunc()
	rule := color.New(color.FgCyan).SprintFunc()

	headerLine := fmt.Sprintf("agent run · schedule=%s · task=%s · model=%s",
		schedule, task, model)
	if len(skills) > 0 {
		headerLine += " · skills=[" + strings.Join(skills, ",") + "]"
	}
	bar := strings.Repeat("━", len(headerLine)+8)

	fmt.Fprintln(w, rule(bar))
	fmt.Fprintln(w, header(headerLine))
	fmt.Fprintln(w, rule(bar))
}

// WriteAgentRunFooter writes the bracketed metadata footer for an agent run.
// cacheRead is the cumulative cache-read tokens; >0 indicates a cache hit
// occurred during the run (turn 2+ replayed the cached prefix at 0.1× cost).
func WriteAgentRunFooter(w io.Writer, in, out, cacheRead int64, costStr string, durationMs int64, stopReason, transcriptPath string) {
	footer := color.New(color.Faint).SprintFunc()

	parts := []string{
		fmt.Sprintf("%d in / %d out tokens", in, out),
	}
	if cacheRead > 0 {
		parts = append(parts, fmt.Sprintf("cached=%d", cacheRead))
	}
	parts = append(parts,
		fmt.Sprintf("$%s", costStr),
		fmt.Sprintf("%dms", durationMs),
	)
	if stopReason != "" {
		parts = append(parts, fmt.Sprintf("stop=%s", stopReason))
	}
	if transcriptPath != "" {
		parts = append(parts, fmt.Sprintf("transcript=%s", filepath.Base(transcriptPath)))
	}
	fmt.Fprintln(w, footer("["+strings.Join(parts, " · ")+"]"))
	fmt.Fprintln(w)
}

// WriteShellRunHeader writes the bordered header for a shell task run block.
func WriteShellRunHeader(w io.Writer, schedule, task, command string) {
	header := color.New(color.FgCyan, color.Bold).SprintFunc()
	rule := color.New(color.FgCyan).SprintFunc()

	headerLine := fmt.Sprintf("shell run · schedule=%s · task=%s · %s",
		schedule, task, truncate(escapeControl(command), 60))
	bar := strings.Repeat("━", len(headerLine)+8)

	fmt.Fprintln(w, rule(bar))
	fmt.Fprintln(w, header(headerLine))
	fmt.Fprintln(w, rule(bar))
}

// WriteShellRunFooter writes the bracketed metadata footer for a shell run.
func WriteShellRunFooter(w io.Writer, exit int64, durationMs int64, transcriptPath string) {
	footer := color.New(color.Faint).SprintFunc()

	parts := []string{
		fmt.Sprintf("exit=%d", exit),
		fmt.Sprintf("%dms", durationMs),
	}
	if transcriptPath != "" {
		parts = append(parts, fmt.Sprintf("transcript=%s", filepath.Base(transcriptPath)))
	}
	fmt.Fprintln(w, footer("["+strings.Join(parts, " · ")+"]"))
	fmt.Fprintln(w)
}

// NewAgentStreamRenderer returns a StreamHandler that writes the body of an
// agent run block to w. Text deltas pass through; thinking deltas render
// dimmed and prefixed; tool_use events render with the parsed command (for
// bash) on tool_use_stop; tool_result events render an exit/duration line
// (success green, error red).
//
// The caller is responsible for writing the header before the first event
// and the footer after the run completes.
func NewAgentStreamRenderer(w io.Writer) agent.StreamHandler {
	dim := color.New(color.Faint).SprintFunc()
	toolHi := color.New(color.FgYellow).SprintFunc()
	ok := color.New(color.FgGreen, color.Faint).SprintFunc()
	bad := color.New(color.FgRed, color.Faint).SprintFunc()

	inThinking := false

	flushThinking := func() {
		if inThinking {
			fmt.Fprintln(w)
			inThinking = false
		}
	}

	return func(e agent.StreamEvent) {
		switch e.Type {
		case agent.StreamEventTextDelta:
			flushThinking()
			fmt.Fprint(w, e.Text)
		case agent.StreamEventThinkingDelta:
			if !inThinking {
				fmt.Fprint(w, dim("\n┊ thinking: "))
				inThinking = true
			}
			fmt.Fprint(w, dim(e.Text))
		case agent.StreamEventToolUseStart:
			flushThinking()
			fmt.Fprintf(w, "\n%s%s%s %s\n",
				toolHi("→ "), toolHi(displayToolName(e.ToolName)), toolHi(":"),
				formatToolInput(e.ToolName, e.ToolInput))
		case agent.StreamEventToolResult:
			marker := ok("← exit=0")
			if e.IsError {
				marker = bad("← error")
			}
			fmt.Fprintf(w, "%s %s\n", marker, dim(formatDuration(e.DurationMs)))
		case agent.StreamEventTurnStart:
			fmt.Fprintln(w, dim(fmt.Sprintf("\n— turn %d —", e.TurnIndex+1)))
		}
	}
}

// formatToolInput renders the raw JSON tool input compactly for the pretty
// stream. Per-tool formatting:
//
//	bash                          → command string
//	str_replace_based_edit_tool   → "<command> <path>"  (e.g. "view foo.go")
//	web_search                    → query
//	web_fetch                     → url
//
// Unknown tools fall through to the raw JSON.
func formatToolInput(toolName, raw string) string {
	if raw == "" {
		return ""
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return raw
	}
	switch toolName {
	case "bash":
		if cmd, ok := args["command"].(string); ok {
			return cmd
		}
	case "str_replace_based_edit_tool":
		cmd, _ := args["command"].(string)
		path, _ := args["path"].(string)
		switch {
		case cmd != "" && path != "":
			return cmd + " " + path
		case path != "":
			return path
		case cmd != "":
			return cmd
		}
	case "web_search":
		if q, ok := args["query"].(string); ok {
			return q
		}
	case "web_fetch":
		if u, ok := args["url"].(string); ok {
			return u
		}
	case "load_skill":
		if n, ok := args["name"].(string); ok {
			return n
		}
	}
	// MCP tools (server__tool) — show the args as compact JSON, but trim
	// to keep the line readable. Generic since tool schemas vary widely.
	if strings.Contains(toolName, MCPNameSep) {
		return truncate(compactJSON(raw), 80)
	}
	return raw
}

// compactJSON rewraps a JSON blob to its compact form. Returns the original
// string on any parse error so we don't lose information when the model
// emits unusual content.
func compactJSON(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return s
	}
	return string(b)
}

// displayToolName maps the API tool name to a friendlier label for pretty
// rendering. The model invokes tools by their API name (e.g.
// "str_replace_based_edit_tool"); the user just wants to see "editor".
// MCP tools come in as `<server>__<tool>` and render as `<server>.<tool>`
// so the server boundary is obvious in the stream.
func displayToolName(name string) string {
	switch name {
	case "str_replace_based_edit_tool":
		return "editor"
	case "load_skill":
		return "skill"
	}
	if i := strings.Index(name, MCPNameSep); i > 0 {
		return name[:i] + "." + name[i+len(MCPNameSep):]
	}
	return name
}

// renderShellRun renders a shell task as a block with the same shape as
// agent_run: header rule, header line, body (stdout or stderr), footer.
func (p *prettyHandler) renderShellRun(r slog.Record) error {
	header := color.New(color.FgCyan, color.Bold).SprintFunc()
	rule := color.New(color.FgCyan).SprintFunc()
	footer := color.New(color.Faint).SprintFunc()
	errc := color.New(color.FgRed, color.Bold).SprintFunc()

	schedule := attrString(r, "schedule")
	task := attrString(r, "task")
	command := attrString(r, "command")
	stdout := attrString(r, "stdout")
	stderr := attrString(r, "stderr")
	transcript := attrString(r, "transcript")
	durationMs := attrInt64(r, "duration_ms")
	exit := attrInt64(r, "exit")
	success := attrBool(r, "success")

	headerLine := fmt.Sprintf("shell run · schedule=%s · task=%s · %s",
		schedule, task, truncate(escapeControl(command), 60))
	bar := strings.Repeat("━", len(headerLine)+8)

	var b bytes.Buffer
	b.WriteString(rule(bar))
	b.WriteByte('\n')
	b.WriteString(header(headerLine))
	b.WriteByte('\n')
	b.WriteString(rule(bar))
	b.WriteString("\n\n")

	if success {
		body := strings.TrimRight(stdout, "\n")
		if body == "" {
			b.WriteString(footer("(no stdout)\n"))
		} else {
			b.WriteString(body)
			b.WriteByte('\n')
		}
	} else {
		b.WriteString(errc("ERROR (exit "))
		fmt.Fprintf(&b, "%d", exit)
		b.WriteString(errc("):\n"))
		body := strings.TrimRight(stderr, "\n")
		if body == "" {
			body = strings.TrimRight(stdout, "\n")
		}
		b.WriteString(body)
		b.WriteByte('\n')
	}

	b.WriteByte('\n')
	footerParts := []string{
		fmt.Sprintf("exit=%d", exit),
		fmt.Sprintf("%dms", durationMs),
	}
	if transcript != "" {
		footerParts = append(footerParts, fmt.Sprintf("transcript=%s", filepath.Base(transcript)))
	}
	b.WriteString(footer("[" + strings.Join(footerParts, " · ") + "]"))
	b.WriteString("\n\n")

	_, err := p.out.Write(b.Bytes())
	return err
}

// renderScheduleStart renders the section header above a schedule's task
// blocks: a horizontal rule, the schedule name, and the DAG list.
func (p *prettyHandler) renderScheduleStart(r slog.Record) error {
	rule := color.New(color.FgMagenta).SprintFunc()
	header := color.New(color.FgMagenta, color.Bold).SprintFunc()
	dim := color.New(color.Faint).SprintFunc()

	schedule := attrString(r, "schedule")
	tasks := attrStrings(r, "tasks")

	timeStr := r.Time.Format("15:04:05")
	headerLine := fmt.Sprintf("%s · schedule \"%s\"", timeStr, schedule)
	const totalWidth = 70
	const leftRule = 4 // "────"
	trailingCount := totalWidth - leftRule - 1 - len(headerLine) - 1
	if trailingCount < 0 {
		trailingCount = 0
	}

	var b bytes.Buffer
	b.WriteString(rule(strings.Repeat("─", leftRule)))
	b.WriteByte(' ')
	b.WriteString(header(headerLine))
	b.WriteByte(' ')
	b.WriteString(rule(strings.Repeat("─", trailingCount)))
	b.WriteByte('\n')

	if len(tasks) > 0 {
		b.WriteString(dim("DAG:\n"))
		for i, t := range tasks {
			prefix := "  ├─ "
			if i == len(tasks)-1 {
				prefix = "  └─ "
			}
			b.WriteString(dim(prefix + t + "\n"))
		}
	}
	b.WriteByte('\n')

	_, err := p.out.Write(b.Bytes())
	return err
}

// renderScheduleComplete renders the summary line at the bottom of a schedule.
func (p *prettyHandler) renderScheduleComplete(r slog.Record) error {
	ok := color.New(color.FgGreen, color.Bold).SprintFunc()
	bad := color.New(color.FgRed, color.Bold).SprintFunc()
	dim := color.New(color.Faint).SprintFunc()

	schedule := attrString(r, "schedule")
	taskCount := attrInt64(r, "task_count")
	durationMs := attrInt64(r, "duration_ms")
	success := attrBool(r, "success")

	var b bytes.Buffer
	if success {
		b.WriteString(ok("✓ "))
		fmt.Fprintf(&b, "schedule \"%s\" complete ", schedule)
	} else {
		b.WriteString(bad("✗ "))
		fmt.Fprintf(&b, "schedule \"%s\" failed ", schedule)
		errMsg := attrString(r, "error")
		if errMsg != "" {
			b.WriteString(dim("(" + errMsg + ") "))
		}
	}
	taskWord := "tasks"
	if taskCount == 1 {
		taskWord = "task"
	}
	b.WriteString(dim(fmt.Sprintf("· %d %s · %s", taskCount, taskWord, formatDuration(durationMs))))
	b.WriteString("\n\n")

	_, err := p.out.Write(b.Bytes())
	return err
}

// renderDimLine renders a record as a faint single line. Used for lifecycle
// events (Loading, executing tasks, heartbeat, refreshing config, etc.) that
// don't carry a structural entry_type.
func (p *prettyHandler) renderDimLine(r slog.Record) error {
	dim := color.New(color.Faint).SprintFunc()

	var b bytes.Buffer
	b.WriteString(dim(r.Time.Format("15:04:05")))
	b.WriteByte(' ')
	b.WriteString(dim(r.Message))
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "entry_type" {
			return true
		}
		fmt.Fprintf(&b, " %s", dim(a.Key+"="+formatValue(a.Value)))
		return true
	})
	b.WriteByte('\n')

	_, err := p.out.Write(b.Bytes())
	return err
}

// escapeControl replaces newlines and tabs with their literal escape forms so
// they don't break a single-line header layout when the source is shell input.
func escapeControl(s string) string {
	r := strings.NewReplacer("\n", "\\n", "\t", "\\t", "\r", "\\r")
	return r.Replace(s)
}

// truncate trims s to at most n runes, appending "…" when shortened.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// formatDuration renders a millisecond value as "Nms" or "N.Ns" depending on
// magnitude.
func formatDuration(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}
