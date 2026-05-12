// Package configsource pulls cronicle.hcl bytes from a pluggable
// backing store: a local file, an HTTP(S) endpoint, or (later) an
// object store. The runtime polls Source.Fetch on its own ticker —
// separate from the heartbeat — so config refresh can be aggressive
// (sub-second) without spamming the heartbeat observability channel.
//
// Etag handling is opaque per-source. Callers cache the etag from the
// previous fetch and pass it back as prevEtag; sources that can
// short-circuit unchanged content (HTTP If-None-Match, S3 ETag, DB
// version column) return changed=false without touching network/disk
// beyond the conditional probe.
//
// Errors at fetch time are transient by convention — the cron-loop
// caller logs and continues; the previous good config stays loaded.
// Only hard parse errors in cronicle's own pipeline are treated as
// "keep previous, surface the diagnostic".
package configsource

import (
	"context"
	"errors"
)

// ErrNotFound is the sentinel returned by Source.Fetch when the source
// itself is absent (file missing, HTTP 404, S3 NoSuchKey). The
// cron-loop treats this distinctly from a transient I/O error so it
// can warn once and keep retrying on the refresh tick.
var ErrNotFound = errors.New("config source not found")

// Source is the pluggable backing store for cronicle.hcl. Implementations
// must be safe for concurrent Fetch calls (the cron-loop is single-
// threaded today, but tests exercise concurrent paths).
type Source interface {
	// Fetch returns the current config bytes plus an opaque etag.
	//
	// When the underlying content hasn't changed since prevEtag,
	// implementations should return (nil, prevEtag, false, nil) so the
	// caller can skip the parse step on every refresh tick.
	//
	// On the first call (prevEtag == ""), implementations always return
	// the full content with changed=true.
	Fetch(ctx context.Context, prevEtag string) (bytes []byte, etag string, changed bool, err error)

	// String returns a short, redacted description for log lines —
	// includes the scheme + target but never credentials.
	String() string
}
