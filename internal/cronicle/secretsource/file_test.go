package secretsource

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileSource_roundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.json")
	if err := os.WriteFile(path, []byte(`{"values":{"FOO":"bar"}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	s, err := NewFileSource(path)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	got, etag1, changed, err := s.Fetch(context.Background(), "")
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if !changed || got["FOO"] != "bar" {
		t.Errorf("first fetch: changed=%v values=%v", changed, got)
	}

	// Same contents, same etag, no change.
	_, etag2, changed, err := s.Fetch(context.Background(), etag1)
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if changed || etag2 != etag1 {
		t.Errorf("unchanged contents should yield changed=false; got changed=%v", changed)
	}

	// Edit contents → new etag, new map.
	if err := os.WriteFile(path, []byte(`{"values":{"FOO":"baz","NEW":"x"}}`), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	got3, etag3, changed, err := s.Fetch(context.Background(), etag1)
	if err != nil {
		t.Fatalf("third fetch: %v", err)
	}
	if !changed || etag3 == etag1 || got3["FOO"] != "baz" || got3["NEW"] != "x" {
		t.Errorf("post-edit fetch: changed=%v etag=%q values=%v", changed, etag3, got3)
	}
}

func TestFileSource_missingIsNotFound(t *testing.T) {
	s, err := NewFileSource(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	_, _, _, err = s.Fetch(context.Background(), "")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestFileSource_invalidJSONErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.json")
	if err := os.WriteFile(path, []byte(`{not json}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	s, _ := NewFileSource(path)
	_, _, _, err := s.Fetch(context.Background(), "")
	if err == nil {
		t.Errorf("invalid JSON should error")
	}
}
