package cronicle

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fatih/color"
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
	case "shell_run":
		return p.renderShellRun(r)
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
	header := color.New(color.FgCyan, color.Bold).SprintFunc()
	rule := color.New(color.FgCyan).SprintFunc()
	footer := color.New(color.Faint).SprintFunc()
	errc := color.New(color.FgRed, color.Bold).SprintFunc()

	var (
		schedule, task, model, response, costStr string
		stop, transcript, errMsg                  string
		durationMs, in, out                       int64
		success                                   bool
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
		case "transcript":
			transcript = a.Value.String()
		case "stop_reason":
			stop = a.Value.String()
		case "error":
			errMsg = a.Value.String()
		case "success":
			success = a.Value.Bool()
		}
		return true
	})

	headerLine := fmt.Sprintf("agent run · schedule=%s · task=%s · model=%s",
		schedule, task, model)
	bar := strings.Repeat("━", len(headerLine)+8)

	var b bytes.Buffer
	b.WriteString(rule(bar))
	b.WriteByte('\n')
	b.WriteString(header(headerLine))
	b.WriteByte('\n')
	b.WriteString(rule(bar))
	b.WriteString("\n\n")

	if success {
		if response == "" {
			b.WriteString(footer("(no text response)\n"))
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
	footerParts := []string{
		fmt.Sprintf("%d in / %d out tokens", in, out),
		fmt.Sprintf("$%s", costStr),
		fmt.Sprintf("%dms", durationMs),
	}
	if stop != "" {
		footerParts = append(footerParts, fmt.Sprintf("stop=%s", stop))
	}
	if transcript != "" {
		footerParts = append(footerParts, fmt.Sprintf("transcript=%s", filepath.Base(transcript)))
	}
	b.WriteString(footer("[" + strings.Join(footerParts, " · ") + "]"))
	b.WriteString("\n\n")

	_, err := p.out.Write(b.Bytes())
	return err
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

	headerLine := fmt.Sprintf("schedule \"%s\"", schedule)
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
