package cronicle

import (
	"bytes"
	"sync"

	cron "github.com/robfig/cron/v3"
)

// runtimeState bundles the three pieces of per-process scheduler state
// that were previously scattered as package-level vars (confPriorGlobal,
// lastRawHCL, staticEntryIDs):
//
//   - conf:           most-recently-loaded *Config, exposed to the HTTP
//                     listener for /v1/schedules etc. and used by the
//                     source-path's lossy diff check.
//   - lastRawHCL:     raw file bytes from the previous successful load,
//                     used by the file path's byte-for-byte change check.
//                     (The previous Hcl()-round-trip approach was lossy —
//                     gohcl drops the repo block, comments, and formatting,
//                     so two different files could produce identical
//                     round-trip bytes and the reload would never trigger.)
//   - staticEntryIDs: cron entry IDs for the heartbeat + config_refresh
//                     entries, used as a "do not delete" mask when
//                     re-registering dynamic schedules on a config change.
//
// They're guarded by a single RWMutex because they're touched together
// by the file path's LoadCron (which writes conf + lastRawHCL after
// re-registering entries) and by the source path's loadInto. Writes
// happen on cron ticks (seconds-scale); reads happen on HTTP requests
// (rarer). RWMutex contention is irrelevant at this rate; a plain Mutex
// would also be fine, but RWMutex makes the "read snapshot" intent
// explicit in the accessor names.
//
// Why a struct instead of separate package-level vars under a mutex:
// future code in the package can no longer reach in and read/write the
// fields directly — every access must go through an accessor that
// acquires the lock. The old layout was honor-system; tests routinely
// did `confPriorGlobal = conf` and would have compiled fine even after
// adding a mutex around the var, silently re-introducing the race.
type runtimeState struct {
	mu             sync.RWMutex
	conf           *Config
	lastRawHCL     []byte
	staticEntryIDs map[cron.EntryID]bool
}

// globalRuntime is the single per-process instance. cmd/run.go's two
// dispatch paths (file source, config source) and the HTTP listener all
// route through it.
var globalRuntime = &runtimeState{staticEntryIDs: map[cron.EntryID]bool{}}

// snapshotConf returns the most-recently-loaded *Config. The returned
// pointer MUST be treated as read-only by callers — concurrent
// LoadCron / loadInto may replace it at any time, and the underlying
// Config is not deep-cloned.
func (r *runtimeState) snapshotConf() *Config {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.conf
}

// storeConf replaces the active *Config. Used by Run / RunFromSource
// at startup and by loadInto on every refresh.
func (r *runtimeState) storeConf(c *Config) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conf = c
}

// storeConfAndHCL atomically updates both conf and lastRawHCL. Used by
// LoadCron at the end of a successful reload so an interleaving reader
// can't see the new rawHCL with the prior conf (or vice-versa).
func (r *runtimeState) storeConfAndHCL(c *Config, raw []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conf = c
	r.lastRawHCL = raw
}

// hclEquals reports whether the supplied bytes are identical to the
// last successfully-loaded HCL. Used by LoadCron to short-circuit a
// no-op reload when the file is unchanged.
func (r *runtimeState) hclEquals(raw []byte) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return bytes.Equal(r.lastRawHCL, raw)
}

// setStaticEntries records the cron entry IDs that LoadCron / loadInto
// must preserve when removing dynamic schedule entries on a reload.
// Called once at scheduler startup, before c.Start() so that a refresh
// tick firing immediately doesn't observe an empty static set and strip
// its own registration.
func (r *runtimeState) setStaticEntries(ids map[cron.EntryID]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.staticEntryIDs = ids
}

// isStaticEntry reports whether id was registered as a static entry
// (heartbeat / config_refresh). Used by LoadCron / loadInto to skip
// these when iterating c.Entries() for removal.
func (r *runtimeState) isStaticEntry(id cron.EntryID) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.staticEntryIDs[id]
}

// resetRuntimeStateForTest restores the package-level runtime to a
// fresh state. Tests that exercise StartCron / LoadCron mutate the
// shared globalRuntime; without a reset between tests their writes
// would leak into each other.
func resetRuntimeStateForTest() {
	globalRuntime.mu.Lock()
	defer globalRuntime.mu.Unlock()
	globalRuntime.conf = nil
	globalRuntime.lastRawHCL = nil
	globalRuntime.staticEntryIDs = map[cron.EntryID]bool{}
}
