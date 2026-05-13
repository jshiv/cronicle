package state

import (
	"encoding/json"
	"testing"
	"time"
)

// TestRetryTask_KeepsTargetAndDependents: a 4-task run finished with B
// failed; retrying B should produce a fresh payload containing only B,
// C, D (B's transitive dependents) with depends on A stripped.
func TestRetryTask_KeepsTargetAndDependents(t *testing.T) {
	s := newQueueTestStore(t)
	payload := `{"Name":"chain","RunID":"R1","Tasks":[
		{"Name":"A","Depends":null},
		{"Name":"B","Depends":["A"]},
		{"Name":"C","Depends":["B"]},
		{"Name":"D","Depends":["C"]}
	]}`
	_ = s.Enqueue("R1", "chain", []byte(payload))
	_, _ = s.Claim("W_A", time.Minute)

	apply := func(line string) {
		ev, _ := DecodeEvent([]byte(line))
		_ = s.Apply(ev)
	}
	apply(`{"time":"2026-05-12T12:00:00Z","entry_type":"schedule_start","run_id":"R1","schedule":"chain","tasks":["A","B","C","D"]}`)
	apply(`{"time":"2026-05-12T12:00:01Z","entry_type":"shell_run","run_id":"R1","schedule":"chain","task":"A","exit":0,"duration_ms":5,"success":true}`)
	apply(`{"time":"2026-05-12T12:00:02Z","entry_type":"shell_run","run_id":"R1","schedule":"chain","task":"B","exit":1,"duration_ms":5,"success":false,"error":"boom"}`)
	if _, err := s.Cancel("R1"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	keep := map[string]bool{"B": true, "C": true, "D": true}
	res, err := s.RetryTask("R1", "B", keep, "R1-retryB")
	if err != nil {
		t.Fatalf("RetryTask: %v", err)
	}
	if res.NewRunID != "R1-retryB" || res.Schedule != "chain" {
		t.Fatalf("result: %+v", res)
	}
	// SkippedTasks should be ["A"] (the predecessor dropped from the
	// new payload).
	if len(res.SkippedTasks) != 1 || res.SkippedTasks[0] != "A" {
		t.Fatalf("expected SkippedTasks=[A], got %v", res.SkippedTasks)
	}

	// Inspect the new payload.
	var stored string
	_ = s.db.QueryRow(`SELECT payload FROM jobs WHERE run_id = ?`, "R1-retryB").Scan(&stored)
	var raw map[string]any
	_ = json.Unmarshal([]byte(stored), &raw)
	tasks, _ := raw["Tasks"].([]any)
	if len(tasks) != 3 {
		t.Fatalf("expected 3 tasks (B,C,D) in retry payload, got %d", len(tasks))
	}
	first, _ := tasks[0].(map[string]any)
	if first["Name"] != "B" {
		t.Fatalf("first task should be B, got %v", first["Name"])
	}
	// B's depends on A should be stripped (A is filtered out).
	if first["Depends"] != nil {
		t.Fatalf("expected B.Depends=nil after dependency strip, got %v", first["Depends"])
	}
	// C's depends should still contain B.
	second, _ := tasks[1].(map[string]any)
	deps, _ := second["Depends"].([]any)
	if len(deps) != 1 || deps[0] != "B" {
		t.Fatalf("C.Depends should be [B], got %v", second["Depends"])
	}
}

// TestRetryTask_HeadOnlyWhenNoCascade: a task with no dependents, like
// a leaf, retries as a singleton run.
func TestRetryTask_HeadOnlyWhenNoCascade(t *testing.T) {
	s := newQueueTestStore(t)
	payload := `{"Name":"chain","RunID":"R2","Tasks":[
		{"Name":"A","Depends":null},
		{"Name":"B","Depends":["A"]}
	]}`
	_ = s.Enqueue("R2", "chain", []byte(payload))
	_, _ = s.Claim("W", time.Minute)
	apply := func(line string) {
		ev, _ := DecodeEvent([]byte(line))
		_ = s.Apply(ev)
	}
	apply(`{"time":"2026-05-12T12:00:00Z","entry_type":"schedule_start","run_id":"R2","schedule":"chain","tasks":["A","B"]}`)
	apply(`{"time":"2026-05-12T12:00:01Z","entry_type":"shell_run","run_id":"R2","schedule":"chain","task":"A","exit":0,"duration_ms":1,"success":true}`)
	apply(`{"time":"2026-05-12T12:00:02Z","entry_type":"shell_run","run_id":"R2","schedule":"chain","task":"B","exit":1,"duration_ms":1,"success":false}`)
	_, _ = s.Cancel("R2")

	res, err := s.RetryTask("R2", "B", map[string]bool{"B": true}, "R2-retry")
	if err != nil {
		t.Fatalf("RetryTask: %v", err)
	}
	var stored string
	_ = s.db.QueryRow(`SELECT payload FROM jobs WHERE run_id = ?`, "R2-retry").Scan(&stored)
	var raw map[string]any
	_ = json.Unmarshal([]byte(stored), &raw)
	tasks, _ := raw["Tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task in retry payload, got %d", len(tasks))
	}
	if !contains(res.SkippedTasks, "A") {
		t.Fatalf("A should be dropped (skipped), got %v", res.SkippedTasks)
	}
}

func TestRetryTask_StillInFlight(t *testing.T) {
	s := newQueueTestStore(t)
	_ = s.Enqueue("R3", "x", []byte(`{"Name":"x","RunID":"R3","Tasks":[{"Name":"a"}]}`))
	_, _ = s.Claim("W", time.Minute)

	_, err := s.RetryTask("R3", "a", map[string]bool{"a": true}, "R3-retry")
	if err == nil {
		t.Fatalf("expected error for in-flight RetryTask")
	}
}

func TestRetryTask_UnknownTask(t *testing.T) {
	s := newQueueTestStore(t)
	payload := `{"Name":"x","RunID":"R4","Tasks":[{"Name":"a"}]}`
	_ = s.Enqueue("R4", "x", []byte(payload))
	_, _ = s.Claim("W", time.Minute)
	apply := func(line string) {
		ev, _ := DecodeEvent([]byte(line))
		_ = s.Apply(ev)
	}
	apply(`{"time":"2026-05-12T12:00:00Z","entry_type":"schedule_start","run_id":"R4","schedule":"x","tasks":["a"]}`)
	apply(`{"time":"2026-05-12T12:00:01Z","entry_type":"shell_run","run_id":"R4","schedule":"x","task":"a","exit":0,"duration_ms":1,"success":true}`)
	apply(`{"time":"2026-05-12T12:00:02Z","entry_type":"schedule_complete","run_id":"R4","schedule":"x","task_count":1,"duration_ms":2,"success":true}`)

	_, err := s.RetryTask("R4", "ghost", map[string]bool{"ghost": true}, "R4-retry")
	if err == nil {
		t.Fatalf("expected error for unknown task")
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
