package state

import (
	"context"
	"log/slog"
	"sync"
)

// LiveSink is the in-memory pub/sub for the visceral live-stream plane.
//
// Architecture: cronicle keeps three distinct planes for run telemetry,
// each tuned to a different consumer:
//
//   - State (SQLite runs/tasks/events tables) — answers "what is the
//     status of every run?" Small, indexed, queryable.
//   - History (cronicle.jsonl → Loki) — answers "show me the logs of
//     this failed task last Tuesday." Structured, searchable, retained.
//   - LIVE (this) — answers "what is happening RIGHT NOW in this
//     task?" Token-by-token, line-by-line, ANSI-colored, no retention.
//     If you disconnect, you miss it; switch to Loki for history.
//
// LiveSink sits in the slog handler chain alongside Sink and the file
// handler. Every record flows through it, gets format-encoded (pretty
// by default), and is fanned out to subscribers indexed by run_id.
// Subscribers receive pre-rendered bytes — the SSE handler ships them
// straight to the wire without further encoding.
//
// Wire format is whatever the Encoder produces. The default pretty
// encoder emits ANSI-colored multi-line text identical to what your
// local terminal shows when cronicle runs there. A frontend with
// xterm.js (or any terminal emulator widget) renders those bytes
// faithfully — the operator feels like they're watching the run on
// the producer's TTY.
//
// LiveSink is concurrency-safe. The pub/sub mutex is held only for
// the subscriber-set walk; channel sends happen under no lock so a
// slow consumer's drop costs only the drop, not other subscribers.
type LiveSink struct {
	encode Encoder

	subsMu sync.Mutex
	subs   map[*liveSub]struct{}
}

// Encoder turns a slog.Record into the wire bytes a subscriber will
// receive. Implementations are stateless and goroutine-safe; LiveSink
// calls Encoder once per Handle. Bytes can include newlines — the SSE
// layer splits them into multiple `data:` lines per the spec.
//
// Three Encoders are provided by the cronicle package: pretty (ANSI
// multi-line, default), json (one compact JSON object), text (logfmt-
// ish). Operators pick at startup via --live-format.
type Encoder func(slog.Record) []byte

// liveSub is one subscription. runID="" matches every run (firehose);
// otherwise this subscriber only receives records carrying that run_id.
// task filtering is intentionally not in the type — a frontend usually
// wants the whole run's stream so it can show schedule_start,
// task_start, output, and schedule_complete in one pane.
type liveSub struct {
	ch    chan []byte
	runID string
}

// NewLiveSink returns a handler that encodes via `encode` and fans out
// to subscribers. Pass nil and the handler will silently drop everything
// (useful as a placeholder when state isn't enabled).
func NewLiveSink(encode Encoder) *LiveSink {
	return &LiveSink{encode: encode}
}

// Subscribe registers a per-run consumer. Pass runID="" for the firehose.
// Returns the receive channel and an unsubscribe func the caller MUST
// defer to clean up — leaks compound when many SSE connections come
// and go.
//
// Channel buffer is 256. Empirically that's >5x the burst rate of a
// fast schedule_complete chain when per-line stdout chunks are
// flowing. A consumer that falls further behind drops records — the
// frontend's reaction is "switch to Loki to see what you missed."
func (s *LiveSink) Subscribe(runID string) (<-chan []byte, func()) {
	sub := &liveSub{ch: make(chan []byte, 256), runID: runID}
	s.subsMu.Lock()
	if s.subs == nil {
		s.subs = make(map[*liveSub]struct{})
	}
	s.subs[sub] = struct{}{}
	s.subsMu.Unlock()

	// Unsubscribe is idempotent. We delete-then-skip-close so a publish
	// in flight at the moment of unsubscribe can't see a closed channel —
	// the channel is abandoned and GC'd once the caller drops its ref.
	var unsubOnce sync.Once
	return sub.ch, func() {
		unsubOnce.Do(func() {
			s.subsMu.Lock()
			delete(s.subs, sub)
			s.subsMu.Unlock()
		})
	}
}

// Enabled is true for every level — the live stream wants WARN/ERROR
// from within a task too (e.g., "tool failed" diagnostics), not just
// INFO. The per-record run_id presence is the only gate.
func (s *LiveSink) Enabled(_ context.Context, _ slog.Level) bool { return true }

// Handle is the slog.Handler entry point. Extract run_id, drop if
// absent, encode via the injected Encoder, fan out to matching
// subscribers. Never returns an error — slog handlers in a multi-
// chain are best-effort; a return would short-circuit siblings.
func (s *LiveSink) Handle(_ context.Context, r slog.Record) error {
	if s == nil || s.encode == nil {
		return nil
	}
	runID := extractRunID(r)
	if runID == "" {
		// Process-level lifecycle prints ("config loaded", heartbeat) don't
		// belong to any specific run; the SSE endpoint is per-run.
		return nil
	}
	line := s.encode(r)
	if len(line) == 0 {
		return nil
	}
	s.fanout(runID, line)
	return nil
}

// WithAttrs / WithGroup are no-ops. cronicle doesn't construct
// logger.With(...) chains for projection-bearing events; supporting
// them would add cost and complexity without payoff.
func (s *LiveSink) WithAttrs(_ []slog.Attr) slog.Handler { return s }
func (s *LiveSink) WithGroup(_ string) slog.Handler      { return s }

// Inject forwards a pre-encoded byte sequence to subscribers as if it
// had come through Handle. Used by the distributed-mode ingest path
// (POST /v1/events) where worker records arrive at the producer as
// bytes, not through the producer's slog handler chain, so Handle
// never sees them.
func (s *LiveSink) Inject(runID string, line []byte) {
	if s == nil || runID == "" || len(line) == 0 {
		return
	}
	s.fanout(runID, line)
}

// fanout walks the subscriber set under the mutex, then sends under no
// lock. Slow consumers get a non-blocking drop — better to lose a few
// frames in one tab than stall every other tab watching the same run.
func (s *LiveSink) fanout(runID string, line []byte) {
	s.subsMu.Lock()
	if len(s.subs) == 0 {
		s.subsMu.Unlock()
		return
	}
	targets := make([]*liveSub, 0, len(s.subs))
	for sub := range s.subs {
		if sub.runID == "" || sub.runID == runID {
			targets = append(targets, sub)
		}
	}
	s.subsMu.Unlock()

	for _, t := range targets {
		select {
		case t.ch <- line:
		default:
			// slow consumer — drop. The frontend's recourse is Loki for
			// the missing window, not a buffered replay we'd have to keep.
		}
	}
}

// extractRunID walks the record's attrs looking for `run_id`. Returns
// "" if absent. Cheap for the common case (run_id near the front of
// the attr slice); slog records have small attr slices in practice.
func extractRunID(r slog.Record) string {
	var out string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "run_id" {
			out = a.Value.String()
			return false
		}
		return true
	})
	return out
}
