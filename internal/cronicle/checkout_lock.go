package cronicle

import (
	"path/filepath"
	"sync"
)

// A DAG may send multiple tasks (and a config refresh) to the same worktree.
// go-git does not serialize their index writes. Keep clone + fetch + checkout
// together; commands and independent worktrees remain concurrent. This is an
// in-process guard, not a lease for multiple workers sharing a filesystem.
var checkoutLocks = struct {
	sync.Mutex
	entries map[string]*checkoutLock
}{entries: make(map[string]*checkoutLock)}

type checkoutLock struct {
	mu    sync.Mutex
	users int
}

func lockCheckout(dir string) (func(), error) {
	key, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	// Resolve existing ancestors too, so a new clone through a symlink uses
	// the same key as later accesses to the populated worktree.
	key = canonicalCheckoutPath(key)
	checkoutLocks.Lock()
	entry := checkoutLocks.entries[key]
	if entry == nil {
		entry = &checkoutLock{}
		checkoutLocks.entries[key] = entry
	}
	entry.users++
	checkoutLocks.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		checkoutLocks.Lock()
		entry.users--
		if entry.users == 0 {
			delete(checkoutLocks.entries, key)
		}
		checkoutLocks.Unlock()
	}, nil
}

func canonicalCheckoutPath(dir string) string {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	parent := filepath.Dir(dir)
	if parent == dir {
		return dir
	}
	return filepath.Join(canonicalCheckoutPath(parent), filepath.Base(dir))
}
