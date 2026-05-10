package cronicle

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// runsHarness wires a listenServer to a real in-memory state.Store
// so the runs API tests exercise both layers together. The harness
// pre-seeds runs/tasks via the same JSONL Apply path the slog Sink
// uses in production, so tests reflect actual flow.
func runsHarness(t *testing.T) (*listenServer, *state.Store) {
	t.Helper()
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &listenServer{
		token:    "secret",
		stateSrc: func() *state.Store { return store },
	}, store
}

func seedRun(t *testing.T, s *state.Store, runID, schedule, status string, started time.Time) {
	t.Helper()
	apply := func(line string) {
		ev, ok := state.DecodeEvent([]byte(line))
		if !ok {
			t.Fatalf("decode: %s", line)
		}
		if err := s.Apply(ev); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}
	startTS := started.UTC().Format(time.RFC3339Nano)
	endTS := started.Add(time.Second).UTC().Format(time.RFC3339Nano)
	apply(`{"time":"` + startTS + `","entry_type":"schedule_start","run_id":"` + runID + `","schedule":"` + schedule + `","tasks":["t1"]}`)
	apply(`{"time":"` + startTS + `","entry_type":"task_start","run_id":"` + runID + `","schedule":"` + schedule + `","task":"t1","attempt":1}`)
	if status == "succeeded" {
		apply(`{"time":"` + endTS + `","entry_type":"shell_run","run_id":"` + runID + `","schedule":"` + schedule + `","task":"t1","exit":0,"duration_ms":100,"success":true}`)
		apply(`{"time":"` + endTS + `","entry_type":"schedule_complete","run_id":"` + runID + `","schedule":"` + schedule + `","task_count":1,"duration_ms":150,"success":true}`)
	} else if status == "failed" {
		apply(`{"time":"` + endTS + `","entry_type":"shell_run","run_id":"` + runID + `","schedule":"` + schedule + `","task":"t1","exit":1,"duration_ms":50,"success":false,"error":"nope"}`)
		apply(`{"time":"` + endTS + `","entry_type":"schedule_complete","run_id":"` + runID + `","schedule":"` + schedule + `","task_count":1,"duration_ms":80,"success":false,"error":"nope"}`)
	}
	// "running" leaves it without a complete event.
}

// TestRuns_ListAuthRequired: bearer token check applies to /v1/runs.
func TestRuns_ListAuthRequired(t *testing.T) {
	srv, _ := runsHarness(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
	srv.handleListRuns(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rr.Code)
	}
}

// TestRuns_ListEmpty: a valid request against an empty store returns []
// (not null) so the JSON shape is stable for clients.
func TestRuns_ListEmpty(t *testing.T) {
	srv, _ := runsHarness(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer secret")
	srv.handleListRuns(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if body != "[]\n" {
		t.Fatalf("expected [], got %q", body)
	}
}

// TestRuns_ListAndFilter: seed three runs, exercise status, schedule,
// since, and limit filters.
func TestRuns_ListAndFilter(t *testing.T) {
	srv, store := runsHarness(t)
	t0 := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	seedRun(t, store, "A", "daily", "succeeded", t0)
	seedRun(t, store, "B", "daily", "failed", t0.Add(time.Minute))
	seedRun(t, store, "C", "weekly", "succeeded", t0.Add(2*time.Minute))

	get := func(query string) []state.Run {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/runs?"+query, nil)
		req.Header.Set("Authorization", "Bearer secret")
		srv.handleListRuns(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("query=%q: %d body=%s", query, rr.Code, rr.Body.String())
		}
		var got []state.Run
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return got
	}

	all := get("")
	if len(all) != 3 {
		t.Fatalf("all: got %d, want 3", len(all))
	}
	if all[0].RunID != "C" {
		t.Fatalf("expected newest first (C), got %s", all[0].RunID)
	}
	if got := get("status=failed"); len(got) != 1 || got[0].RunID != "B" {
		t.Fatalf("status=failed: %+v", got)
	}
	if got := get("schedule=weekly"); len(got) != 1 || got[0].RunID != "C" {
		t.Fatalf("schedule=weekly: %+v", got)
	}
	if got := get("limit=1"); len(got) != 1 || got[0].RunID != "C" {
		t.Fatalf("limit=1: %+v", got)
	}
	since := t0.Add(90 * time.Second).Format(time.RFC3339)
	if got := get("since=" + since); len(got) != 1 || got[0].RunID != "C" {
		t.Fatalf("since: %+v", got)
	}
}

// TestRuns_ListInvalidParams: bad limit/since produce 400, not 500.
func TestRuns_ListInvalidParams(t *testing.T) {
	srv, _ := runsHarness(t)
	bad := func(query string) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/runs?"+query, nil)
		req.Header.Set("Authorization", "Bearer secret")
		srv.handleListRuns(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("query=%q: got %d, want 400", query, rr.Code)
		}
	}
	bad("limit=abc")
	bad("limit=0")
	bad("since=yesterday")
}

// TestRuns_GetRun: 200 with full task detail when present, 404 when not.
func TestRuns_GetRun(t *testing.T) {
	srv, store := runsHarness(t)
	seedRun(t, store, "X1", "daily", "succeeded", time.Now().UTC())

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/X1", nil)
	req.Header.Set("Authorization", "Bearer secret")
	srv.handleGetRun(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var run state.Run
	if err := json.Unmarshal(rr.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if run.RunID != "X1" || run.Status != "succeeded" || len(run.Tasks) != 1 {
		t.Fatalf("get run wrong: %+v", run)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/runs/nope", nil)
	req.Header.Set("Authorization", "Bearer secret")
	srv.handleGetRun(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown id, got %d", rr.Code)
	}
}

// TestRuns_ServiceUnavailable: when state store isn't wired, surface 503
// instead of returning empty results — clients can tell the projection
// is degraded.
func TestRuns_ServiceUnavailable(t *testing.T) {
	srv := &listenServer{
		token:    "secret",
		stateSrc: func() *state.Store { return nil },
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer secret")
	srv.handleListRuns(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", rr.Code)
	}
}
