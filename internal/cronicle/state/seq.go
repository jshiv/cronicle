package state

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sync/atomic"
)

// Sequence numbering for the slog event stream.
//
// Tagger sits at the top of the slog handler chain and stamps every
// record with two attrs before fan-out to pretty/file/Sink/LiveSink:
//
//   - seq      — process-monotonic int64, starts at 1, increments per record.
//   - lifetime — 8-char hex nonce minted at process startup.
//
// These attrs flow into cronicle.jsonl (and from there into Loki) so the
// frontend can deep-link between the live SSE pane and a Loki query at
// the same cursor:  "I see seq=42 in live, take me to seq=42 in history."
// They also let any consumer detect missed records (gap in seq) within
// a single process lifetime, and distinguish two processes' streams
// (different lifetime) when correlating across restarts.
//
// What this is NOT (this time):
//
//   - NOT a de-dup key for SSE replay. Live is live; no replay path
//     exists, so de-dup isn't required.
//   - NOT written to the events table. The slim chronology ledger
//     (id, run_id, task, entry_type, ts) is sufficient for ordering;
//     seq/lifetime would be redundant there.

// processNonce is minted lazily on first read. Stable for the lifetime
// of this process. crypto/rand avoids any chance of collision across
// producer restarts that happen within the same wall second.
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
	// Mint once. CAS dance avoids importing sync.Once for one call site;
	// with a single producer this path runs once and never contends.
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

// nextSeq is the per-process monotonic counter. Each Tagger.Handle call
// increments it by one, so seq values are dense (no gaps) and globally
// strictly increasing within the process.
var nextSeq atomic.Int64

// Tagger is the top-of-chain slog handler. It mints (lifetime, seq)
// for every record and injects them as attrs before delegating. All
// sibling handlers (pretty, JSON file, Sink, LiveSink) observe the
// SAME tagged record — so the seq emitted in the live SSE pane
// matches the seq written to the JSONL log byte-for-byte.
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
// not just run-bearing ones — operators want monotonic counters on
// lifecycle prints too (proves no records were dropped between
// handlers).
func (t *Tagger) Handle(ctx context.Context, r slog.Record) error {
	seq := nextSeq.Add(1)
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
