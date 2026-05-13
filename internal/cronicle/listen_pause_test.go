package cronicle

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// pauseHarness builds a listener wired to:
//   - an in-memory state store
//   - a static Config with one schedule "daily" / task "t1"
//   - a bounded send queue so triggers don't deadlock under the 5s
//     send timeout when the queue is plumbed
//
// Returns the listener, the underlying store, and a channel the
// triggers will land in.
func pauseHarness(t *testing.T) (*listenServer, *state.Store, chan []byte) {
	t.Helper()
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	conf := &Config{
		Schedules: []Schedule{{
			Name: "daily",
			Cron: "@every 1m",
			Tasks: []Task{{Name: "t1"}},
		}},
	}
	q := make(chan []byte, 4)
	srv := &listenServer{
		token:    "secret",
		queue:    q,
		confSrc:  func() *Config { return conf },
		stateSrc: func() state.Backend { return store },
	}
	return srv, store, q
}

func authedPost(path string, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(http.MethodPost, path, nil)
	} else {
		r = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer secret")
	return r
}

func authedGet(path string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Authorization", "Bearer secret")
	return r
}

func TestPauseSchedule_BasicLifecycle(t *testing.T) {
	srv, store, _ := pauseHarness(t)

	// State of an unknown schedule is { paused: false }.
	rr := httptest.NewRecorder()
	srv.handleScheduleRoute(rr, authedGet("/v1/schedules/daily/state"))
	if rr.Code != http.StatusOK {
		t.Fatalf("state pre-pause: got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp scheduleStateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Paused {
		t.Fatalf("expected paused=false, got %+v", resp)
	}

	// Pause.
	rr = httptest.NewRecorder()
	srv.handleScheduleRoute(rr, authedPost("/v1/schedules/daily/pause", `{"actor":"alice","reason":"migration"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("pause: got %d body=%s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Paused || resp.PausedBy != "alice" || resp.Reason != "migration" || resp.PausedAt == "" {
		t.Fatalf("pause response shape wrong: %+v", resp)
	}

	// Verify the store actually has it paused.
	if paused, err := store.IsSchedulePaused("daily"); err != nil || !paused {
		t.Fatalf("store didn't reflect pause: paused=%v err=%v", paused, err)
	}

	// State endpoint reflects the pause.
	rr = httptest.NewRecorder()
	srv.handleScheduleRoute(rr, authedGet("/v1/schedules/daily/state"))
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if !resp.Paused {
		t.Fatalf("state post-pause should be paused=true: %+v", resp)
	}

	// Resume.
	rr = httptest.NewRecorder()
	srv.handleScheduleRoute(rr, authedPost("/v1/schedules/daily/resume", `{"actor":"alice"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("resume: got %d body=%s", rr.Code, rr.Body.String())
	}
	if paused, _ := store.IsSchedulePaused("daily"); paused {
		t.Fatalf("store still paused after resume")
	}
}

func TestPauseSchedule_BlocksTrigger(t *testing.T) {
	srv, _, queue := pauseHarness(t)

	// Pause first.
	rr := httptest.NewRecorder()
	srv.handleScheduleRoute(rr, authedPost("/v1/schedules/daily/pause", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("pause: %d", rr.Code)
	}

	// Trigger schedule should now 409.
	rr = httptest.NewRecorder()
	srv.handleScheduleRoute(rr, authedPost("/v1/schedules/daily/trigger", ""))
	if rr.Code != http.StatusConflict {
		t.Fatalf("paused trigger: got %d, want 409 — body=%s", rr.Code, rr.Body.String())
	}
	select {
	case got := <-queue:
		t.Fatalf("paused schedule somehow queued: %s", got)
	default:
	}

	// Trigger task should also 409.
	rr = httptest.NewRecorder()
	srv.handleScheduleRoute(rr, authedPost("/v1/schedules/daily/tasks/t1/trigger", ""))
	if rr.Code != http.StatusConflict {
		t.Fatalf("paused task trigger: got %d, want 409", rr.Code)
	}

	// Resume; trigger should land in queue.
	rr = httptest.NewRecorder()
	srv.handleScheduleRoute(rr, authedPost("/v1/schedules/daily/resume", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("resume: %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	srv.handleScheduleRoute(rr, authedPost("/v1/schedules/daily/trigger", ""))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("post-resume trigger: got %d, want 202 body=%s", rr.Code, rr.Body.String())
	}
	select {
	case <-queue:
		// good
	default:
		t.Fatalf("post-resume trigger did not enqueue")
	}
}

func TestPauseSchedule_UnauthAndUnknown(t *testing.T) {
	srv, _, _ := pauseHarness(t)

	// Unauthenticated pause is rejected.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/schedules/daily/pause", nil)
	srv.handleScheduleRoute(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauth pause: got %d, want 401", rr.Code)
	}

	// Unknown schedule.
	rr = httptest.NewRecorder()
	srv.handleScheduleRoute(rr, authedPost("/v1/schedules/missing/pause", ""))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown schedule pause: got %d, want 404", rr.Code)
	}
}

func TestPauseSchedule_IdempotentPause(t *testing.T) {
	srv, store, _ := pauseHarness(t)

	// First pause.
	rr := httptest.NewRecorder()
	srv.handleScheduleRoute(rr, authedPost("/v1/schedules/daily/pause", `{"actor":"first","reason":"one"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("first pause: %d", rr.Code)
	}
	first, _ := store.GetScheduleState("daily")
	if first.PausedAt.IsZero() {
		t.Fatalf("paused_at zero after first pause")
	}

	// Second pause — paused_at must not advance.
	rr = httptest.NewRecorder()
	srv.handleScheduleRoute(rr, authedPost("/v1/schedules/daily/pause", `{"actor":"second","reason":"two"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("second pause: %d", rr.Code)
	}
	second, _ := store.GetScheduleState("daily")
	if !second.PausedAt.Equal(first.PausedAt) {
		t.Fatalf("paused_at moved across idempotent pauses: %v vs %v", first.PausedAt, second.PausedAt)
	}
	if second.PausedBy != "second" || second.Reason != "two" {
		t.Fatalf("expected latest actor/reason to win: %+v", second)
	}
}

func TestPauseSchedule_HeaderFallbacks(t *testing.T) {
	srv, _, _ := pauseHarness(t)

	// No body — actor/reason come from headers.
	req := authedPost("/v1/schedules/daily/pause", "")
	req.Header.Set("X-Actor", "bob")
	req.Header.Set("X-Reason", "header-fallback")

	rr := httptest.NewRecorder()
	srv.handleScheduleRoute(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("pause via headers: %d", rr.Code)
	}
	var resp scheduleStateResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.PausedBy != "bob" || resp.Reason != "header-fallback" {
		t.Fatalf("header fallback didn't take: %+v", resp)
	}
}
