package configsource

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestHTTPSource_firstFetchAndConditional(t *testing.T) {
	version := 1
	body := "schedule \"x\" {}"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		etag := strconv.Itoa(version)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Write([]byte(body))
	}))
	defer srv.Close()

	s, err := NewHTTPSource(srv.URL)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	gotBody, etag1, changed, err := s.Fetch(context.Background(), "")
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if !changed {
		t.Errorf("first fetch should return changed=true")
	}
	if etag1 != "1" {
		t.Errorf("etag: want 1, got %q", etag1)
	}
	if string(gotBody) != body {
		t.Errorf("body mismatch: %q", gotBody)
	}

	// Second fetch with the same etag: server should 304 and we return changed=false.
	_, etag2, changed, err := s.Fetch(context.Background(), etag1)
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if changed {
		t.Errorf("304 should yield changed=false")
	}
	if etag2 != etag1 {
		t.Errorf("etag should be preserved on 304")
	}

	// Bump server-side, expect change.
	version = 2
	body = "schedule \"y\" {}"
	got3, etag3, changed, err := s.Fetch(context.Background(), etag1)
	if err != nil {
		t.Fatalf("third fetch: %v", err)
	}
	if !changed {
		t.Errorf("post-bump fetch should return changed=true")
	}
	if etag3 != "2" {
		t.Errorf("etag: want 2, got %q", etag3)
	}
	if string(got3) != body {
		t.Errorf("body mismatch: %q", got3)
	}
}

func TestHTTPSource_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	s, _ := NewHTTPSource(srv.URL)
	_, _, _, err := s.Fetch(context.Background(), "")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound; got %v", err)
	}
}

func TestHTTPSource_fallsBackToVersionHeader(t *testing.T) {
	// cronicle-infra emits X-Cronicle-Config-Version, not ETag.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Cronicle-Config-Version", "42")
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	s, _ := NewHTTPSource(srv.URL)
	_, etag, changed, err := s.Fetch(context.Background(), "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !changed {
		t.Errorf("changed should be true on first fetch")
	}
	if etag != "42" {
		t.Errorf("etag: want 42, got %q", etag)
	}
}

func TestHTTPSource_withBearer(t *testing.T) {
	seen := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	s, _ := NewHTTPSource(srv.URL)
	s.WithBearer("abc")
	_, _, _, _ = s.Fetch(context.Background(), "")
	if seen != "Bearer abc" {
		t.Errorf("want Authorization=Bearer abc, got %q", seen)
	}
}

func TestHTTPSource_redactsUserinfoInString(t *testing.T) {
	s, err := NewHTTPSource("http://user:pass@example.com/cfg")
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	str := s.String()
	if str != "http://example.com/cfg" {
		t.Errorf("userinfo not redacted: %q", str)
	}
}
