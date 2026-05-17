package secretsource

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpen_filePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.json")
	if err := os.WriteFile(path, []byte(`{"values":{}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Bare path → FileSource.
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("bare path: %v", err)
	}
	if !strings.HasPrefix(s.String(), "file://") {
		t.Errorf("bare path should dispatch to file source: %q", s.String())
	}

	// file:// scheme → FileSource.
	s, err = Open(context.Background(), "file://"+path)
	if err != nil {
		t.Fatalf("file:// scheme: %v", err)
	}
	if !strings.HasPrefix(s.String(), "file://") {
		t.Errorf("file:// scheme should dispatch to file source: %q", s.String())
	}
}

func TestOpen_httpURL(t *testing.T) {
	s, err := Open(context.Background(), "http://example.com/v1/secrets")
	if err != nil {
		t.Fatalf("http url: %v", err)
	}
	if !strings.HasPrefix(s.String(), "http://") {
		t.Errorf("http url should dispatch to http source: %q", s.String())
	}
}

func TestOpen_emptySpec(t *testing.T) {
	if _, err := Open(context.Background(), ""); err == nil {
		t.Error("empty spec should error")
	}
}

func TestOpen_unsupportedScheme(t *testing.T) {
	if _, err := Open(context.Background(), "vault://my-secret"); err == nil {
		t.Error("unsupported scheme should error")
	}
}

func TestOpen_fileURLRejectsRelative(t *testing.T) {
	// file://host/path is ambiguous; tell the user explicitly.
	if _, err := Open(context.Background(), "file://relative/path"); err == nil {
		t.Error("file://relative/... should error")
	}
}
