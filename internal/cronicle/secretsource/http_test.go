package secretsource

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestHTTPSource_firstFetchAndConditional(t *testing.T) {
	version := 1
	val := "v1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		etag := strconv.Itoa(version)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_, _ = w.Write([]byte(`{"values":{"ANTHROPIC_API_KEY":"` + val + `"}}`))
	}))
	defer srv.Close()

	s, err := NewHTTPSource(srv.URL)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	got, etag1, changed, err := s.Fetch(context.Background(), "")
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if !changed {
		t.Errorf("first fetch should return changed=true")
	}
	if etag1 != "1" {
		t.Errorf("etag: want 1, got %q", etag1)
	}
	if got["ANTHROPIC_API_KEY"] != "v1" {
		t.Errorf("value mismatch: %v", got)
	}

	// Same etag: 304, changed=false.
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

	// Bump server, expect change.
	version = 2
	val = "v2"
	got3, etag3, changed, err := s.Fetch(context.Background(), etag1)
	if err != nil {
		t.Fatalf("third fetch: %v", err)
	}
	if !changed || etag3 != "2" || got3["ANTHROPIC_API_KEY"] != "v2" {
		t.Errorf("post-bump fetch: changed=%v etag=%q values=%v", changed, etag3, got3)
	}
}

func TestHTTPSource_404IsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	s, _ := NewHTTPSource(srv.URL)
	_, _, _, err := s.Fetch(context.Background(), "")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestHTTPSource_401IsUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	s, _ := NewHTTPSource(srv.URL)
	_, _, _, err := s.Fetch(context.Background(), "")
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("want ErrUnauthorized, got %v", err)
	}
}

func TestHTTPSource_bearerHeaderSent(t *testing.T) {
	gotAuth := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"values":{}}`))
	}))
	defer srv.Close()
	s, _ := NewHTTPSource(srv.URL)
	s.WithBearer("topsecret")
	if _, _, _, err := s.Fetch(context.Background(), ""); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if gotAuth != "Bearer topsecret" {
		t.Errorf("auth header: want Bearer topsecret, got %q", gotAuth)
	}
}

func TestHTTPSource_stringRedactsCredsAndQuery(t *testing.T) {
	s, err := NewHTTPSource("https://alice:hunter2@api.example.com/v1/secrets?actor=worker&trace=abc")
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	got := s.String()
	for _, bad := range []string{"alice", "hunter2", "actor=", "trace="} {
		if strings.Contains(got, bad) {
			t.Errorf("String() leaked %q: %q", bad, got)
		}
	}
}

func TestHTTPSource_emptyValuesIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", "v1")
		_, _ = w.Write([]byte(`{"values":null}`))
	}))
	defer srv.Close()
	s, _ := NewHTTPSource(srv.URL)
	got, _, changed, err := s.Fetch(context.Background(), "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !changed || got == nil || len(got) != 0 {
		t.Errorf("want empty-map, changed=true; got changed=%v values=%v", changed, got)
	}
}

func TestHTTPSource_responseBodyTooLarge(t *testing.T) {
	// A misbehaving server can't drown us in memory: enforce a 1 MiB
	// ceiling. We don't error on overflow — we just truncate, which
	// causes the JSON decode to fail and yield a clear error.
	big := strings.Repeat("x", 2<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"values":{"X":"` + big + `"}}`))
	}))
	defer srv.Close()
	s, _ := NewHTTPSource(srv.URL)
	_, _, _, err := s.Fetch(context.Background(), "")
	if err == nil {
		t.Errorf("oversized body should fail with a decode error")
	}
}
