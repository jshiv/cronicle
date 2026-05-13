package cronicle

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// skipHarness extends pauseHarness with a multi-task schedule so we can
// exercise the per-task skip endpoints. Same auth/queue plumbing.
func skipHarness(t *testing.T) (*listenServer, *state.Store, chan []byte) {
	t.Helper()
	srv, store, q := pauseHarness(t)
	// Replace the single-task schedule with one that has multiple tasks.
	conf := &Config{
		Schedules: []Schedule{{
			Name: "daily",
			Cron: "@every 1m",
			Tasks: []Task{
				{Name: "extract"},
				{Name: "transform"},
				{Name: "load"},
			},
		}},
	}
	srv.confSrc = func() *Config { return conf }
	return srv, store, q
}

func TestSkipTask_BasicLifecycle(t *testing.T) {
	srv, store, _ := skipHarness(t)

	// Skip "extract".
	rr := httptest.NewRecorder()
	srv.handleScheduleRoute(rr, authedPost("/v1/schedules/daily/tasks/extract/skip", `{"actor":"alice","reason":"upstream broken"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("skip: got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp taskStateResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if !resp.Skipped || resp.SkippedBy != "alice" || resp.Reason != "upstream broken" || resp.SkippedAt == "" {
		t.Fatalf("response shape wrong: %+v", resp)
	}
	if skipped, _ := store.IsTaskSkipped("daily", "extract"); !skipped {
		t.Fatalf("store didn't reflect skip")
	}

	// Schedule state should surface skipped_tasks.
	rr = httptest.NewRecorder()
	srv.handleScheduleRoute(rr, authedGet("/v1/schedules/daily/state"))
	var schedResp scheduleStateResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &schedResp)
	if len(schedResp.SkippedTasks) != 1 || schedResp.SkippedTasks[0].Task != "extract" {
		t.Fatalf("expected one skipped task in schedule state: %+v", schedResp)
	}

	// Unskip.
	rr = httptest.NewRecorder()
	srv.handleScheduleRoute(rr, authedPost("/v1/schedules/daily/tasks/extract/unskip", `{"actor":"alice"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("unskip: %d", rr.Code)
	}
	if skipped, _ := store.IsTaskSkipped("daily", "extract"); skipped {
		t.Fatalf("store still skipped after unskip")
	}
}

func TestSkipTask_UnknownTask(t *testing.T) {
	srv, _, _ := skipHarness(t)
	rr := httptest.NewRecorder()
	srv.handleScheduleRoute(rr, authedPost("/v1/schedules/daily/tasks/missing/skip", ""))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown task: got %d, want 404", rr.Code)
	}
}

func TestSkipTask_TriggerStillAllowed(t *testing.T) {
	// A skipped task doesn't block triggering the schedule — the run
	// will execute the skip at DAG-walk time. Trigger is just queue
	// admission; pause is the verb that gates admission.
	srv, _, queue := skipHarness(t)

	rr := httptest.NewRecorder()
	srv.handleScheduleRoute(rr, authedPost("/v1/schedules/daily/tasks/extract/skip", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("skip: %d", rr.Code)
	}

	rr = httptest.NewRecorder()
	srv.handleScheduleRoute(rr, authedPost("/v1/schedules/daily/trigger", ""))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("trigger after task skip should be 202, got %d body=%s", rr.Code, rr.Body.String())
	}
	select {
	case <-queue:
	default:
		t.Fatalf("trigger didn't enqueue")
	}
}
