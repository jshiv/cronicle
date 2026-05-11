package state

import (
	"context"
	"log/slog"
	"sync"
)

// LiveSink is a slog.Handler that forwards each record carrying a
// `run_id` attr to subscribed channels. It sits alongside Sink in the
// multi-handler chain — they share encodeRecord for byte-for-byte
// payload parity but are independent: a Sink/Apply failure does not
// stop the live stream, and a slow LiveSink subscriber does not stall
// the projection write.
//
// Filter rule: records with `run_id` pass; records without are dropped.
// That distinction is intentional — process-level lifecycle prints
// ("config loaded", "Starting cron…") don't belong to any run, and the
// SSE endpoint is per-run.
//
// Wire format: subscribers receive the encoded JSON line, NOT the
// decoded Event struct. Two reasons:
//
//  1. Identical to what the file log and the events table store —
//     replay and live emit the same bytes.
//  2. Forward-compat — adding new slog attrs (e.g. a future
//     entry_type=text_delta for per-token agent streaming) reaches
//     subscribers automatically without a typed-Event migration.
//
// LiveSink is concurrency-safe. The pub/sub mutex is held only for
// the subscriber-set walk; the actual channel sends happen under no
// lock, so a slow consumer's `select-default` drop costs only the
// drop, not other subscribers' deliveries.
type LiveSink struct {
	subsMu sync.Mutex
	subs   map[*liveSub]struct{}
}

// liveSub is one subscription. runID="" matches every run (firehose);
// otherwise the sub only receives lines from that run.
type liveSub struct {
	ch    chan []byte
	runID string
}

// NewLiveSink returns a ready-to-use handler. Zero subscribers means
// Handle is a near-no-op (one map-len check + one runID extract).
func NewLiveSink() *LiveSink { return &LiveSink{} }

// Subscribe registers a per-run live consumer. Pass runID="" for the
// firehose. Returns the receive channel + an unsubscribe func the
// caller MUST defer to clean up — leaks compound when many SSE
// connections come and go.
//
// Channel buffer is 64. Empirically that's >10x the burst rate of a
// fast schedule_complete chain (5–8 events in <100ms). Subscribers
// that fall further behind get dropped messages — the events table
// replay covers the gap on reconnect.
func (s *LiveSink) Subscribe(runID string) (<-chan []byte, func()) {
	sub := &liveSub{ch: make(chan []byte, 64), runID: runID}
	s.subsMu.Lock()
	if s.subs == nil {
		s.subs = make(map[*liveSub]struct{})
	}
	s.subs[sub] = struct{}{}
	s.subsMu.Unlock()

	// Unsubscribe is idempotent. We delete-then-skip-close so a
	// publish in flight at the moment of unsubscribe can't see a
	// closed channel — the channel is abandoned and GC'd once the
	// caller drops its reference.
	var unsubOnce sync.Once
	return sub.ch, func() {
		unsubOnce.Do(func() {
			s.subsMu.Lock()
			delete(s.subs, sub)
			s.subsMu.Unlock()
		})
	}
}

// Enabled is true for every level — the projection wants warnings and
// errors too. LiveSink is filtering by run_id presence, not by level.
func (s *LiveSink) Enabled(_ context.Context, _ slog.Level) bool { return true }

// Handle is the slog.Handler entry point. Extract run_id, drop if
// absent, encode via the shared encoder, fan out to matching
// subscribers. Never returns an error — slog handlers in the
// multi-handler chain should be best-effort; a return would
// short-circuit subsequent handlers.
func (s *LiveSink) Handle(_ context.Context, r slog.Record) error {
	runID := extractRunID(r)
	if runID == "" {
		return nil
	}
	line, err := encodeRecord(r)
	if err != nil {
		return nil
	}
	s.fanout(runID, line)
	return nil
}

// WithAttrs and WithGroup are no-ops. cronicle doesn't construct
// logger.With(...) chains for projection-bearing events; supporting
// them would add cost and complexity without payoff.
func (s *LiveSink) WithAttrs(_ []slog.Attr) slog.Handler { return s }
func (s *LiveSink) WithGroup(_ string) slog.Handler      { return s }

// Inject forwards a pre-encoded JSONL line to subscribers as if it
// had come through Handle. Used by the distributed-mode ingest path
// (POST /v1/events): worker events arrive at the producer as bytes,
// NOT through the producer's slog handler chain, so Handle never
// sees them. Inject closes that gap.
//
// runID must already be extracted by the caller (the ingest path
// has it from the decoded state.Event).
func (s *LiveSink) Inject(runID string, line []byte) {
	if runID == "" {
		return
	}
	s.fanout(runID, line)
}

// fanout is the shared dispatch path for Handle and Inject. Walks the
// subscriber set under the mutex (cheap; tiny map), then sends under no
// lock. Slow consumers get a non-blocking drop.
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
			// slow consumer; the SSE replay path covers the gap when
			// the client reconnects with a Last-Event-ID resume.
		}
	}
}

// extractRunID walks the record's attrs looking for `run_id`. Returns
// "" if absent. Cheap for the common case (records with run_id near
// the front of the attr slice); the slice is always small.
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
