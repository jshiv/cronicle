package state

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sync/atomic"
)

// Sequence numbering for the event stream.
//
// Every slog record that flows through the multi-handler chain gets
// two extra attrs injected by Tagger at the top of the chain:
//
//   - seq      — process-monotonic int64, starts at 1, increments per record
//   - lifetime — 8-char hex nonce minted at process startup
//
// These are the de-dup key for the SSE event stream. The replay path
// reads them back from the events table (schema v4 columns); the live
// path emits them in the encoded JSON line. Same record on both paths
// → same (lifetime, seq) → trivial client-side de-dup.
//
// Why lifetime: process restart resets seq to 1, so a client reconnecting
// with Last-Event-ID containing the OLD seq would otherwise risk
// "matching" the wrong record. With a different lifetime, the server
// detects the mismatch and replays everything for the run from scratch.

// processNonce is minted lazily on first read. It's stable for the
// lifetime of this process. Using crypto/rand to avoid any chance of
// collision across producer restarts.
var (
	nonceOnce  atomic.Pointer[string]
	nonceMutex atomic.Bool // light spin-lock guarding the one-time mint
)

// ProcessNonce returns the 8-char hex lifetime tag for this process.
// First call mints it; subsequent calls return the cached value.
func ProcessNonce() string {
	if p := nonceOnce.Load(); p != nil {
		return *p
	}
	// Mint once. The CAS dance avoids a sync.Once import here so the
	// package keeps its slim imports; with a single producer this path
	// runs once and never contends.
	for !nonceMutex.CompareAndSwap(false, true) {
	}
	defer nonceMutex.Store(false)
	if p := nonceOnce.Load(); p != nil {
		return *p
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	n := hex.EncodeToString(b[:])
	nonceOnce.Store(&n)
	return n
}

// nextSeq is the per-process monotonic counter. Each call to Tagger.Handle
// increments it by one, so seq values are dense (no gaps) and globally
// strictly increasing within the process.
var nextSeq atomic.Int64

// Tagger is the top-of-chain slog handler. It mints (lifetime, seq)
// for every record, injects them as attrs, and delegates to the
// downstream chain. All sibling handlers (pretty, json file, Sink,
// LiveSink) observe the SAME tagged record — guarantees that the seq
// emitted on SSE live exactly matches the seq written to events table
// by Sink.
//
// The wrapped handler is the existing multi-handler that fans out to
// stdout/file/projection/live. Tagger sits one level above that.
type Tagger struct {
	inner slog.Handler
}

// NewTagger wraps inner. Callers point slog.Default at the returned
// Tagger. See log.go where it's wired in.
func NewTagger(inner slog.Handler) *Tagger { return &Tagger{inner: inner} }

// Inner returns the wrapped handler. Used by EnableStateStore to peel
// the Tagger off when re-installing the chain so consecutive enables
// don't stack two Taggers (which would double-tag every record).
func (t *Tagger) Inner() slog.Handler { return t.inner }

func (t *Tagger) Enabled(ctx context.Context, l slog.Level) bool {
	return t.inner.Enabled(ctx, l)
}

// Handle injects seq + lifetime attrs and delegates. Tag every record,
// not just run-bearing ones: it makes the JSONL log uniformly tagged
// and gives operators a monotonic count to verify no records were
// dropped between handlers.
func (t *Tagger) Handle(ctx context.Context, r slog.Record) error {
	seq := nextSeq.Add(1)
	// Clone to avoid mutating the caller's record (slog handler
	// convention: handlers may mutate Time and attrs only on their own
	// copy of the record).
	r2 := r.Clone()
	r2.AddAttrs(
		slog.Int64("seq", seq),
		slog.String("lifetime", ProcessNonce()),
	)
	return t.inner.Handle(ctx, r2)
}

func (t *Tagger) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Tagger{inner: t.inner.WithAttrs(attrs)}
}

func (t *Tagger) WithGroup(name string) slog.Handler {
	return &Tagger{inner: t.inner.WithGroup(name)}
}

// Retag re-stamps a pre-encoded JSONL line with this process's
// (lifetime, seq), producing a new line. Used by the ingest path
// (POST /v1/events) where events arrive as bytes carrying a remote
// worker's seq/lifetime. We re-tag in P's space so the events table
// and the SSE stream are uniformly in P's identity.
//
// Returns the re-encoded line and the assigned seq. If the input
// isn't a valid JSON object, returns the input unchanged and seq=0.
func Retag(line []byte) ([]byte, int64) {
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		return line, 0
	}
	seq := nextSeq.Add(1)
	m["seq"] = seq
	m["lifetime"] = ProcessNonce()
	// Preserve the original worker tags if any, so operators can trace
	// "this row in producer P came from worker W's seq=5 record".
	// Skip when worker's seq is missing (e.g., this is a producer-local
	// line being re-tagged for some reason).
	if origSeq, ok := line2GetField(line, "seq"); ok && m["origin_seq"] == nil {
		m["origin_seq"] = origSeq
	}
	if origLT, ok := line2GetField(line, "lifetime"); ok && m["origin_lifetime"] == nil {
		m["origin_lifetime"] = origLT
	}
	out, err := json.Marshal(m)
	if err != nil {
		return line, 0
	}
	return out, seq
}

// line2GetField is a small helper to pull a top-level JSON field
// without re-decoding the whole record. Used by Retag to preserve
// origin tags. Returns the json.Number / string / float64 / bool
// directly from json.Unmarshal of a fresh map (cheap; called only on
// the ingest path, not the hot slog path).
func line2GetField(line []byte, key string) (any, bool) {
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		return nil, false
	}
	v, ok := m[key]
	return v, ok
}
