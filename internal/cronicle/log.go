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
	"github.com/jshiv/cronicle/internal/cronicle/state"
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

// LiveFormat selects the wire encoding of the live-stream SSE endpoint
// (GET /v1/runs/{id}/events). Independent of LogFormat — operators may
// want pretty-on-stdout AND json-on-the-wire, or vice versa.
//
// Default (set by SetLiveFormat at startup) is pretty: the SSE stream
// emits ANSI-colored multi-line text identical to what a TTY would
// show. Frontends consume via xterm.js. JSON / text are alternatives
// for non-terminal clients.
type LiveFormat string

const (
	LiveFormatPretty LiveFormat = "pretty"
	// LiveFormatPrettyColor is `pretty` but with ANSI color codes ALWAYS
	// emitted, regardless of whether cronicled's own stdout is a TTY.
	// Necessary for headless service deployments (systemd, docker, k8s)
	// where fatih/color's global NoColor=true would otherwise strip the
	// color codes before they reach the SSE wire — leaving the xterm.js
	// frontend with monochrome text even though it can render ANSI fine.
	LiveFormatPrettyColor LiveFormat = "pretty-color"
	LiveFormatJSON        LiveFormat = "json"
	LiveFormatText        LiveFormat = "text"
)

// currentLiveFormat is the format applied when NewLiveEncoder is called
// from EnableStateStore. Defaults to pretty; override with SetLiveFormat
// before EnableStateStore.
var currentLiveFormat LiveFormat = LiveFormatPretty

// SetLiveFormat changes the wire format the SSE live-stream will use.
// Call before EnableStateStore. Empty string keeps the default (pretty).
func SetLiveFormat(format LiveFormat) {
	if format != "" {
		currentLiveFormat = format
	}
}

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
		handler = &stdoutPrettyHandler{
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

// stateStore is the process-wide projection store, typed as the
// interface so cronicle-infra (and any other cluster runner) can
// inject a non-SQLite implementation via SetStateStore. Default backing
// is *state.Store opened by EnableStateStore.
var stateStore state.Backend

// liveSink is the process-wide live-stream pub/sub. Constructed by
// EnableStateStore (paired with the projection store — same lifecycle).
// nil when state is disabled. The HTTP listener's SSE handler reads
// this via LiveSink() to subscribe per-run.
var liveSink *state.LiveSink

// StateStore returns the process-wide state backend, or nil if not
// enabled. Listener handlers and the cron tick / DAG walker gates use
// this to issue queries.
func StateStore() state.Backend { return stateStore }

// SetStateStore swaps in an external Backend implementation (e.g.,
// cronicle-infra's PostgresStore). Closes the prior store if one was
// open. The slog fan-out chain that EnableStateStore configures is
// NOT reconfigured here — pair this with EnableStateStore-style wiring
// in the embedding process if you need the projection-side ingest path.
//
// Intended for cluster deployments where the runtime-control rows
// (pause/skip/cancel/drain) live in a shared Postgres instance instead
// of the per-runner SQLite file.
func SetStateStore(b state.Backend) {
	if stateStore != nil && stateStore != b {
		_ = stateStore.Close()
	}
	stateStore = b
}

// LiveSink returns the process-wide LiveSink, or nil if not enabled.
// The /v1/runs/{id}/events SSE handler uses this to subscribe.
func LiveSink() *state.LiveSink { return liveSink }

// EnableStateStore opens the projection store at dsn, wires the slog
// chain to fan out into:
//
//	Tagger → multiHandler{ existing, Sink, LiveSink }
//
// Tagger sits at the top so every record gets seq+lifetime BEFORE the
// fan-out — guaranteeing the bytes in cronicle.jsonl (which Loki ships)
// carry the same cursor IDs as the frames in the live SSE stream. The
// frontend can deep-link "I clicked seq=42 in live → take me to the
// Loki view scoped to lifetime+seq=42."
//
// Idempotent — a second call closes the prior store and replaces both
// store and live sink.
func EnableStateStore(dsn string) error {
	if stateStore != nil {
		_ = stateStore.Close()
		stateStore = nil
		liveSink = nil
	}
	s, err := state.Open(dsn)
	if err != nil {
		return err
	}
	// At-rest secrets encryption is opt-in via CRONICLE_DEK_HEX. When
	// the env is set, bind the AEAD and run a backfill of any pre-v10
	// plaintext rows once. The DEK is held in process memory; an
	// attacker with read-only access to state.db sees only ct/nonce.
	if dek := strings.TrimSpace(os.Getenv("CRONICLE_DEK_HEX")); dek != "" {
		aead, err := state.NewAEAD(dek)
		if err != nil {
			_ = s.Close()
			return fmt.Errorf("CRONICLE_DEK_HEX: %w", err)
		}
		s.WithAEAD(aead)
		if n, err := s.BackfillSecrets(); err != nil {
			_ = s.Close()
			return fmt.Errorf("secrets backfill: %w", err)
		} else if n > 0 {
			slog.Info("secrets: at-rest backfill complete", "rows", n)
		}
	}
	stateStore = s
	liveSink = state.NewLiveSink(newLiveEncoder(currentLiveFormat))

	current := slog.Default().Handler()
	// If a Tagger already sits at the top of the chain (re-enable),
	// peel it off so we don't stack two — that would double-tag every
	// record (two seq attrs, two lifetime attrs).
	if t, ok := current.(*state.Tagger); ok {
		current = t.Inner()
	}
	sink := state.NewSink(s)
	multi := &multiHandler{handlers: []slog.Handler{current, sink, liveSink}}
	slog.SetDefault(slog.New(state.NewTagger(multi)))
	return nil
}

// newLiveEncoder constructs the per-record encoding closure used by
// LiveSink. Each invocation gets a fresh bytes.Buffer plus a handler
// configured to write into it; we return the captured bytes.
//
// Pretty mode reuses the existing prettyHandler (same logic that drives
// cronicle's stdout rendering); JSON/text reuse the slog built-ins.
// Allocation per record is small and amortizes against the network send.
func newLiveEncoder(format LiveFormat) state.Encoder {
	return func(r slog.Record) []byte {
		var buf bytes.Buffer
		var h slog.Handler
		switch format {
		case LiveFormatJSON:
			h = slog.NewJSONHandler(&buf, nil)
		case LiveFormatText:
			h = slog.NewTextHandler(&buf, nil)
		case LiveFormatPrettyColor:
			h = &liveSinkPrettyHandler{
				fallback:   newTintHandler(&buf),
				out:        &buf,
				forceColor: true,
			}
		default: // pretty (also the empty-string fallback)
			h = &liveSinkPrettyHandler{
				fallback: newTintHandler(&buf),
				out:      &buf,
			}
		}
		_ = h.Handle(context.Background(), r)
		return buf.Bytes()
	}
}

// CloseStateStore tears down the projection store. Safe to call when
// none was ever opened.
func CloseStateStore() error {
	if stateStore == nil {
		return nil
	}
	err := stateStore.Close()
	stateStore = nil
	return err
}

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
	// Preserve Tagger at the top so file records also carry seq+lifetime.
	// Without this, cronicle.jsonl → Loki would lack the cursor IDs the
	// frontend uses to deep-link from live → history.
	if t, ok := current.(*state.Tagger); ok {
		inner := t.Inner()
		combined := &multiHandler{handlers: []slog.Handler{inner, fileHandler}}
		slog.SetDefault(slog.New(state.NewTagger(combined)))
	} else {
		slog.SetDefault(slog.New(&multiHandler{handlers: []slog.Handler{current, fileHandler}}))
	}
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

// ---- pretty handlers -------------------------------------------------------
//
// Two slog.Handler implementations share the same render helpers but
// target different destinations:
//
//   - stdoutPrettyHandler is wired to os.Stdout by SetupLogging when
//     --log-format=pretty. In that mode, an in-process StreamingWriter
//     also writes pretty bytes directly to stdout (bypassing slog) for
//     leader/follower coordination across concurrent tasks. The handler
//     therefore SUPPRESSES the streaming entry_types (stdout_chunk,
//     text_delta, agent_run_start, …) — rendering them too would
//     double-print every line on the terminal.
//
//   - liveSinkPrettyHandler is wired to a per-call bytes.Buffer by
//     newLiveEncoder when --live-format=pretty (the default). LiveSink
//     subscribers (the SSE endpoint) never see the StreamingWriter's
//     bytes, so this handler RENDERS the streaming entry_types — that's
//     the entire reason the SSE consumer can show live token/line output.
//
// The two handlers used to be one type with a `suppressChunks bool`
// field. Splitting them removes per-call branching and keeps each
// Handle implementation top-to-bottom unconditional.

// stdoutPrettyHandler renders to os.Stdout. The StreamingWriter wrote
// streaming-record bytes directly to the same stdout, so this handler
// renders ONLY the structural / lifecycle records that don't have a
// pre-emption path. Streaming entry_types are no-ops.
type stdoutPrettyHandler struct {
	fallback slog.Handler
	out      io.Writer // os.Stdout in normal use; an io.Writer in tests
}

func (p *stdoutPrettyHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return p.fallback.Enabled(ctx, l)
}

func (p *stdoutPrettyHandler) Handle(_ context.Context, r slog.Record) error {
	switch entryType(r) {
	case "agent_run":
		// Non-streaming agent path (stdout is JSON-mode, no
		// StreamingWriter for agent text). Render the full block here.
		return renderAgentRun(p.out, r)
	case "shell_run":
		// Same idea for shell — only fires when the streaming
		// dispatch didn't claim the bytes.
		return renderShellRun(p.out, r)
	case "schedule_start":
		return renderScheduleStart(p.out, r)
	case "schedule_complete":
		return renderScheduleComplete(p.out, r)
	case "agent_run_start", "shell_run_start",
		"agent_run_streamed", "shell_run_streamed",
		"stdout_chunk", "stderr_chunk",
		"text_delta", "thinking_delta",
		"tool_use_start", "tool_result",
		"task_start":
		// StreamingWriter already wrote these bytes; task_start is
		// subsumed by block headers in pretty mode.
		return nil
	default:
		return renderDimLine(p.out, r)
	}
}

func (p *stdoutPrettyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &stdoutPrettyHandler{fallback: p.fallback.WithAttrs(attrs), out: p.out}
}

func (p *stdoutPrettyHandler) WithGroup(name string) slog.Handler {
	return &stdoutPrettyHandler{fallback: p.fallback.WithGroup(name), out: p.out}
}

// liveSinkPrettyHandler renders to a per-call buffer for the SSE wire.
// No pre-emption path exists for the SSE consumer, so every record gets
// turned into visible bytes — header / footer / per-line / per-token.
//
// forceColor=true (set by LiveFormatPrettyColor) toggles fatih/color's
// global NoColor=false for the duration of Handle. This is gated by a
// process-wide mutex so concurrent stdout pretty renders aren't affected
// mid-record. The mutex held window is one Handle call (typically a few
// microseconds); LiveSink fanout is already serialized per subscriber so
// contention is bounded by the number of subscribers, not by traffic.
type liveSinkPrettyHandler struct {
	fallback   slog.Handler
	out        io.Writer // the LiveSink encoder's per-Handle bytes.Buffer
	forceColor bool
}

// liveColorMu serializes color.NoColor flips for force-color live renders.
// Without it, two concurrent live encoder calls could race-toggle the flag
// while a stdout pretty handler is rendering, producing mid-record color
// flipping. The cost is whole-process serialization of force-color renders,
// which is fine: SSE volume isn't bounded by render throughput here.
var liveColorMu sync.Mutex

func (p *liveSinkPrettyHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return p.fallback.Enabled(ctx, l)
}

func (p *liveSinkPrettyHandler) Handle(_ context.Context, r slog.Record) error {
	if p.forceColor {
		liveColorMu.Lock()
		prev := color.NoColor
		color.NoColor = false
		defer func() {
			color.NoColor = prev
			liveColorMu.Unlock()
		}()
	}
	return p.handle(r)
}

func (p *liveSinkPrettyHandler) handle(r slog.Record) error {
	switch entryType(r) {
	case "agent_run_start":
		return renderAgentRunStart(p.out, r)
	case "agent_run", "agent_run_streamed":
		// Body was already streamed via text_delta records; emit just
		// the metrics footer.
		return renderAgentRunFooter(p.out, r)
	case "shell_run_start":
		return renderShellRunStart(p.out, r)
	case "shell_run", "shell_run_streamed":
		return renderShellRunFooter(p.out, r)
	case "schedule_start":
		return renderScheduleStart(p.out, r)
	case "schedule_complete":
		return renderScheduleComplete(p.out, r)
	case "stdout_chunk":
		return renderStdoutChunk(p.out, r, false)
	case "stderr_chunk":
		return renderStdoutChunk(p.out, r, true)
	case "text_delta":
		return renderTextDelta(p.out, r, false)
	case "thinking_delta":
		return renderTextDelta(p.out, r, true)
	case "tool_use_start":
		return renderToolUseStart(p.out, r)
	case "tool_result":
		return renderToolResult(p.out, r)
	case "task_start":
		// Block headers subsume task_start in pretty rendering — the
		// agent_run_start / shell_run_start records carry the same
		// signal with richer context.
		return nil
	default:
		return renderDimLine(p.out, r)
	}
}

func (p *liveSinkPrettyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &liveSinkPrettyHandler{
		fallback:   p.fallback.WithAttrs(attrs),
		out:        p.out,
		forceColor: p.forceColor,
	}
}

func (p *liveSinkPrettyHandler) WithGroup(name string) slog.Handler {
	return &liveSinkPrettyHandler{
		fallback:   p.fallback.WithGroup(name),
		out:        p.out,
		forceColor: p.forceColor,
	}
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
			n = valueInt64(a.Value)
			return false
		}
		return true
	})
	return n
}

// valueInt64 reads a slog.Value as int64, tolerating Float64 (e.g. when an
// integer attr round-tripped through JSON and arrived as float). Falls
// back to 0 for unhandled kinds. Used in place of a bare a.Value.Int64()
// so a kind mismatch (which would otherwise panic) silently degrades.
func valueInt64(v slog.Value) int64 {
	switch v.Kind() {
	case slog.KindInt64, slog.KindUint64:
		return v.Int64()
	case slog.KindFloat64:
		return int64(v.Float64())
	}
	return 0
}

// valueFloat64 mirrors valueInt64 for float-typed attrs (e.g. cost_usd).
// Worker→producer event shipping JSON-encodes Float64(0) as `0`, which
// the rehydrate path then reads as Int64 (json.Number prefers integer
// when it parses cleanly); reading as Float64() would panic.
func valueFloat64(v slog.Value) float64 {
	switch v.Kind() {
	case slog.KindFloat64:
		return v.Float64()
	case slog.KindInt64, slog.KindUint64:
		return float64(v.Int64())
	}
	return 0
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

// renderAgentRunStart writes the bordered header block for an agent run
// to p.out — same shape WriteAgentRunHeader produces for the TTY's
// StreamingWriter. Used by LiveSink (suppressChunks=false) so SSE
// consumers see model/task/schedule + the RESOLVED prompt/system/tools
// BEFORE the deltas stream. Surfacing the resolved prompt lets operators
// see exactly what `${date}`/`${path}` expanded to without digging into
// the transcript.
func renderAgentRunStart(out io.Writer, r slog.Record) error {
	schedule := attrString(r, "schedule")
	task := attrString(r, "task")
	model := attrString(r, "model")
	prompt := attrString(r, "prompt")
	system := attrString(r, "system")
	var skills, tools, mcpCommands []string
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "skills_available":
			if v, ok := a.Value.Any().([]string); ok {
				skills = v
			}
		case "tools":
			if v, ok := a.Value.Any().([]string); ok {
				tools = v
			}
		case "mcp_commands":
			if v, ok := a.Value.Any().([]string); ok {
				mcpCommands = v
			}
		}
		return true
	})
	var b bytes.Buffer
	WriteAgentRunHeader(&b, schedule, task, model, skills, prompt, system, tools, mcpCommands)
	b.WriteByte('\n')
	_, err := out.Write(b.Bytes())
	return err
}

// renderAgentRunFooter writes ONLY the bracketed footer line for an
// agent run — the metrics summary. Used by LiveSink (suppressChunks=
// false): the header was already emitted via agent_run_start, the body
// streamed via text_delta records, so the SSE consumer only needs the
// terminal metrics line.
func renderAgentRunFooter(out io.Writer, r slog.Record) error {
	var (
		costStr                              string
		stop, transcript                     string
		durationMs, in, outTokens, cacheRead int64
	)
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "cost_usd":
			// Worker-shipped events round-trip through JSON; a float value
			// of 0 (or any whole number) arrives as Int64 after the
			// rehydrate path (ingest_rehydrate.go:jsonValueToAttr prefers
			// Int64 when json.Number parses cleanly). Reading it as
			// Float64() would panic. Use the resilient reader.
			costStr = fmt.Sprintf("%.6f", valueFloat64(a.Value))
		case "duration_ms":
			durationMs = valueInt64(a.Value)
		case "input_tokens":
			in = valueInt64(a.Value)
		case "output_tokens":
			outTokens = valueInt64(a.Value)
		case "cache_read":
			cacheRead = valueInt64(a.Value)
		case "stop_reason":
			stop = a.Value.String()
		case "transcript":
			transcript = a.Value.String()
		}
		return true
	})
	var b bytes.Buffer
	WriteAgentRunFooter(&b, in, outTokens, cacheRead, costStr, durationMs, stop, transcript)
	_, err := out.Write(b.Bytes())
	return err
}

// renderShellRunStart writes the bordered header block for a shell run.
// Mirrors renderAgentRunStart for the shell-task path.
func renderShellRunStart(out io.Writer, r slog.Record) error {
	schedule := attrString(r, "schedule")
	task := attrString(r, "task")
	command := attrString(r, "command")
	var b bytes.Buffer
	WriteShellRunHeader(&b, schedule, task, command)
	b.WriteByte('\n')
	_, err := out.Write(b.Bytes())
	return err
}

// renderShellRunFooter writes the bracketed footer line for a shell run.
func renderShellRunFooter(out io.Writer, r slog.Record) error {
	exit := attrInt64(r, "exit")
	durationMs := attrInt64(r, "duration_ms")
	transcript := attrString(r, "transcript")
	var b bytes.Buffer
	WriteShellRunFooter(&b, exit, durationMs, transcript)
	_, err := out.Write(b.Bytes())
	return err
}

func renderAgentRun(out io.Writer, r slog.Record) error {
	var (
		schedule, task, model, response, costStr string
		stop, transcript, errMsg                 string
		prompt, system                           string
		durationMs, in, outTokens, cacheRead     int64
		success                                  bool
		skills, tools, mcpCommands               []string
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
		case "prompt":
			prompt = a.Value.String()
		case "system":
			system = a.Value.String()
		case "cost_usd":
			// cost_usd is a Float64 attr (slog.Float64 in execAgent);
			// fmt to a 6-decimal string for the pretty footer.
			costStr = fmt.Sprintf("%.6f", a.Value.Float64())
		case "duration_ms":
			durationMs = a.Value.Int64()
		case "input_tokens":
			in = a.Value.Int64()
		case "output_tokens":
			outTokens = a.Value.Int64()
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
		case "tools":
			if v, ok := a.Value.Any().([]string); ok {
				tools = v
			}
		case "mcp_commands":
			if v, ok := a.Value.Any().([]string); ok {
				mcpCommands = v
			}
		}
		return true
	})

	var b bytes.Buffer
	WriteAgentRunHeader(&b, schedule, task, model, skills, prompt, system, tools, mcpCommands)
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
	WriteAgentRunFooter(&b, in, outTokens, cacheRead, costStr, durationMs, stop, transcript)

	_, err := out.Write(b.Bytes())
	return err
}

// WriteAgentRunHeader writes the bordered header for an agent run block.
// Shared by the slog renderAgentRun path and the streaming dispatch path.
// `skills` is the available-skills list to surface in the header (empty/nil
// omits the segment). `prompt` and `system` are the RESOLVED (post template
// substitution) strings the agent will actually receive — surfacing them in
// the header tells the whole story of what got dispatched, so operators can
// debug `${date}`/`${path}` expansion at a glance. `tools` and `mcpCommands`
// list the tool surface available for the run.
func WriteAgentRunHeader(w io.Writer, schedule, task, model string, skills []string,
	prompt, system string, tools []string, mcpCommands []string) {
	header := color.New(color.FgCyan, color.Bold).SprintFunc()
	rule := color.New(color.FgCyan).SprintFunc()
	label := color.New(color.FgCyan, color.Faint).SprintFunc()

	headerLine := fmt.Sprintf("agent run · schedule=%s · task=%s · model=%s",
		schedule, task, model)
	if len(skills) > 0 {
		headerLine += " · skills=[" + strings.Join(skills, ",") + "]"
	}
	// Compose the meta lines first so the bar can width-match the widest
	// line in the block — keeps the framing visually balanced when prompt
	// or system is the longest line.
	type metaLine struct {
		key, val string
	}
	var meta []metaLine
	if p := strings.TrimSpace(prompt); p != "" {
		meta = append(meta, metaLine{"prompt", p})
	}
	if s := strings.TrimSpace(system); s != "" {
		meta = append(meta, metaLine{"system", s})
	}
	if len(tools) > 0 {
		meta = append(meta, metaLine{"tools", strings.Join(tools, ", ")})
	}
	for _, mc := range mcpCommands {
		meta = append(meta, metaLine{"mcp", mc})
	}

	width := len(headerLine)
	rendered := make([]string, 0, len(meta))
	for _, m := range meta {
		line := truncate(escapeControl(fmt.Sprintf("%s: %s", m.key, m.val)), 200)
		if len(line) > width {
			width = len(line)
		}
		rendered = append(rendered, line)
	}
	bar := strings.Repeat("━", width+8)

	fmt.Fprintln(w, rule(bar))
	fmt.Fprintln(w, header(headerLine))
	for i, m := range meta {
		// Color the key prefix only; keep the value plain so terminals
		// without color still read naturally and so search-in-page finds
		// the literal prompt/system text.
		colon := strings.Index(rendered[i], ":")
		if colon < 0 {
			fmt.Fprintln(w, rendered[i])
			continue
		}
		_ = m
		fmt.Fprintln(w, label(rendered[i][:colon+1])+rendered[i][colon+1:])
	}
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
// The resolved command (post ${date}/${path}/${scratch} substitution) is
// emitted on its own `command: …` line so the full string is visible — the
// header line stays compact for the schedule/task chip while the command
// line tells the whole story of what got executed. Control chars in the
// command are escaped (newlines for sh -c bodies); the line is capped at
// 200 chars so an enormous shell payload doesn't blow out the framing.
func WriteShellRunHeader(w io.Writer, schedule, task, command string) {
	header := color.New(color.FgCyan, color.Bold).SprintFunc()
	rule := color.New(color.FgCyan).SprintFunc()
	label := color.New(color.FgCyan, color.Faint).SprintFunc()

	headerLine := fmt.Sprintf("shell run · schedule=%s · task=%s", schedule, task)
	cmdLine := "command: " + truncate(escapeControl(command), 200)

	width := len(headerLine)
	if len(cmdLine) > width {
		width = len(cmdLine)
	}
	bar := strings.Repeat("━", width+8)

	fmt.Fprintln(w, rule(bar))
	fmt.Fprintln(w, header(headerLine))
	// Color the "command:" label only; keep the resolved string plain so
	// users can search/copy the exact text without escape codes in the way.
	fmt.Fprintln(w, label("command:")+cmdLine[len("command:"):])
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
	case "git":
		// Render `git <command> <key arg>` so the stream reads naturally:
		// e.g. `→ git: log limit=20`, `→ git: commit "msg…"`.
		cmd, _ := args["command"].(string)
		sub, _ := args["args"].(map[string]any)
		if cmd == "" {
			return raw
		}
		switch cmd {
		case "log", "status", "push":
			if sub == nil || len(sub) == 0 {
				return cmd
			}
		case "branch":
			if name, ok := sub["name"].(string); ok && name != "" {
				return cmd + " " + name
			}
		case "commit":
			if msg, ok := sub["message"].(string); ok && msg != "" {
				return cmd + " " + truncate(escapeControl(msg), 60)
			}
		case "diff":
			if sub == nil {
				return cmd
			}
			from, _ := sub["from"].(string)
			to, _ := sub["to"].(string)
			if from != "" && to != "" {
				return cmd + " " + from + ".." + to
			}
			return cmd
		}
		// Fallback: cmd plus a compact JSON of args.
		if b, err := json.Marshal(sub); err == nil && len(b) > 2 {
			return cmd + " " + truncate(string(b), 60)
		}
		return cmd
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
func renderShellRun(out io.Writer, r slog.Record) error {
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

	var b bytes.Buffer
	WriteShellRunHeader(&b, schedule, task, command)
	b.WriteByte('\n')

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

	_, err := out.Write(b.Bytes())
	return err
}

// renderScheduleStart renders the section header above a schedule's task
// blocks: a horizontal rule, the schedule name, and the DAG list.
func renderScheduleStart(out io.Writer, r slog.Record) error {
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

	_, err := out.Write(b.Bytes())
	return err
}

// renderScheduleComplete renders the summary line at the bottom of a schedule.
func renderScheduleComplete(out io.Writer, r slog.Record) error {
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

	_, err := out.Write(b.Bytes())
	return err
}

// renderDimLine renders a record as a faint single line. Used for lifecycle
// events (Loading, executing tasks, heartbeat, refreshing config, etc.) that
// don't carry a structural entry_type.
// renderStdoutChunk writes one line of streaming task output. The slog
// record carries the line as msg. stderr is rendered in red (matching
// what a TTY would show); stdout is rendered plain.
func renderStdoutChunk(out io.Writer, r slog.Record, isStderr bool) error {
	line := r.Message
	if isStderr {
		// Faint red so error output is visible without overpowering normal
		// stdout. Mirrors what users see when running the command locally.
		errc := color.New(color.FgRed, color.Faint).SprintFunc()
		_, err := fmt.Fprintln(out, errc(line))
		return err
	}
	_, err := fmt.Fprintln(out, line)
	return err
}

// renderTextDelta writes one chunk of agent output. Text deltas are
// rendered inline (no decoration, no trailing newline added — the model
// controls its own newlines). Thinking deltas are dimmed and prefixed
// once per turn-start, matching the existing NewAgentStreamRenderer
// behavior so the live SSE pane reads identically to the local TTY.
func renderTextDelta(out io.Writer, r slog.Record, isThinking bool) error {
	if isThinking {
		dim := color.New(color.Faint).SprintFunc()
		_, err := fmt.Fprint(out, dim(r.Message))
		return err
	}
	_, err := fmt.Fprint(out, r.Message)
	return err
}

// renderToolUseStart writes the highlighted "→ tool: <input>" line that
// the agent stream renderer used to write directly to stdout. Reads
// the attrs the bridge emits: tool, input.
func renderToolUseStart(out io.Writer, r slog.Record) error {
	toolHi := color.New(color.FgYellow).SprintFunc()
	tool := attrString(r, "tool")
	input := attrString(r, "input")
	display := displayToolName(tool)
	formatted := formatToolInput(tool, input)
	_, err := fmt.Fprintf(out, "\n%s%s%s %s\n",
		toolHi("→ "), toolHi(display), toolHi(":"), formatted)
	return err
}

// renderToolResult writes the colored result line. Exit / duration come
// from attrs; is_error flips green→red.
func renderToolResult(out io.Writer, r slog.Record) error {
	dim := color.New(color.Faint).SprintFunc()
	ok := color.New(color.FgGreen, color.Faint).SprintFunc()
	bad := color.New(color.FgRed, color.Faint).SprintFunc()
	isError := attrBool(r, "is_error")
	duration := attrInt64(r, "duration_ms")
	marker := ok("← exit=0")
	if isError {
		marker = bad("← error")
	}
	_, err := fmt.Fprintf(out, "%s %s\n", marker, dim(formatDuration(duration)))
	return err
}

func renderDimLine(out io.Writer, r slog.Record) error {
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

	_, err := out.Write(b.Bytes())
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
