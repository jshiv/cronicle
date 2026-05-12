package cronicle

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

func controlHarness(t *testing.T) (*listenServer, *state.Store) {
	t.Helper()
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &listenServer{
		token:    "secret",
		stateSrc: func() *state.Store { return store },
	}, store
}

// TestWorkers_ListEmpty: with no workers registered, returns [].
func TestWorkers_ListEmpty(t *testing.T) {
	srv, _ := controlHarness(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/workers", nil)
	req.Header.Set("Authorization", "Bearer secret")
	srv.handleListWorkers(rr, req)
	if rr.Code != http.StatusOK || strings.TrimSpace(rr.Body.String()) != "[]" {
		t.Fatalf("got %d %q, want 200 []", rr.Code, rr.Body.String())
	}
}

// TestWorkers_ListAfterClaim: claiming a job populates the registry,
// list returns the worker with status=active.
func TestWorkers_ListAfterClaim(t *testing.T) {
	srv, store := controlHarness(t)
	_ = store.Enqueue("R1", "x", []byte(`{}`))
	_, _ = store.Claim("W_A", time.Minute)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/workers", nil)
	req.Header.Set("Authorization", "Bearer secret")
	srv.handleListWorkers(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d", rr.Code)
	}
	var ws []state.Worker
	if err := json.Unmarshal(rr.Body.Bytes(), &ws); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(ws) != 1 || ws[0].WorkerID != "W_A" || ws[0].Status != "active" {
		t.Fatalf("workers list wrong: %+v", ws)
	}
}

// TestRunCancel_Pending: cancel via API marks job + projection canceled.
func TestRunCancel_Pending(t *testing.T) {
	srv, store := controlHarness(t)
	_ = store.Enqueue("R1", "x", []byte(`{"RunID":"R1","Schedule":"x","Tasks":[]}`))
	// Seed a runs row via the projection so the API can return state.
	applyEv := func(line string) {
		ev, _ := state.DecodeEvent([]byte(line))
		_ = store.Apply(ev)
	}
	applyEv(`{"time":"2026-05-10T12:00:00Z","entry_type":"schedule_start","run_id":"R1","schedule":"x","tasks":["a"]}`)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/R1/cancel", nil)
	req.Header.Set("Authorization", "Bearer secret")
	srv.handleRunRoute(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if n, _ := store.CountJobsByStatus(state.JobCanceled); n != 1 {
		t.Fatalf("expected 1 canceled job, got %d", n)
	}
	r, _ := store.GetRun("R1")
	if r.Status != "canceled" {
		t.Fatalf("run status: got %s, want canceled", r.Status)
	}
}

// TestRunCancel_Terminal: cancel on completed run returns 409.
func TestRunCancel_Terminal(t *testing.T) {
	srv, store := controlHarness(t)
	_ = store.Enqueue("R1", "x", []byte(`{}`))
	_, _ = store.Claim("W_A", time.Minute)
	_ = store.Ack("R1", "W_A", true, "")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/R1/cancel", nil)
	req.Header.Set("Authorization", "Bearer secret")
	srv.handleRunRoute(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409", rr.Code)
	}
}

// TestRunCancel_PushesSSE: when a worker is subscribed AND holds the
// claim, a cancel POST results in an SSE message landing on the worker's
// channel before the request returns.
func TestRunCancel_PushesSSE(t *testing.T) {
	srv, store := controlHarness(t)
	_ = store.Enqueue("R1", "x", []byte(`{}`))
	_, _ = store.Claim("W_A", time.Minute)
	ch, unsub := store.Subscribe("W_A")
	defer unsub()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/R1/cancel", nil)
	req.Header.Set("Authorization", "Bearer secret")
	srv.handleRunRoute(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d", rr.Code)
	}
	select {
	case msg := <-ch:
		if msg.Type != "cancel" || msg.RunID != "R1" {
			t.Fatalf("got: %+v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("no SSE message delivered")
	}
}

// TestRunRetry_HappyPath: terminal run → retry produces a new run_id
// and 202 response.
func TestRunRetry_HappyPath(t *testing.T) {
	srv, store := controlHarness(t)
	_ = store.Enqueue("R1", "daily", []byte(`{"RunID":"R1","Schedule":"daily"}`))
	_, _ = store.Claim("W_A", time.Minute)
	_ = store.Ack("R1", "W_A", false, "boom")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/R1/retry", nil)
	req.Header.Set("Authorization", "Bearer secret")
	srv.handleRunRoute(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	var res state.RetryResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.OriginalRunID != "R1" || res.NewRunID == "" || res.Schedule != "daily" {
		t.Fatalf("retry result: %+v", res)
	}
	if n, _ := store.CountJobsByStatus(state.JobPending); n != 1 {
		t.Fatalf("retry didn't create pending job; got %d", n)
	}
}

// TestRunRetryFailed_HappyPath: cancel a 3-task run after task A
// succeeded, retry-failed → new run has only B and C with depends
// rewired.
func TestRunRetryFailed_HappyPath(t *testing.T) {
	srv, store := controlHarness(t)
	payload := `{"Name":"daily","RunID":"R1","Tasks":[
		{"Name":"A","Depends":null},
		{"Name":"B","Depends":["A"]},
		{"Name":"C","Depends":["B"]}
	]}`
	_ = store.Enqueue("R1", "daily", []byte(payload))
	_, _ = store.Claim("W_A", time.Minute)
	apply := func(line string) {
		ev, _ := state.DecodeEvent([]byte(line))
		_ = store.Apply(ev)
	}
	apply(`{"time":"2026-05-10T12:00:00Z","entry_type":"schedule_start","run_id":"R1","schedule":"daily","tasks":["A","B","C"]}`)
	apply(`{"time":"2026-05-10T12:00:01Z","entry_type":"shell_run","run_id":"R1","schedule":"daily","task":"A","exit":0,"duration_ms":5,"success":true}`)
	apply(`{"time":"2026-05-10T12:00:02Z","entry_type":"shell_run","run_id":"R1","schedule":"daily","task":"B","exit":1,"duration_ms":5,"success":false,"error":"boom"}`)
	_, _ = store.Cancel("R1")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/R1/retry-failed", nil)
	req.Header.Set("Authorization", "Bearer secret")
	srv.handleRunRoute(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	var res state.RetryResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.SkippedTasks) != 1 || res.SkippedTasks[0] != "A" {
		t.Fatalf("expected skipped=[A], got %v", res.SkippedTasks)
	}
}

// TestRunRetry_StillInFlight: in-flight run → 400.
func TestRunRetry_StillInFlight(t *testing.T) {
	srv, store := controlHarness(t)
	_ = store.Enqueue("R1", "x", []byte(`{}`))
	_, _ = store.Claim("W_A", time.Minute)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/R1/retry", nil)
	req.Header.Set("Authorization", "Bearer secret")
	srv.handleRunRoute(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rr.Code)
	}
}

// TestSSE_PingAndCancel: a worker subscribes via /v1/workers/W/control,
// receives the periodic ping (we shrink the test by sending a Push
// directly), then a cancel.
func TestSSE_PingAndCancel(t *testing.T) {
	srv, store := controlHarness(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/workers/", srv.handleWorkerRoute)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/v1/workers/W_A/control", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type: %q", ct)
	}

	// Push a cancel; expect to read it from the SSE stream.
	go func() {
		// Brief wait so the handler has had time to register the
		// subscription before we push.
		time.Sleep(50 * time.Millisecond)
		_ = store.PushControl("W_A", state.ControlMsg{Type: "cancel", RunID: "R42"})
	}()

	scanner := bufio.NewScanner(resp.Body)
	deadline := time.Now().Add(2 * time.Second)
	gotCancel := false
	for time.Now().Before(deadline) && !gotCancel {
		if !scanner.Scan() {
			break
		}
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			body := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if strings.Contains(body, `"cancel"`) && strings.Contains(body, `"R42"`) {
				gotCancel = true
				break
			}
		}
	}
	if !gotCancel {
		t.Fatal("did not receive cancel SSE message")
	}
}
