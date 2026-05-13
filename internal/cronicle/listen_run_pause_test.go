package cronicle

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// runPauseHarness seeds a real run row so pause/unpause has a target.
func runPauseHarness(t *testing.T) (*listenServer, *state.Store) {
	t.Helper()
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	feedEvent(t, store, `{"time":"2026-05-12T12:00:00Z","entry_type":"schedule_start","run_id":"R_P","schedule":"daily","tasks":["a","b"]}`)

	conf := &Config{
		Schedules: []Schedule{{
			Name: "daily",
			Cron: "@every 1m",
			Tasks: []Task{{Name: "a"}, {Name: "b", Depends: []string{"a"}}},
		}},
	}
	srv := &listenServer{
		token:    "secret",
		confSrc:  func() *Config { return conf },
		stateSrc: func() state.Backend { return store },
	}
	return srv, store
}

func TestRunPause_BasicLifecycle(t *testing.T) {
	srv, store := runPauseHarness(t)

	rr := httptest.NewRecorder()
	srv.handleRunRoute(rr, authedPost("/v1/runs/R_P/pause", `{"actor":"alice","reason":"check it"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("pause: got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp runStateResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if !resp.Paused || resp.PausedBy != "alice" || resp.Reason != "check it" {
		t.Fatalf("response shape wrong: %+v", resp)
	}
	if paused, _ := store.IsRunPaused("R_P"); !paused {
		t.Fatalf("store didn't reflect pause")
	}

	rr = httptest.NewRecorder()
	srv.handleRunRoute(rr, authedPost("/v1/runs/R_P/resume", `{"actor":"alice"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("unpause: %d", rr.Code)
	}
	if paused, _ := store.IsRunPaused("R_P"); paused {
		t.Fatalf("store still paused after unpause")
	}
}

func TestRunPause_UnknownRun(t *testing.T) {
	srv, _ := runPauseHarness(t)

	rr := httptest.NewRecorder()
	srv.handleRunRoute(rr, authedPost("/v1/runs/MISSING/pause", ""))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown run pause: got %d, want 404", rr.Code)
	}
}

func TestRunPause_Unauthorized(t *testing.T) {
	srv, _ := runPauseHarness(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/R_P/pause", nil)
	srv.handleRunRoute(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauth pause: got %d, want 401", rr.Code)
	}
}
