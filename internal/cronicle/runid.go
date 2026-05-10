package cronicle

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// newRunID returns a unique-per-fire run identifier shaped as
// "<utc-yyyymmddThhmmssZ>-<8 hex>". Sortable lexicographically by time
// (newest IDs sort last alphabetically when scoped to a single day —
// the projection sorts by started_at, not by run_id, so this just makes
// the IDs human-readable in logs).
//
// Random suffix is 8 hex chars (~32 bits); collisions are theoretically
// possible at very high rates within the same second but cronicle's
// realistic peak is ~1 trigger/second, where 32 bits is plenty.
func newRunID() string {
	var b [4]byte
	_, _ = rand.Read(b[:]) // crypto/rand never errors on darwin/linux
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

// monotonicSuffix is reserved for a future sortability guarantee if we
// switch to ULIDs. Kept behind a mutex to avoid clock-skew foot-guns.
var monotonicSuffix struct {
	sync.Mutex
	last time.Time
}
