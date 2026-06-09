package cronicle

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/jshiv/cronicle/pkg/agent"
)

// maxLoggedToolInputBytes caps the byte length of an agent tool input
// when it's emitted to the slog stream (file/Loki + live SSE). The full
// input is preserved in the per-run agent transcript at
// .cronicle/runs/{run_id}-{task}.jsonl (which is 0o600 since PR fixing
// M5), so this truncation only restricts the observability fan-out — it
// does not lose audit fidelity.
//
// Why truncate at all: an agent's bash command could embed a token or
// header value in its args (e.g. `curl -H "Authorization: Bearer X"`)
// from a tool_result it composed in a prior turn. Without truncation
// the full command — including the secret — ships to Loki and the
// frontend SSE stream. Head-truncation leaves the most diagnostic part
// (the start of the command) intact while bounding the leak surface.
//
// 256 bytes is comfortably above any normal text_editor.path or git
// arg, while short enough that pasting a typical secret-bearing bash
// line (which usually leads with the secret-bearing flag) still loses
// most of the value.
const maxLoggedToolInputBytes = 256

// redactToolInput returns a string suitable for slog emission from an
// agent tool's raw JSON input. Short inputs pass through unchanged;
// long inputs are truncated to maxLoggedToolInputBytes with an explicit
// marker showing how many bytes were dropped. The full input remains in
// the agent transcript file for post-hoc audit.
func redactToolInput(input string) string {
	if len(input) <= maxLoggedToolInputBytes {
		return input
	}
	return fmt.Sprintf("%s... [+%d bytes truncated; see transcript for full]",
		input[:maxLoggedToolInputBytes],
		len(input)-maxLoggedToolInputBytes)
}

// lineEmitter wraps an io.Writer to emit one slog record per newline
// AS BYTES FLOW THROUGH. The underlying writer (typically the
// StreamingWriter that fronts stdout in pretty mode) still receives
// the bytes unchanged, so the operator's terminal renders the output
// in real time the same way it always did.
//
// Each completed line is also emitted as a slog.Info record with
// entry_type=stdout_chunk (or stderr_chunk), feeding three downstream
// destinations through the slog chain:
//
//   - file (cronicle.jsonl → Loki) — durable history, queryable
//   - LiveSink → SSE — feeds the frontend's live xterm.js pane
//   - Sink filter drops these (see state/sink.go skipEntryTypes)
//
// Partial lines (no trailing \n) are held in the internal buffer until
// the next Write call brings the newline. Flush emits whatever's left
// when the command exits — the final printf-without-newline case.
//
// The slog records carry run_id + task + schedule so LiveSink can
// route by run, and Loki can label / search by them.
type lineEmitter struct {
	inner    io.Writer
	runID    string
	task     string
	schedule string
	stream   string // "stdout" | "stderr"

	mu  sync.Mutex // guards buf (Write may run on multiple goroutines for stderr+stdout)
	buf []byte
}

// newLineEmitter wraps inner. stream must be "stdout" or "stderr" — it
// becomes the prefix of the emitted entry_type.
func newLineEmitter(inner io.Writer, runID, task, schedule, stream string) *lineEmitter {
	return &lineEmitter{
		inner:    inner,
		runID:    runID,
		task:     task,
		schedule: schedule,
		stream:   stream,
	}
}

// Write forwards bytes to the inner writer first (so the terminal
// rendering is unaffected), then splits the incoming bytes plus any
// buffered partial line on '\n' and emits one slog record per
// completed line. The return value reports what the inner writer
// accepted, so exec's plumbing semantics stay intact.
func (e *lineEmitter) Write(p []byte) (int, error) {
	n, err := e.inner.Write(p)
	// Even if the inner writer failed partway, emit slog for whatever
	// bytes it did accept — better partial telemetry than none.
	if n > 0 {
		e.consume(p[:n])
	}
	return n, err
}

// consume buffers bytes and emits one slog record per newline. Holds
// the mutex briefly to keep stdout and stderr writers from interleaving
// half-lines into each other's buffers.
// maxLineEmitterBufBytes caps the unflushed buffer that a stream emits
// no-newline bytes into. A subprocess that writes a long binary blob
// or a progress bar with carriage-return updates can otherwise grow
// this buffer without bound. When the cap is exceeded we force-emit
// the current buffer as one synthetic line — operators see the partial
// output rather than the process OOMing on multi-GB no-newline streams.
//
// 1 MiB is well above any reasonable single-line output (the maximum
// bash truncation budget is 30 KiB) and small enough that even
// pathological output is bounded to that per Flush window.
const maxLineEmitterBufBytes = 1 << 20

func (e *lineEmitter) consume(p []byte) {
	e.mu.Lock()
	e.buf = append(e.buf, p...)
	for {
		i := bytes.IndexByte(e.buf, '\n')
		if i < 0 {
			break
		}
		line := string(e.buf[:i])
		e.buf = e.buf[i+1:]
		e.emit(line)
	}
	// Cap the unflushed buffer (M13): a stream that never emits a
	// newline used to grow this buffer indefinitely. Force-emit the
	// current contents as one synthetic line so memory stays bounded.
	if len(e.buf) > maxLineEmitterBufBytes {
		e.emit(string(e.buf) + " [truncated: no newline]")
		e.buf = e.buf[:0]
	}
	e.mu.Unlock()
}

// Flush emits any buffered partial line (a command that printed without
// a trailing newline before exiting). Idempotent — leaves an empty
// buffer.
func (e *lineEmitter) Flush() {
	e.mu.Lock()
	if len(e.buf) > 0 {
		e.emit(string(e.buf))
		e.buf = e.buf[:0]
	}
	e.mu.Unlock()
}

// emit publishes one slog record with the line as msg. entry_type
// drives routing: pretty handler suppresses it for stdout (already
// rendered by the streaming writer), LiveSink encodes it as a frame,
// Loki indexes it.
func (e *lineEmitter) emit(line string) {
	slog.Info(line,
		"entry_type", e.stream+"_chunk",
		"run_id", e.runID,
		"task", e.task,
		"schedule", e.schedule,
		"stream", e.stream,
	)
}

// NewAgentSlogBridge returns an agent.StreamHandler that fans each
// stream event out to slog AND to the optional `downstream` handler.
//
// The slog records (entry_type=text_delta / thinking_delta /
// tool_use_start / tool_result / turn_start) flow through the normal
// handler chain: file → Loki for history, LiveSink → SSE for the
// frontend's live pane. The pretty handler on stdout SUPPRESSES these
// types (suppressChunks=true) because `downstream` — when set — is the
// existing NewAgentStreamRenderer writing directly to the stream-pretty
// writer; rendering them again from slog would double-print on stdout.
//
// When stdout isn't in pretty mode, downstream is nil; nothing renders
// to stdout, but Loki + LiveSink still see every delta. That's exactly
// the design contract: live SSE consumers get the visceral experience
// regardless of how the operator configured stdout.
func NewAgentSlogBridge(runID, task, schedule string, downstream agent.StreamHandler) agent.StreamHandler {
	return func(e agent.StreamEvent) {
		switch e.Type {
		case agent.StreamEventTextDelta:
			slog.Info(e.Text,
				"entry_type", "text_delta",
				"run_id", runID, "task", task, "schedule", schedule,
			)
		case agent.StreamEventThinkingDelta:
			slog.Info(e.Text,
				"entry_type", "thinking_delta",
				"run_id", runID, "task", task, "schedule", schedule,
			)
		case agent.StreamEventToolUseStart:
			slog.Info("tool use start",
				"entry_type", "tool_use_start",
				"run_id", runID, "task", task, "schedule", schedule,
				"tool", e.ToolName,
				"input", redactToolInput(e.ToolInput),
			)
		case agent.StreamEventToolResult:
			slog.Info("tool result",
				"entry_type", "tool_result",
				"run_id", runID, "task", task, "schedule", schedule,
				"tool", e.ToolName,
				"is_error", e.IsError,
				"duration_ms", e.DurationMs,
			)
		case agent.StreamEventTurnStart:
			slog.Info("turn start",
				"entry_type", "turn_start",
				"run_id", runID, "task", task, "schedule", schedule,
				"turn_index", e.TurnIndex,
			)
		}
		if downstream != nil {
			downstream(e)
		}
	}
}
