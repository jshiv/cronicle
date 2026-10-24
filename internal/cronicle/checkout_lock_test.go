package cronicle

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckoutLockAliasesAndIndependentDirectories(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	// The clone directory does not exist yet: canonicalize its parent.
	unlock, err := lockCheckout(filepath.Join(real, "clone"))
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			unlock()
		}
	}()
	same := make(chan struct{})
	go func() {
		release, e := lockCheckout(filepath.Join(alias, "clone"))
		if e == nil {
			close(same)
			release()
		}
	}()
	other := make(chan struct{})
	go func() {
		release, e := lockCheckout(filepath.Join(root, "other"))
		if e == nil {
			close(other)
			release()
		}
	}()
	select {
	case <-other:
	case <-time.After(time.Second):
		t.Fatal("independent checkout blocked")
	}
	select {
	case <-same:
		t.Fatal("alias bypassed checkout lock")
	case <-time.After(30 * time.Millisecond):
	}
	unlock()
	released = true
	select {
	case <-same:
	case <-time.After(time.Second):
		t.Fatal("waiter did not resume")
	}
}
