package cronicle

import (
	"context"
	"testing"
)

// fakeSource always returns the supplied body, claiming changed=true
// regardless of whether the etag matches. Simulates a config source
// that rewrites the etag on every response (some HTTP servers do this
// on 200 even when the content is unchanged). Without the M3
// raw-bytes diff, this would force a reload on every refresh tick
// even when the content is stable.
type fakeSource struct {
	body    []byte
	etag    string
	fetches int
}

func (f *fakeSource) Fetch(ctx context.Context, prevEtag string) ([]byte, string, bool, error) {
	f.fetches++
	return f.body, f.etag, true, nil
}
func (f *fakeSource) String() string { return "fake://" }

// TestFetchAndParse_RawBytesDiffShortCircuits is the M3 invariant.
// Two consecutive fetches return identical body bytes but different
// etags. The previous Hcl()-round-trip diff would lose comments and
// the repo block; the new raw-bytes diff catches them. The second
// fetch must report bytesChanged=false.
func TestFetchAndParse_RawBytesDiffShortCircuits(t *testing.T) {
	body := []byte(`
schedule "s" {
  cron = "@every 1h"
  task "t" {
    command = ["true"]
  }
}
`)
	src := &fakeSource{body: body, etag: "etag-v1"}
	state := newReloadState(src)

	// First fetch — initial, seeds lastRawBody.
	conf1, changed1, err := state.fetchAndParse(context.Background(), true)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if conf1 == nil {
		t.Fatalf("first fetch returned nil config")
	}
	if !changed1 {
		t.Errorf("first fetch should report changed=true (no prior cache)")
	}

	// Server rewrites the etag but keeps the bytes identical.
	src.etag = "etag-v2"
	conf2, changed2, err := state.fetchAndParse(context.Background(), false)
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if changed2 {
		t.Errorf("second fetch with identical bytes should report changed=false; got true")
	}
	// The conf pointer should be the same (cached).
	if conf1 != conf2 {
		t.Errorf("expected cached conf to be returned on no-change; got fresh pointer")
	}
}

// TestFetchAndParse_DetectsRealByteChange: when the body actually
// differs, bytesChanged must be true and the parsed conf must reflect
// the new content.
func TestFetchAndParse_DetectsRealByteChange(t *testing.T) {
	v1 := []byte(`
schedule "s1" {
  cron = "@every 1h"
  task "t" {
    command = ["true"]
  }
}
`)
	v2 := []byte(`
schedule "s2" {
  cron = "@every 1h"
  task "t" {
    command = ["true"]
  }
}
`)
	src := &fakeSource{body: v1, etag: "v1"}
	state := newReloadState(src)
	if _, _, err := state.fetchAndParse(context.Background(), true); err != nil {
		t.Fatalf("first fetch: %v", err)
	}

	src.body = v2
	src.etag = "v2"
	conf, changed, err := state.fetchAndParse(context.Background(), false)
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if !changed {
		t.Errorf("real byte change must set changed=true")
	}
	if conf == nil || len(conf.Schedules) == 0 || conf.Schedules[0].Name != "s2" {
		t.Errorf("parsed conf should reflect the new schedule name; got %+v", conf)
	}
}
