package state

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// TestCancel_Pending: a job that's still queued (never claimed) → moves
// to canceled in the queue and projection.
func TestCancel_Pending(t *testing.T) {
	s := newQueueTestStore(t)
	_ = s.Enqueue("R1", "x", []byte(`{"RunID":"R1","Schedule":"x","Tasks":[]}`))

	res, err := s.Cancel("R1")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !res.WasPending || res.WasClaimed || res.WasUnknown {
		t.Fatalf("flags: %+v", res)
	}
	if n, _ := s.CountJobsByStatus(JobCanceled); n != 1 {
		t.Fatalf("expected 1 canceled job, got %d", n)
	}
}

// TestCancel_Claimed: a job in flight → marks canceled and returns the
// worker_id so the SSE pusher knows where to send the signal.
func TestCancel_Claimed(t *testing.T) {
	s := newQueueTestStore(t)
	_ = s.Enqueue("R1", "x", []byte(`{"RunID":"R1","Schedule":"x"}`))
	_, _ = s.Claim("W_A", time.Minute)

	res, err := s.Cancel("R1")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !res.WasClaimed || res.WorkerID != "W_A" {
		t.Fatalf("expected claimed by W_A: %+v", res)
	}
	// Heartbeat after cancel should fail (job no longer claimed).
	if err := s.Heartbeat("R1", "W_A", time.Minute); err == nil {
		t.Fatalf("heartbeat after cancel should fail")
	}
}

// TestCancel_Terminal: cancel on done/failed job → ErrNotCancelable.
func TestCancel_Terminal(t *testing.T) {
	s := newQueueTestStore(t)
	_ = s.Enqueue("R1", "x", []byte(`{}`))
	_, _ = s.Claim("W_A", time.Minute)
	_ = s.Ack("R1", "W_A", true, "")

	if _, err := s.Cancel("R1"); !errors.Is(err, ErrNotCancelable) {
		t.Fatalf("expected ErrNotCancelable, got %v", err)
	}
}

// TestCancel_UnknownButProjected: the run exists in the projection but
// not in the queue (e.g., default-mode runs that never went through
// --queue self). Should still mark the projection canceled.
func TestCancel_UnknownButProjected(t *testing.T) {
	s := newQueueTestStore(t)
	// Apply a schedule_start so a runs row exists, no jobs row.
	applyLine(t, s, `{"time":"2026-05-10T12:00:00Z","entry_type":"schedule_start","run_id":"R_NOQ","schedule":"x","tasks":["a"]}`)

	res, err := s.Cancel("R_NOQ")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !res.WasUnknown {
		t.Fatalf("expected WasUnknown, got %+v", res)
	}
	r, _ := s.GetRun("R_NOQ")
	if r.Status != StatusCanceled {
		t.Fatalf("run status: got %s, want canceled", r.Status)
	}
}

// TestRetry_HappyPath: ack done, then retry → new pending row with the
// new run_id; old row stays as 'done'.
func TestRetry_HappyPath(t *testing.T) {
	s := newQueueTestStore(t)
	_ = s.Enqueue("R1", "daily", []byte(`{"RunID":"R1","Schedule":"daily"}`))
	_, _ = s.Claim("W_A", time.Minute)
	_ = s.Ack("R1", "W_A", false, "boom")

	res, err := s.Retry("R1", "R1-retry")
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if res.NewRunID != "R1-retry" || res.Schedule != "daily" {
		t.Fatalf("retry result wrong: %+v", res)
	}
	if n, _ := s.CountJobsByStatus(JobPending); n != 1 {
		t.Fatalf("expected 1 pending after retry, got %d", n)
	}
	// Original is preserved as failed.
	if n, _ := s.CountJobsByStatus(JobFailedQ); n != 1 {
		t.Fatalf("original should still be failed, got %d", n)
	}
}

// TestRetry_StillInFlight: cannot retry a job that hasn't terminated.
func TestRetry_StillInFlight(t *testing.T) {
	s := newQueueTestStore(t)
	_ = s.Enqueue("R1", "x", []byte(`{}`))
	_, _ = s.Claim("W_A", time.Minute)

	if _, err := s.Retry("R1", "R1-retry"); err == nil {
		t.Fatalf("expected error for in-flight retry")
	}
}

// TestResume_OnlyNonSucceededTasksReQueued: a 4-task DAG where the
// first two succeed and the last two cancel. Resume should produce a
// new run with only the last two tasks, depends stripped of refs to
// the skipped tasks.
func TestResume_OnlyNonSucceededTasksReQueued(t *testing.T) {
	s := newQueueTestStore(t)
	payload := `{"Name":"daily","RunID":"R1","Tasks":[
		{"Name":"A","Depends":null},
		{"Name":"B","Depends":["A"]},
		{"Name":"C","Depends":["B"]},
		{"Name":"D","Depends":["C"]}
	]}`
	_ = s.Enqueue("R1", "daily", []byte(payload))
	_, _ = s.Claim("W_A", time.Minute)

	// Mark per-task state via the projection.
	applyLine(t, s, `{"time":"2026-05-10T12:00:00Z","entry_type":"schedule_start","run_id":"R1","schedule":"daily","tasks":["A","B","C","D"]}`)
	applyLine(t, s, `{"time":"2026-05-10T12:00:01Z","entry_type":"shell_run","run_id":"R1","schedule":"daily","task":"A","exit":0,"duration_ms":5,"success":true}`)
	applyLine(t, s, `{"time":"2026-05-10T12:00:02Z","entry_type":"shell_run","run_id":"R1","schedule":"daily","task":"B","exit":0,"duration_ms":5,"success":true}`)
	applyLine(t, s, `{"time":"2026-05-10T12:00:03Z","entry_type":"shell_run","run_id":"R1","schedule":"daily","task":"C","exit":1,"duration_ms":5,"success":false,"error":"boom"}`)

	// Operator cancels (run + remaining tasks become canceled).
	if _, err := s.Cancel("R1"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	res, err := s.RetryFailed("R1", "R1-resume")
	if err != nil {
		t.Fatalf("RetryFailed: %v", err)
	}
	if res.NewRunID != "R1-resume" || res.Schedule != "daily" {
		t.Fatalf("resume result: %+v", res)
	}
	if len(res.SkippedTasks) != 2 {
		t.Fatalf("expected 2 skipped, got %v", res.SkippedTasks)
	}

	// Inspect the new payload: should have only C and D, with C's
	// depends=[B] stripped (B was skipped), D's depends=[C] kept.
	var stored string
	_ = s.db.QueryRow(`SELECT payload FROM jobs WHERE run_id = ?`, "R1-resume").Scan(&stored)
	var raw map[string]any
	if err := json.Unmarshal([]byte(stored), &raw); err != nil {
		t.Fatalf("decode new payload: %v", err)
	}
	tasks, _ := raw["Tasks"].([]any)
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks in resumed payload, got %d", len(tasks))
	}
	first, _ := tasks[0].(map[string]any)
	second, _ := tasks[1].(map[string]any)
	if first["Name"] != "C" || second["Name"] != "D" {
		t.Fatalf("expected C, D in order; got %v, %v", first["Name"], second["Name"])
	}
	// C's depends should be nil (only dep B was skipped).
	if first["Depends"] != nil {
		t.Fatalf("expected C.Depends=nil after skip-strip, got %v", first["Depends"])
	}
	// D's depends should still contain C.
	deps, _ := second["Depends"].([]any)
	if len(deps) != 1 || deps[0] != "C" {
		t.Fatalf("expected D.Depends=[C], got %v", second["Depends"])
	}
}

// TestResume_NothingLeft: every task succeeded → resume returns an
// error so the operator knows there's no remaining work.
func TestResume_NothingLeft(t *testing.T) {
	s := newQueueTestStore(t)
	payload := `{"Name":"x","RunID":"R1","Tasks":[{"Name":"only","Depends":null}]}`
	_ = s.Enqueue("R1", "x", []byte(payload))
	_, _ = s.Claim("W_A", time.Minute)

	applyLine(t, s, `{"time":"2026-05-10T12:00:00Z","entry_type":"schedule_start","run_id":"R1","schedule":"x","tasks":["only"]}`)
	applyLine(t, s, `{"time":"2026-05-10T12:00:01Z","entry_type":"shell_run","run_id":"R1","schedule":"x","task":"only","exit":0,"duration_ms":5,"success":true}`)
	applyLine(t, s, `{"time":"2026-05-10T12:00:02Z","entry_type":"schedule_complete","run_id":"R1","schedule":"x","task_count":1,"duration_ms":10,"success":true}`)

	_, err := s.RetryFailed("R1", "R1-resume")
	if err == nil {
		t.Fatalf("expected error when every task already succeeded")
	}
}

// TestResume_StillInFlight: cannot resume a run that's still running.
func TestResume_StillInFlight(t *testing.T) {
	s := newQueueTestStore(t)
	_ = s.Enqueue("R1", "x", []byte(`{"Name":"x","RunID":"R1","Tasks":[]}`))
	_, _ = s.Claim("W_A", time.Minute)

	if _, err := s.RetryFailed("R1", "R1-resume"); err == nil {
		t.Fatalf("expected error for in-flight resume")
	}
}

// TestRetry_Unknown: cannot retry a run that never went through the
// queue.
func TestRetry_Unknown(t *testing.T) {
	s := newQueueTestStore(t)
	if _, err := s.Retry("nope", "newrun"); err == nil {
		t.Fatalf("expected error for unknown run")
	}
}

// TestSubscribe_PushControl: a subscribed worker receives messages.
func TestSubscribe_PushControl(t *testing.T) {
	s := newQueueTestStore(t)
	ch, unsub := s.Subscribe("W_A")
	defer unsub()

	if !s.PushControl("W_A", ControlMsg{Type: "cancel", RunID: "R1"}) {
		t.Fatalf("push should succeed")
	}
	select {
	case m := <-ch:
		if m.Type != "cancel" || m.RunID != "R1" {
			t.Fatalf("got: %+v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive control msg")
	}
}

// TestPushControl_NoSubscriber: PushControl on an unknown worker
// returns false but doesn't error — the cancel is still recorded in
// the DB and heartbeat will pick it up.
func TestPushControl_NoSubscriber(t *testing.T) {
	s := newQueueTestStore(t)
	if s.PushControl("ghost", ControlMsg{Type: "cancel"}) {
		t.Fatalf("expected false for unknown worker")
	}
}

// TestSubscribe_Reconnect: re-subscribing closes the old channel
// (so the previous SSE goroutine exits cleanly).
func TestSubscribe_Reconnect(t *testing.T) {
	s := newQueueTestStore(t)
	ch1, _ := s.Subscribe("W_A")
	ch2, unsub2 := s.Subscribe("W_A")
	defer unsub2()

	// First channel should now be closed.
	select {
	case _, ok := <-ch1:
		if ok {
			t.Fatalf("ch1 should be closed")
		}
	case <-time.After(time.Second):
		t.Fatal("ch1 not closed within deadline")
	}
	if !s.PushControl("W_A", ControlMsg{Type: "cancel"}) {
		t.Fatalf("push to fresh sub should succeed")
	}
	<-ch2 // consume so unsub doesn't deadlock
}

// TestListWorkers_StatusDerivation: claim → active; ack → idle;
// stale time → stale.
func TestListWorkers_StatusDerivation(t *testing.T) {
	s := newQueueTestStore(t)
	_ = s.Enqueue("R1", "x", []byte(`{}`))
	_, _ = s.Claim("W_A", time.Minute)

	ws, err := s.ListWorkers()
	if err != nil {
		t.Fatalf("ListWorkers: %v", err)
	}
	if len(ws) != 1 || ws[0].WorkerID != "W_A" {
		t.Fatalf("workers: %+v", ws)
	}
	if ws[0].Status != "active" || ws[0].CurrentRun != "R1" {
		t.Fatalf("active wrong: %+v", ws[0])
	}
	if ws[0].RunsTotal != 1 || ws[0].RunsFailed != 0 {
		t.Fatalf("counts wrong: %+v", ws[0])
	}

	_ = s.Ack("R1", "W_A", false, "boom")
	ws, _ = s.ListWorkers()
	if ws[0].Status != "idle" || ws[0].CurrentRun != "" {
		t.Fatalf("post-ack: %+v", ws[0])
	}
	if ws[0].RunsFailed != 1 {
		t.Fatalf("failed counter: got %d, want 1", ws[0].RunsFailed)
	}
}

// TestCancel_StickyAgainstLateTerminalEvent: when Cancel wins the
// race against a post-SIGTERM shell_run event, the projection must
// not flip back to 'failed'. The operator's intent — "this run was
// canceled" — is what surfaces in /v1/runs.
func TestCancel_StickyAgainstLateTerminalEvent(t *testing.T) {
	s := newQueueTestStore(t)
	_ = s.Enqueue("R1", "x", []byte(`{}`))
	_, _ = s.Claim("W_A", time.Minute)

	// Seed a runs+task row in 'running' via the projection.
	applyLine(t, s, `{"time":"2026-05-10T12:00:00Z","entry_type":"schedule_start","run_id":"R1","schedule":"x","tasks":["only"]}`)
	applyLine(t, s, `{"time":"2026-05-10T12:00:01Z","entry_type":"task_start","run_id":"R1","schedule":"x","task":"only","attempt":1}`)

	// Operator cancels.
	if _, err := s.Cancel("R1"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	// Worker's SIGTERM'd shell now reports back. Status should NOT
	// flip from canceled to failed.
	applyLine(t, s, `{"time":"2026-05-10T12:00:02Z","entry_type":"shell_run","run_id":"R1","schedule":"x","task":"only","exit":-1,"duration_ms":50,"success":false,"error":"signal: terminated"}`)
	applyLine(t, s, `{"time":"2026-05-10T12:00:02Z","entry_type":"schedule_complete","run_id":"R1","schedule":"x","task_count":1,"duration_ms":80,"success":false,"error":"signal: terminated"}`)

	r, _ := s.GetRun("R1")
	if r.Status != StatusCanceled {
		t.Fatalf("run status: got %s, want canceled (sticky)", r.Status)
	}
	if r.Tasks[0].Status != StatusCanceled {
		t.Fatalf("task status: got %s, want canceled (sticky)", r.Tasks[0].Status)
	}
}

// TestUpsertWorker_PreservesHostOnEmpty: an empty host arg doesn't
// blow away an existing populated host.
func TestUpsertWorker_PreservesHostOnEmpty(t *testing.T) {
	s := newQueueTestStore(t)
	if err := s.UpsertWorker("W_A", "ec2-east-1"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.UpsertWorker("W_A", ""); err != nil {
		t.Fatalf("upsert empty: %v", err)
	}
	ws, _ := s.ListWorkers()
	if ws[0].Host != "ec2-east-1" {
		t.Fatalf("host clobbered: %q", ws[0].Host)
	}
}
