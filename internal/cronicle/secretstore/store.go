// Package secretstore is the bridge between cronicle's pluggable
// secretsource layer and the in-tree state.Backend's SecretStore role.
//
// Architecture (post-redesign): the state.db is the canonical store.
// Everything reads from there at dispatch time. --secret-source
// values are *ingestion paths* that populate state.db on each refresh
// tick; they are not consulted on the hot path. Benefits:
//
//   - One read path for $secret.NAME resolution, regardless of
//     ingestion source.
//   - Survives runtime restarts (values are at-rest in state.db).
//   - Switching the state Backend (sqlite → Postgres via cronicle-infra)
//     carries the secret layer along — same SecretStore role.
//   - `cronicle secret set/list/rm` and the listener's /v1/secrets
//     endpoints are first-class write paths, peers of refresh
//     ingestion rather than parallel subsystems.
//
// Security stance:
//   - Audit on change logs names only, never values. The refresh loop
//     compares incoming map vs state.db's current list and emits added/
//     removed/rotated entries by name.
//   - Stale TTL gates dispensing: if the most recent successful refresh
//     is older than StaleTTL, Get returns ErrStale rather than handing
//     out potentially-revoked values.
//   - Subprocess env scrubbing (in pkg/exec) ensures secrets dispensed
//     to a bash tool's env don't leak via os.Environ inheritance.
package secretstore

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle/secretsource"
	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// ErrStale is returned when the most recent successful ingest is older
// than StaleTTL. Callers must fail the current operation — the explicit
// failure signal is the whole point of staleness gating (revocation
// must not silently regress to old values).
var ErrStale = errors.New("secretstore: cached values are stale (refresh has been failing)")

// ErrNotConfigured is returned when no Backend has been bound. Distinct
// from "name not found" so callers can produce a clearer error than
// silent "$secret.X resolved to empty."
var ErrNotConfigured = errors.New("secretstore: not configured (no state backend bound)")

// Default refresh / staleness. Refresh is frequent enough that
// rotations take effect within seconds; TTL is long enough that brief
// outages don't fail tasks but short enough that an extended outage
// doesn't let revoked secrets keep flowing for hours.
const (
	DefaultRefreshInterval = 5 * time.Second
	DefaultStaleTTL        = 5 * time.Minute
)

// Store is the dispatch-facing read façade. All resolution
// ($secret.NAME → value) flows through Get/ExpandValue/ExpandEnv. The
// Store does NOT cache values in process memory — every Get is a
// Backend.GetSecret call so the canonical source of truth is the
// state.db row, not an in-process snapshot that could drift.
//
// staleTTL is enforced against the most recent successful *ingest*:
// if the refresh loop has been failing long enough, Get refuses to
// dispense. When no ingestion source is configured (`state://` or
// pure CLI-managed mode), staleness gating is bypassed — the state.db
// IS the source.
type Store struct {
	mu             sync.RWMutex
	backend        state.SecretStore
	lastIngest     time.Time
	lastSourceETag string
	source         secretsource.Source // nil when no ingestion configured
	staleTTL       time.Duration

	// now is injectable so staleness tests don't have to sleep.
	now func() time.Time
}

// New constructs an unconfigured Store. Most callers want Start
// (binds Backend + spawns refresh loop); this exists for tests that
// drive the Store directly.
func New() *Store {
	return &Store{staleTTL: DefaultStaleTTL, now: time.Now}
}

// SetBackend installs the state.SecretStore implementation reads
// flow through. Call BEFORE Start when an ingestion source will be
// driving writes; or call alone for the "manage via CLI/HTTP only"
// shape.
func (s *Store) SetBackend(b state.SecretStore) *Store {
	s.mu.Lock()
	s.backend = b
	s.mu.Unlock()
	return s
}

// StaleTTL overrides the threshold. 0 disables gating — appropriate
// for backends with no ingest pump (state.db is the truth).
func (s *Store) StaleTTL(d time.Duration) *Store {
	s.mu.Lock()
	s.staleTTL = d
	s.mu.Unlock()
	return s
}

// Configured reports whether SetBackend has been called.
func (s *Store) Configured() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.backend != nil
}

// Get returns the value for name from the state Backend. Errors:
//   - ErrNotConfigured when SetBackend hasn't been called
//   - ErrStale when an ingestion source is bound and its last
//     successful refresh is older than staleTTL
//   - backend I/O error otherwise
//
// ok=false (no error) when name is simply not in the store.
func (s *Store) Get(name string) (value string, ok bool, err error) {
	s.mu.RLock()
	be := s.backend
	src := s.source
	last := s.lastIngest
	ttl := s.staleTTL
	now := s.now()
	s.mu.RUnlock()

	if be == nil {
		return "", false, ErrNotConfigured
	}
	// Staleness only applies when an ingestion source is wired AND a
	// ttl is set. The CLI/HTTP-only case (no source) skips this gate.
	if src != nil && ttl > 0 && !last.IsZero() && now.Sub(last) > ttl {
		return "", false, ErrStale
	}
	return be.GetSecret(name)
}

// Names returns the sorted secret names from the backend, for audit
// inspection. Errors bubble unchanged.
func (s *Store) Names() ([]string, error) {
	s.mu.RLock()
	be := s.backend
	s.mu.RUnlock()
	if be == nil {
		return nil, ErrNotConfigured
	}
	return be.ListSecretNames()
}

// Start binds an ingestion source and launches the refresh loop. The
// initial sync fetch hydrates the backend before returning so startup
// fails loudly on a misconfigured source.
//
// When src is nil, no refresh loop runs — the Backend is the source
// of truth, populated via CLI / HTTP / direct PutSecret. This is the
// shape the state:// source uses.
func (s *Store) Start(ctx context.Context, src secretsource.Source, interval time.Duration) error {
	s.mu.Lock()
	if s.backend == nil {
		s.mu.Unlock()
		return ErrNotConfigured
	}
	s.source = src
	s.mu.Unlock()

	if src == nil {
		return nil
	}
	if interval <= 0 {
		interval = DefaultRefreshInterval
	}
	if err := s.refreshOnce(ctx); err != nil {
		return err
	}
	go s.refreshLoop(ctx, interval)
	return nil
}

func (s *Store) refreshLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.refreshOnce(ctx); err != nil {
				slog.Warn("secretstore: refresh failed; keeping last good state.db rows",
					"source", s.sourceString(), "err", err.Error())
			}
		}
	}
}

// refreshOnce pulls from the configured source and writes any
// changes into the Backend. Audit logs names of added/removed/
// rotated entries — values are never logged.
func (s *Store) refreshOnce(ctx context.Context) error {
	s.mu.RLock()
	src := s.source
	be := s.backend
	prevSourceEtag := s.lastSourceETag
	s.mu.RUnlock()
	if src == nil || be == nil {
		return nil
	}

	values, etag, changed, err := src.Fetch(ctx, prevSourceEtag)
	if err != nil {
		return err
	}
	now := s.now()
	if !changed {
		s.mu.Lock()
		s.lastIngest = now
		if etag != "" {
			s.lastSourceETag = etag
		}
		s.mu.Unlock()
		return nil
	}

	// Diff against current Backend state for audit.
	current, err := be.ListSecrets()
	if err != nil {
		return err
	}
	added, removed, rotated := diff(current, values)

	// Apply. Errors from individual writes are non-fatal at the
	// store-level: we keep applying the rest so a transient single-
	// row failure doesn't abort an entire refresh.
	for _, name := range append(added, rotated...) {
		if err := be.PutSecret(name, values[name], "secretstore:refresh"); err != nil {
			slog.Warn("secretstore: put failed", "name", name, "err", err.Error())
		}
	}
	for _, name := range removed {
		if _, err := be.DeleteSecret(name, "secretstore:refresh"); err != nil {
			slog.Warn("secretstore: delete failed", "name", name, "err", err.Error())
		}
	}

	s.mu.Lock()
	s.lastIngest = now
	s.lastSourceETag = etag
	s.mu.Unlock()

	if len(added)+len(removed)+len(rotated) > 0 {
		slog.Info("secretstore: refresh applied",
			"source", src.String(),
			"etag", etag,
			"added", added,
			"removed", removed,
			"rotated", rotated,
		)
	}
	return nil
}

// diff compares two name → value maps. Values are inspected to detect
// rotations but never logged.
func diff(current, incoming map[string]string) (added, removed, rotated []string) {
	for k, vNew := range incoming {
		if vOld, ok := current[k]; !ok {
			added = append(added, k)
		} else if vOld != vNew {
			rotated = append(rotated, k)
		}
	}
	for k := range current {
		if _, ok := incoming[k]; !ok {
			removed = append(removed, k)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(rotated)
	return
}

func (s *Store) sourceString() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.source == nil {
		return ""
	}
	return s.source.String()
}

// ---- process-wide singleton -----------------------------------------------

var defaultStore = New()

// Default returns the process-wide Store. Always non-nil; before
// SetBackend is called, Get returns ErrNotConfigured.
func Default() *Store { return defaultStore }

// StartDefault is the convenience the worker startup path calls. Binds
// the backend and starts the refresh loop in one call.
func StartDefault(ctx context.Context, be state.SecretStore, src secretsource.Source, interval, staleTTL time.Duration) error {
	defaultStore.SetBackend(be)
	if staleTTL > 0 {
		defaultStore.StaleTTL(staleTTL)
	}
	return defaultStore.Start(ctx, src, interval)
}
