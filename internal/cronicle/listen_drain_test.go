package cronicle

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

func drainHarness(t *testing.T) (*listenServer, *state.Store, chan []byte) {
	t.Helper()
	srv, store, q := pauseHarness(t)
	return srv, store, q
}

func TestRunnerDrain_BasicLifecycle(t *testing.T) {
	srv, store, _ := drainHarness(t)

	// State endpoint reports clean store.
	rr := httptest.NewRecorder()
	srv.handleRunnerState(rr, authedGet("/v1/runner/state"))
	if rr.Code != http.StatusOK {
		t.Fatalf("state: got %d", rr.Code)
	}
	var resp runnerStateResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Drained {
		t.Fatalf("expected drained=false on fresh runner")
	}

	// Drain.
	rr = httptest.NewRecorder()
	srv.handleRunnerDrain(rr, authedPost("/v1/runner/drain", `{"actor":"alice","reason":"deploy"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("drain: got %d body=%s", rr.Code, rr.Body.String())
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if !resp.Drained || resp.DrainedBy != "alice" || resp.Reason != "deploy" {
		t.Fatalf("response shape wrong: %+v", resp)
	}
	if drained, _ := store.IsDrained(); !drained {
		t.Fatalf("store didn't reflect drain")
	}

	// Undrain.
	rr = httptest.NewRecorder()
	srv.handleRunnerUndrain(rr, authedPost("/v1/runner/undrain", `{"actor":"alice"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("undrain: %d", rr.Code)
	}
	if drained, _ := store.IsDrained(); drained {
		t.Fatalf("store still drained after undrain")
	}
}

func TestRunnerDrain_BlocksTriggers(t *testing.T) {
	srv, store, queue := drainHarness(t)
	if err := store.SetDrained("test", "blocking"); err != nil {
		t.Fatalf("SetDrained: %v", err)
	}

	// Schedule trigger should 503.
	rr := httptest.NewRecorder()
	srv.handleScheduleRoute(rr, authedPost("/v1/schedules/daily/trigger", ""))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("drained schedule trigger: got %d, want 503 body=%s", rr.Code, rr.Body.String())
	}

	// Single-task trigger should 503.
	rr = httptest.NewRecorder()
	srv.handleScheduleRoute(rr, authedPost("/v1/schedules/daily/tasks/t1/trigger", ""))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("drained task trigger: got %d, want 503", rr.Code)
	}

	// Nothing made it to the queue.
	select {
	case payload := <-queue:
		t.Fatalf("drained runner enqueued: %s", payload)
	default:
	}

	// Undrain releases triggers.
	if err := store.ClearDrained("test"); err != nil {
		t.Fatalf("ClearDrained: %v", err)
	}
	rr = httptest.NewRecorder()
	srv.handleScheduleRoute(rr, authedPost("/v1/schedules/daily/trigger", ""))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("post-undrain trigger: got %d body=%s", rr.Code, rr.Body.String())
	}
	select {
	case <-queue:
	default:
		t.Fatalf("post-undrain trigger didn't enqueue")
	}
}

func TestRunnerDrain_Unauthorized(t *testing.T) {
	srv, _, _ := drainHarness(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runner/drain", nil)
	srv.handleRunnerDrain(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauth drain: got %d, want 401", rr.Code)
	}
}
