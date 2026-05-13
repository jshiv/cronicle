package cronicle

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// triggerFromHarness builds a listener with a multi-task DAG so the
// subgraph trigger has something to narrow.
//
// DAG: A → B → C → E
//        ↘ D ↗
func triggerFromHarness(t *testing.T) (*listenServer, chan []byte) {
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
				{Name: "A"},
				{Name: "B", Depends: []string{"A"}},
				{Name: "D", Depends: []string{"A"}},
				{Name: "C", Depends: []string{"B", "D"}},
				{Name: "E", Depends: []string{"C"}},
			},
		}},
	}
	q := make(chan []byte, 4)
	srv := &listenServer{
		token:    "secret",
		queue:    q,
		confSrc:  func() *Config { return conf },
		stateSrc: func() state.Backend { return store },
	}
	return srv, q
}

func TestTriggerFrom_SubgraphFromMidDAG(t *testing.T) {
	srv, queue := triggerFromHarness(t)

	rr := httptest.NewRecorder()
	srv.handleScheduleRoute(rr, authedPost("/v1/schedules/chain/tasks/B/trigger-from", ""))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("trigger-from: got %d body=%s", rr.Code, rr.Body.String())
	}

	var payload []byte
	select {
	case payload = <-queue:
	default:
		t.Fatalf("trigger-from did not enqueue")
	}

	// Decode and verify subgraph: B + C + E (D is upstream-sibling of B, dropped).
	var sch Schedule
	if err := json.Unmarshal(payload, &sch); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	names := map[string]Task{}
	for _, tk := range sch.Tasks {
		names[tk.Name] = tk
	}
	if len(names) != 3 || names["A"].Name == "A" || names["D"].Name == "D" {
		t.Fatalf("expected only B,C,E in subgraph payload; got %d tasks: %v", len(sch.Tasks), keysOf(names))
	}
	if _, ok := names["B"]; !ok {
		t.Fatalf("B should be present")
	}
	if _, ok := names["C"]; !ok {
		t.Fatalf("C should be present")
	}
	if _, ok := names["E"]; !ok {
		t.Fatalf("E should be present")
	}
	// B's depends on A should be stripped.
	if names["B"].Depends != nil {
		t.Fatalf("B.Depends should be nil after strip, got %v", names["B"].Depends)
	}
	// C's depends originally [B, D]; D was dropped, so C.Depends should be [B].
	if len(names["C"].Depends) != 1 || names["C"].Depends[0] != "B" {
		t.Fatalf("C.Depends should be [B], got %v", names["C"].Depends)
	}
}

func TestTriggerFrom_LeafTaskHeadOnly(t *testing.T) {
	srv, queue := triggerFromHarness(t)

	rr := httptest.NewRecorder()
	srv.handleScheduleRoute(rr, authedPost("/v1/schedules/chain/tasks/E/trigger-from", ""))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("trigger-from leaf: got %d", rr.Code)
	}
	payload := <-queue
	var sch Schedule
	_ = json.Unmarshal(payload, &sch)
	if len(sch.Tasks) != 1 || sch.Tasks[0].Name != "E" {
		t.Fatalf("leaf trigger-from should yield single task E, got %v", sch.Tasks)
	}
	if sch.Tasks[0].Depends != nil {
		t.Fatalf("leaf E.Depends should be nil, got %v", sch.Tasks[0].Depends)
	}
}

func TestTriggerFrom_RootTaskFiresWholeDAG(t *testing.T) {
	srv, queue := triggerFromHarness(t)

	rr := httptest.NewRecorder()
	srv.handleScheduleRoute(rr, authedPost("/v1/schedules/chain/tasks/A/trigger-from", ""))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("trigger-from root: got %d", rr.Code)
	}
	payload := <-queue
	var sch Schedule
	_ = json.Unmarshal(payload, &sch)
	if len(sch.Tasks) != 5 {
		t.Fatalf("trigger-from root should include all 5 tasks, got %d", len(sch.Tasks))
	}
}

func TestTriggerFrom_RejectsUnknownTask(t *testing.T) {
	srv, _ := triggerFromHarness(t)

	rr := httptest.NewRecorder()
	srv.handleScheduleRoute(rr, authedPost("/v1/schedules/chain/tasks/ghost/trigger-from", ""))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown task: got %d, want 404", rr.Code)
	}
}

func TestTriggerFrom_BlockedByPause(t *testing.T) {
	srv, queue := triggerFromHarness(t)
	store := srv.stateSrc()
	if err := store.SetSchedulePaused("chain", "test", "test"); err != nil {
		t.Fatalf("SetSchedulePaused: %v", err)
	}
	rr := httptest.NewRecorder()
	srv.handleScheduleRoute(rr, authedPost("/v1/schedules/chain/tasks/B/trigger-from", ""))
	if rr.Code != http.StatusConflict {
		t.Fatalf("paused trigger-from: got %d, want 409", rr.Code)
	}
	select {
	case got := <-queue:
		t.Fatalf("paused subgraph trigger somehow queued: %s", got)
	default:
	}
}

func keysOf(m map[string]Task) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
