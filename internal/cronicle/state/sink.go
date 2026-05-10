package state

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
)

// Sink is a slog.Handler that converts each record into an Event and
// folds it into a Store. It composes into the multi-handler chain
// alongside the JSONL file writer and stdout — the same record lands
// in both places. Errors during Apply() are tracked atomically (no
// per-event log spam) so a transient DB write hiccup doesn't take
// down the run that's actually doing the work.
type Sink struct {
	store *Store
	errs  atomic.Int64
}

// NewSink returns a slog.Handler wired to store. Pass nil store and the
// sink becomes a noop — useful for tests that don't care about the
// projection but still construct a Sink to satisfy the handler chain.
func NewSink(store *Store) *Sink { return &Sink{store: store} }

// Errors returns the running count of Apply() failures since process
// start. Useful for /healthz to surface "state plane degraded" without
// flapping on individual writes.
func (s *Sink) Errors() int64 { return s.errs.Load() }

// Enabled accepts everything; the per-record EntryType filter happens
// inside Handle. We could short-circuit on slog.LevelDebug but the
// projection is happy to drop debug-level records anyway since they
// rarely carry entry_type.
func (s *Sink) Enabled(_ context.Context, _ slog.Level) bool { return true }

// Handle converts r → JSON line → Event → store.Apply. The roundtrip
// through JSON keeps Sink consistent with the wire format used in
// Phase 2 (POST /v1/events): the same encoder that produces the file
// log lines produces the bytes we project. No second source of truth
// for "what an event looks like."
func (s *Sink) Handle(ctx context.Context, r slog.Record) error {
	if s == nil || s.store == nil {
		return nil
	}
	// Quick discard for records without entry_type — the bulk of slog
	// traffic is lifecycle prints (Loading config, heartbeat, …) we
	// don't project. Saves the JSON marshal on the hot path.
	hasEntry := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "entry_type" {
			hasEntry = true
			return false
		}
		return true
	})
	if !hasEntry {
		return nil
	}
	line, err := encodeRecord(r)
	if err != nil {
		s.errs.Add(1)
		return nil
	}
	ev, ok := DecodeEvent(line)
	if !ok {
		// Missing run_id, or malformed — already counted above when DecodeEvent
		// rejects malformed input by parse error. ok=false on missing run_id
		// is benign; don't bump the error counter.
		return nil
	}
	if err := s.store.Apply(ev); err != nil {
		s.errs.Add(1)
	}
	return nil
}

// WithAttrs is a noop. cronicle doesn't construct logger.With(...) chains
// for projection-bearing events, so propagating attrs across handler
// clones isn't needed. Returning the same Sink avoids copying the
// embedded atomic counter.
func (s *Sink) WithAttrs(_ []slog.Attr) slog.Handler { return s }

// WithGroup is a noop — the projection ignores grouping. cronicle doesn't
// use groups; supporting them here would add complexity without payoff.
func (s *Sink) WithGroup(_ string) slog.Handler { return s }

// encodeRecord serializes the record (plus any prepended attrs from
// WithAttrs) as one compact JSON object — same shape as the JSONL file
// log. We don't reuse slog.NewJSONHandler because we want the bytes
// back rather than written to a Writer.
func encodeRecord(r slog.Record) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	m := map[string]any{
		"time":  r.Time,
		"level": r.Level.String(),
		"msg":   r.Message,
	}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.Any()
		return true
	})
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	// json.Encoder appends '\n'; trim for caller convenience (DecodeEvent
	// is happy either way but Raw() looks cleaner without a trailing nl).
	out := buf.Bytes()
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out, nil
}
