package cronicle

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// taskCancelHarness builds a listener wired to a state store and a
// schedule with a DAG that has real downstream dependencies, so the
// cascade computation has something to chew on.
//
// DAG: a → b → c → e
//         ↘ d ↗
func taskCancelHarness(t *testing.T) (*listenServer, *state.Store) {
	t.Helper()
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	conf := &Config{
		Schedules: []Schedule{{
			Name: "chain",
			Cron: "@every 1m",
			Tasks: []Task{
				{Name: "a"},
				{Name: "b", Depends: []string{"a"}},
				{Name: "d", Depends: []string{"a"}},
				{Name: "c", Depends: []string{"b", "d"}},
				{Name: "e", Depends: []string{"c"}},
			},
		}},
	}
	srv := &listenServer{
		token:    "secret",
		confSrc:  func() *Config { return conf },
		stateSrc: func() state.Backend { return store },
	}
	return srv, store
}

func TestListenerTaskCancel_CascadesThroughDAG(t *testing.T) {
	srv, store := taskCancelHarness(t)

	// Seed an in-flight run with task "a" running.
	feedEvent(t, store, `{"time":"2026-05-12T12:00:00Z","entry_type":"schedule_start","run_id":"R_T","schedule":"chain","tasks":["a","b","c","d","e"]}`)
	feedEvent(t, store, `{"time":"2026-05-12T12:00:01Z","entry_type":"task_start","run_id":"R_T","schedule":"chain","task":"a","attempt":1}`)

	rr := httptest.NewRecorder()
	srv.handleRunRoute(rr, authedPost("/v1/runs/R_T/tasks/a/cancel", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("task cancel: got %d body=%s", rr.Code, rr.Body.String())
	}
	var res state.CancelTaskResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Expect a + b + c + d + e all canceled in the projection.
	want := map[string]bool{"a": true, "b": true, "c": true, "d": true, "e": true}
	for _, name := range res.CanceledTasks {
		delete(want, name)
	}
	if len(want) != 0 {
		t.Fatalf("expected all DAG tasks canceled, missing %+v (response=%+v)", want, res)
	}
	// Verify projection state.
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		canceled, _ := store.IsTaskCanceledInRun("R_T", name)
		if !canceled {
			t.Fatalf("%s not canceled in projection", name)
		}
	}
}

func TestListenerTaskCancel_TerminalReturns409(t *testing.T) {
	srv, store := taskCancelHarness(t)

	// Task "a" runs to completion (succeeded).
	feedEvent(t, store, `{"time":"2026-05-12T12:00:00Z","entry_type":"schedule_start","run_id":"R_T2","schedule":"chain","tasks":["a"]}`)
	feedEvent(t, store, `{"time":"2026-05-12T12:00:01Z","entry_type":"task_start","run_id":"R_T2","schedule":"chain","task":"a","attempt":1}`)
	feedEvent(t, store, `{"time":"2026-05-12T12:00:02Z","entry_type":"shell_run","run_id":"R_T2","schedule":"chain","task":"a","exit":0,"duration_ms":1,"success":true}`)

	rr := httptest.NewRecorder()
	srv.handleRunRoute(rr, authedPost("/v1/runs/R_T2/tasks/a/cancel", ""))
	if rr.Code != http.StatusConflict {
		t.Fatalf("terminal task cancel: got %d, want 409", rr.Code)
	}
}

func TestListenerTaskCancel_UnknownRun(t *testing.T) {
	srv, _ := taskCancelHarness(t)

	rr := httptest.NewRecorder()
	srv.handleRunRoute(rr, authedPost("/v1/runs/MISSING/tasks/a/cancel", ""))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown run: got %d, want 404", rr.Code)
	}
}

func TestListenerTaskCancel_UnknownTask(t *testing.T) {
	srv, store := taskCancelHarness(t)
	feedEvent(t, store, `{"time":"2026-05-12T12:00:00Z","entry_type":"schedule_start","run_id":"R_T3","schedule":"chain","tasks":["a"]}`)
	feedEvent(t, store, `{"time":"2026-05-12T12:00:01Z","entry_type":"task_start","run_id":"R_T3","schedule":"chain","task":"a","attempt":1}`)

	rr := httptest.NewRecorder()
	srv.handleRunRoute(rr, authedPost("/v1/runs/R_T3/tasks/ghost/cancel", ""))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown task: got %d, want 404 body=%s", rr.Code, rr.Body.String())
	}
}
