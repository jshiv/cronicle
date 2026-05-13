package configsource

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileSource_firstFetchReadsContent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cronicle.hcl")
	if err := os.WriteFile(p, []byte("schedule \"x\" {}\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s, err := NewFileSource(p)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	defer s.Close()

	body, etag, changed, err := s.Fetch(context.Background(), "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !changed {
		t.Errorf("first fetch should return changed=true")
	}
	if etag == "" {
		t.Errorf("etag should be non-empty")
	}
	if string(body) != "schedule \"x\" {}\n" {
		t.Errorf("body mismatch: %q", body)
	}
}

func TestFileSource_secondFetchShortCircuits(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cronicle.hcl")
	_ = os.WriteFile(p, []byte("a"), 0o644)
	s, err := NewFileSource(p)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	defer s.Close()

	_, etag1, _, _ := s.Fetch(context.Background(), "")
	body2, etag2, changed, err := s.Fetch(context.Background(), etag1)
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if changed {
		t.Errorf("unchanged file should return changed=false")
	}
	if etag2 != etag1 {
		t.Errorf("etag drift: %q → %q", etag1, etag2)
	}
	if body2 != nil {
		t.Errorf("body should be nil on short-circuit; got %d bytes", len(body2))
	}
}

func TestFileSource_detectsChange(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cronicle.hcl")
	_ = os.WriteFile(p, []byte("a"), 0o644)
	s, err := NewFileSource(p)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	defer s.Close()

	_, etag1, _, _ := s.Fetch(context.Background(), "")
	// fsnotify may be debounced; sleep a little + force a re-write
	// with new bytes, then fetch and expect changed=true.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(p, []byte("ab"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	// Give fsnotify a moment to propagate the event. On macOS kqueue
	// + Linux inotify this is typically <10ms.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		body, etag2, changed, err := s.Fetch(context.Background(), etag1)
		if err != nil {
			t.Fatalf("fetch after rewrite: %v", err)
		}
		if changed {
			if etag2 == etag1 {
				t.Errorf("etag should change with new content")
			}
			if string(body) != "ab" {
				t.Errorf("body mismatch: %q", body)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("change not detected within deadline")
}

func TestFileSource_missingFile(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileSource(filepath.Join(dir, "no-such.hcl"))
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	defer s.Close()

	_, _, _, err = s.Fetch(context.Background(), "")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound for missing file; got %v", err)
	}
}

func TestFileSource_atomicRenameStillDetected(t *testing.T) {
	// cronicled writes via tmp+rename. The watcher must see this as a
	// change even though the path's inode flips.
	dir := t.TempDir()
	p := filepath.Join(dir, "cronicle.hcl")
	_ = os.WriteFile(p, []byte("v1"), 0o644)
	s, err := NewFileSource(p)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	defer s.Close()

	_, etag1, _, _ := s.Fetch(context.Background(), "")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte("v2-via-rename"), 0o644); err != nil {
		t.Fatalf("tmp write: %v", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		t.Fatalf("rename: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		body, _, changed, err := s.Fetch(context.Background(), etag1)
		if err != nil {
			t.Fatalf("fetch after rename: %v", err)
		}
		if changed {
			if string(body) != "v2-via-rename" {
				t.Errorf("body mismatch: %q", body)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("rename change not detected within deadline")
}
