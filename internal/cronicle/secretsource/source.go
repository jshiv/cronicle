// Package secretsource pulls a name → value map from a pluggable
// backing store: a JSON file on disk, or an HTTP endpoint. Mirrors
// internal/cronicle/configsource — same etag-based polling contract,
// same source interface shape, so the runtime can refresh secrets
// with the same machinery it already uses for HCL config.
//
// What this package is NOT: a long-term secret manager. Values flow
// through process memory only; the source decides how plaintext is
// retrieved (the canonical platform shape is "talk to a control plane
// that holds ciphertext + DEK and returns plaintext over an
// authenticated channel"). This package is the plumbing in cronicle,
// not the storage.
//
// Threading: Source.Fetch may be called concurrently. The cron loop is
// single-threaded today, but tests exercise concurrent paths.
package secretsource

import (
	"context"
	"errors"
)

// ErrNotFound is the sentinel returned by Source.Fetch when the source
// itself is absent — the file doesn't exist, the HTTP endpoint returns
// 404. Distinct from a transient I/O error: a refresh loop should warn
// once and continue retrying without dropping the cached map.
var ErrNotFound = errors.New("secret source not found")

// ErrUnauthorized is returned by HTTPSource when the server rejects
// the request (401/403). Distinct because callers usually want to
// surface this immediately — a token-rotation issue won't fix itself
// on the next tick.
var ErrUnauthorized = errors.New("secret source rejected credentials")

// Source is the pluggable backing store for cronicle's secret map.
//
// Implementations MUST:
//   - Return (values, etag, true, nil) on the first call (prevEtag == "")
//     with the full map.
//   - Return (nil, prevEtag, false, nil) when the underlying content is
//     unchanged since prevEtag — callers use this to skip the diff +
//     audit pass on every refresh tick.
//   - Be safe for concurrent Fetch calls.
//
// values is name → plaintext. The map MUST NOT include any value the
// caller didn't request — there's no opaque per-row metadata; what
// you return is what the agent dispatch sees.
type Source interface {
	Fetch(ctx context.Context, prevEtag string) (values map[string]string, etag string, changed bool, err error)

	// String returns a short, redacted description for log lines:
	// scheme + target, never credentials, never values.
	String() string
}
