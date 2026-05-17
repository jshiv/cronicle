package secretstore

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// fakeSource is a configurable secretsource.Source for store tests.
// values, changed, etag, err can be swapped between calls; tests
// observe behavior via fetchCount and lastPrevEtag.
type fakeSource struct {
	mu           sync.Mutex
	values       map[string]string
	etag         string
	changed      bool
	err          error
	fetchCount   int
	lastPrevEtag string
}

func (f *fakeSource) Fetch(_ context.Context, prevEtag string) (map[string]string, string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetchCount++
	f.lastPrevEtag = prevEtag
	return f.values, f.etag, f.changed, f.err
}
func (f *fakeSource) String() string { return "fake://" }

// memBackend opens an in-memory state.Store. Used as the SecretStore
// backend for every test below — mirrors the real wiring, sqlite-on-disk
// optional.
func memBackend(t *testing.T) *state.Store {
	t.Helper()
	be, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })
	return be
}

func TestStore_unconfiguredReturnsErr(t *testing.T) {
	s := New()
	if s.Configured() {
		t.Error("New() store should not be Configured")
	}
	_, _, err := s.Get("ANYTHING")
	if !errors.Is(err, ErrNotConfigured) {
		t.Errorf("want ErrNotConfigured, got %v", err)
	}
}

func TestStore_initialFetchHydratesBackend(t *testing.T) {
	be := memBackend(t)
	src := &fakeSource{
		values:  map[string]string{"FOO": "bar"},
		etag:    "v1",
		changed: true,
	}
	s := New().SetBackend(be)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx, src, 100*time.Millisecond); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Source value should have made it into the backend.
	v, ok, err := be.GetSecret("FOO")
	if err != nil || !ok || v != "bar" {
		t.Errorf("backend not hydrated: ok=%v v=%q err=%v", ok, v, err)
	}
	// And reads through the store should see it.
	v, ok, err = s.Get("FOO")
	if err != nil || !ok || v != "bar" {
		t.Errorf("Get: ok=%v v=%q err=%v", ok, v, err)
	}
}

func TestStore_startWithoutSourceIsOK(t *testing.T) {
	// state:// shape: backend bound, no ingestion source. Get works
	// against whatever's already in the backend.
	be := memBackend(t)
	_ = be.PutSecret("PRELOADED", "x", "test")

	s := New().SetBackend(be).StaleTTL(0)
	if err := s.Start(context.Background(), nil, 0); err != nil {
		t.Fatalf("Start(nil source): %v", err)
	}
	v, ok, err := s.Get("PRELOADED")
	if err != nil || !ok || v != "x" {
		t.Errorf("Get(preloaded): ok=%v v=%q err=%v", ok, v, err)
	}
}

func TestStore_initialFetchFailureBubblesUp(t *testing.T) {
	be := memBackend(t)
	src := &fakeSource{err: errors.New("boom")}
	s := New().SetBackend(be)
	err := s.Start(context.Background(), src, 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("Start should surface initial fetch error, got %v", err)
	}
}

func TestStore_refreshPicksUpRotationsInBackend(t *testing.T) {
	be := memBackend(t)
	src := &fakeSource{
		values:  map[string]string{"FOO": "v1"},
		etag:    "1",
		changed: true,
	}
	s := New().SetBackend(be)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx, src, 10*time.Millisecond); err != nil {
		t.Fatalf("start: %v", err)
	}

	src.mu.Lock()
	src.values = map[string]string{"FOO": "v2"}
	src.etag = "2"
	src.changed = true
	src.mu.Unlock()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		v, ok, _ := be.GetSecret("FOO")
		if ok && v == "v2" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("backend never observed rotated value")
}

func TestStore_etagPassedToSourceOnSubsequentFetches(t *testing.T) {
	be := memBackend(t)
	src := &fakeSource{values: map[string]string{}, etag: "etag-A", changed: true}
	s := New().SetBackend(be)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx, src, 10*time.Millisecond); err != nil {
		t.Fatalf("start: %v", err)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		src.mu.Lock()
		got := src.lastPrevEtag
		src.mu.Unlock()
		if got == "etag-A" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("subsequent fetch never used prevEtag from previous result")
}

func TestStore_staleTTLBlocksGet(t *testing.T) {
	be := memBackend(t)
	_ = be.PutSecret("FOO", "v", "test")

	src := &fakeSource{values: map[string]string{"FOO": "v"}, etag: "1", changed: true}
	s := New().SetBackend(be)
	frozen := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { return frozen }

	if err := s.Start(context.Background(), src, time.Hour); err != nil {
		t.Fatalf("start: %v", err)
	}
	s.StaleTTL(1 * time.Minute)

	frozen = frozen.Add(2 * time.Minute)
	_, _, err := s.Get("FOO")
	if !errors.Is(err, ErrStale) {
		t.Errorf("want ErrStale after TTL, got %v", err)
	}
}

func TestStore_staleTTLDoesNotApplyWithoutSource(t *testing.T) {
	// state:// case: no ingestion source. Staleness shouldn't fire.
	be := memBackend(t)
	_ = be.PutSecret("FOO", "v", "test")

	s := New().SetBackend(be)
	s.now = func() time.Time { return time.Unix(2_000_000_000, 0) }
	s.StaleTTL(1 * time.Minute) // arbitrary; should be ignored
	if err := s.Start(context.Background(), nil, 0); err != nil {
		t.Fatalf("start: %v", err)
	}
	v, ok, err := s.Get("FOO")
	if err != nil || !ok || v != "v" {
		t.Errorf("staleness must not gate when no source is bound; got ok=%v v=%q err=%v", ok, v, err)
	}
}

func TestStore_diffAuditLogsNamesNotValues(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	be := memBackend(t)
	src := &fakeSource{
		values:  map[string]string{"API_KEY": "topsecret-12345"},
		etag:    "1",
		changed: true,
	}
	s := New().SetBackend(be)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx, src, time.Hour); err != nil {
		t.Fatalf("start: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "API_KEY") {
		t.Errorf("audit should include name: %s", out)
	}
	if strings.Contains(out, "topsecret-12345") {
		t.Errorf("audit MUST NOT include value: %s", out)
	}
}
