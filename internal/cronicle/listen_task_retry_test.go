package cronicle

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// taskRetryHarness seeds a terminal run on a real DAG so the listener
// can compute the cascade against confSrc and the state layer can read
// a real payload from the jobs table.
//
// DAG: A → B → C → E
//        ↘ D ↗
func taskRetryHarness(t *testing.T) (*listenServer, *state.Store) {
	t.Helper()
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Seed the queue + projection.
	payload := `{"Name":"chain","RunID":"R_RT","Tasks":[
		{"Name":"A","Depends":null},
		{"Name":"B","Depends":["A"]},
		{"Name":"D","Depends":["A"]},
		{"Name":"C","Depends":["B","D"]},
		{"Name":"E","Depends":["C"]}
	]}`
	_ = store.Enqueue("R_RT", "chain", []byte(payload))
	_, _ = store.Claim("W", time.Minute)
	feedEvent(t, store, `{"time":"2026-05-12T12:00:00Z","entry_type":"schedule_start","run_id":"R_RT","schedule":"chain","tasks":["A","B","C","D","E"]}`)
	feedEvent(t, store, `{"time":"2026-05-12T12:00:01Z","entry_type":"shell_run","run_id":"R_RT","schedule":"chain","task":"A","exit":0,"duration_ms":5,"success":true}`)
	feedEvent(t, store, `{"time":"2026-05-12T12:00:02Z","entry_type":"shell_run","run_id":"R_RT","schedule":"chain","task":"B","exit":1,"duration_ms":5,"success":false,"error":"boom"}`)
	if _, err := store.Cancel("R_RT"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	conf := &Config{
		Schedules: []Schedule{{
			Name: "chain",
			Cron: "@every 1m",
			Tasks: []Task{
				{Name: "A"},
				{Name: "B", Depends: []string{"A"}},
				{Name: "D", Depends: []string{"A"}},
				{Name: "C", Depends: []string{"B", "D"}},
				{Name: "E", Depends: []string{"C"}},
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

func TestListenerTaskRetry_KeepsTargetAndDescendants(t *testing.T) {
	srv, store := taskRetryHarness(t)

	rr := httptest.NewRecorder()
	srv.handleRunRoute(rr, authedPost("/v1/runs/R_RT/tasks/B/retry", ""))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("retry: got %d body=%s", rr.Code, rr.Body.String())
	}
	var res state.RetryResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.OriginalRunID != "R_RT" || res.NewRunID == "" || res.Schedule != "chain" {
		t.Fatalf("response shape wrong: %+v", res)
	}
	// SkippedTasks should be [A, D] (A is upstream of B; D is
	// downstream of A but not of B, so it's also dropped).
	dropped := map[string]bool{}
	for _, s := range res.SkippedTasks {
		dropped[s] = true
	}
	if !dropped["A"] || !dropped["D"] {
		t.Fatalf("expected A and D dropped, got %v", res.SkippedTasks)
	}
	if dropped["B"] || dropped["C"] || dropped["E"] {
		t.Fatalf("B, C, E should be kept, dropped=%v", res.SkippedTasks)
	}

	_ = store // keep import alive; payload introspection requires
	// state-package internals that aren't exported, so we trust the
	// state-layer tests in retry_task_test.go to verify the actual
	// payload shape. Here the response fields are the contract.
}

func TestListenerTaskRetry_UnknownRun(t *testing.T) {
	srv, _ := taskRetryHarness(t)

	rr := httptest.NewRecorder()
	srv.handleRunRoute(rr, authedPost("/v1/runs/MISSING/tasks/B/retry", ""))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown run: got %d, want 404", rr.Code)
	}
}

func TestListenerTaskRetry_UnknownTask(t *testing.T) {
	srv, _ := taskRetryHarness(t)

	rr := httptest.NewRecorder()
	srv.handleRunRoute(rr, authedPost("/v1/runs/R_RT/tasks/ghost/retry", ""))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown task: got %d, want 400 body=%s", rr.Code, rr.Body.String())
	}
}
