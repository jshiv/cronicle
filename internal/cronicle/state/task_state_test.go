package state

import "testing"

func TestTaskSkippedRoundTrip(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Fresh: unknown task is active.
	if skipped, err := s.IsTaskSkipped("daily", "extract"); err != nil || skipped {
		t.Fatalf("expected unknown task active: skipped=%v err=%v", skipped, err)
	}

	// Skip.
	if err := s.SetTaskSkipped("daily", "extract", "alice", "broken upstream"); err != nil {
		t.Fatalf("SetTaskSkipped: %v", err)
	}
	if skipped, _ := s.IsTaskSkipped("daily", "extract"); !skipped {
		t.Fatalf("expected skipped after Set")
	}

	st, _ := s.GetTaskState("daily", "extract")
	if !st.Skipped || st.SkippedBy != "alice" || st.Reason != "broken upstream" || st.SkippedAt.IsZero() {
		t.Fatalf("state shape wrong: %+v", st)
	}

	// Idempotent re-skip preserves skipped_at.
	first := st.SkippedAt
	if err := s.SetTaskSkipped("daily", "extract", "bob", "still broken"); err != nil {
		t.Fatalf("re-skip: %v", err)
	}
	st2, _ := s.GetTaskState("daily", "extract")
	if !st2.SkippedAt.Equal(first) {
		t.Fatalf("skipped_at moved on idempotent skip: %v vs %v", first, st2.SkippedAt)
	}
	if st2.SkippedBy != "bob" || st2.Reason != "still broken" {
		t.Fatalf("latest actor/reason should win: %+v", st2)
	}

	// Unskip.
	if err := s.ClearTaskSkipped("daily", "extract", "bob"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if skipped, _ := s.IsTaskSkipped("daily", "extract"); skipped {
		t.Fatalf("expected active after Clear")
	}
}

func TestTaskSkipScopedToSchedule(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Same task name in two different schedules: skipping in one
	// must not affect the other.
	if err := s.SetTaskSkipped("daily", "extract", "a", ""); err != nil {
		t.Fatal(err)
	}
	if skipped, _ := s.IsTaskSkipped("hourly", "extract"); skipped {
		t.Fatalf("skip on daily/extract leaked to hourly/extract")
	}
	if skipped, _ := s.IsTaskSkipped("daily", "extract"); !skipped {
		t.Fatalf("daily/extract should still be skipped")
	}
}

func TestListSkippedTasksForSchedule(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	_ = s.SetTaskSkipped("daily", "a", "u", "")
	_ = s.SetTaskSkipped("daily", "b", "u", "")
	_ = s.SetTaskSkipped("daily", "c", "u", "")
	_ = s.ClearTaskSkipped("daily", "b", "u")
	_ = s.SetTaskSkipped("hourly", "x", "u", "") // different schedule, must not leak

	list, err := s.ListSkippedTasksForSchedule("daily")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 skipped in daily, got %d (%+v)", len(list), list)
	}
	names := map[string]bool{}
	for _, st := range list {
		names[st.Task] = true
	}
	if !names["a"] || !names["c"] || names["b"] {
		t.Fatalf("wrong members: %+v", names)
	}
}

// TestApplyTaskSkipped: a task_skipped event becomes a row with
// status='skipped' and duration_ms=0; the parent run still completes
// successfully when other tasks succeed.
func TestApplyTaskSkipped(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	feed := func(line string) {
		ev, ok := DecodeEvent([]byte(line))
		if !ok {
			t.Fatalf("decode: %s", line)
		}
		if err := s.Apply(ev); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}

	feed(`{"time":"2026-05-12T12:00:00Z","entry_type":"schedule_start","run_id":"R_SK","schedule":"daily","tasks":["a","b"]}`)
	feed(`{"time":"2026-05-12T12:00:01Z","entry_type":"task_start","run_id":"R_SK","schedule":"daily","task":"a","attempt":1}`)
	feed(`{"time":"2026-05-12T12:00:02Z","entry_type":"shell_run","run_id":"R_SK","schedule":"daily","task":"a","exit":0,"duration_ms":50,"success":true}`)
	feed(`{"time":"2026-05-12T12:00:03Z","entry_type":"task_skipped","run_id":"R_SK","schedule":"daily","task":"b","reason":"upstream-broken"}`)
	feed(`{"time":"2026-05-12T12:00:04Z","entry_type":"schedule_complete","run_id":"R_SK","schedule":"daily","task_count":2,"duration_ms":200,"success":true}`)

	r, err := s.GetRun("R_SK")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if r.Status != StatusSucceeded {
		t.Fatalf("run status: got %s, want succeeded (skipped task should not fail the run)", r.Status)
	}
	byName := map[string]Task{}
	for _, tk := range r.Tasks {
		byName[tk.Name] = tk
	}
	if byName["a"].Status != StatusSucceeded {
		t.Fatalf("task a: got %s, want succeeded", byName["a"].Status)
	}
	if byName["b"].Status != StatusSkipped {
		t.Fatalf("task b: got %s, want skipped", byName["b"].Status)
	}
	if byName["b"].DurationMs != 0 {
		t.Fatalf("skipped task duration should be 0, got %v", byName["b"].DurationMs)
	}
}

func TestTaskSkipEmptyArgsNoop(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if err := s.SetTaskSkipped("", "x", "u", ""); err == nil {
		t.Fatalf("expected error for empty schedule")
	}
	if err := s.SetTaskSkipped("daily", "", "u", ""); err == nil {
		t.Fatalf("expected error for empty task")
	}
	skipped, err := s.IsTaskSkipped("", "")
	if err != nil {
		t.Fatalf("IsTaskSkipped empty: %v", err)
	}
	if skipped {
		t.Fatalf("empty args should report not skipped")
	}
}
