package state

import (
	"testing"
)

// applyLine: feed a JSONL through DecodeEvent+Apply, panicing on
// failure. Helper shared with store_test.go but redefined locally so we
// can compose multiple suites without coupling test files.
func feed(t *testing.T, s *Store, line string) {
	t.Helper()
	ev, ok := DecodeEvent([]byte(line))
	if !ok {
		t.Fatalf("DecodeEvent rejected: %s", line)
	}
	if err := s.Apply(ev); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

func TestCancelTask_HeadOnly(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()

	feed(t, s, `{"time":"2026-05-12T12:00:00Z","entry_type":"schedule_start","run_id":"R1","schedule":"daily","tasks":["a","b","c"]}`)
	feed(t, s, `{"time":"2026-05-12T12:00:01Z","entry_type":"task_start","run_id":"R1","schedule":"daily","task":"a","attempt":1}`)

	res, err := s.CancelTask("R1", "a", nil)
	if err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if len(res.CanceledTasks) != 1 || res.CanceledTasks[0] != "a" {
		t.Fatalf("expected [a] canceled, got %+v", res)
	}

	canceled, err := s.IsTaskCanceledInRun("R1", "a")
	if err != nil || !canceled {
		t.Fatalf("expected a canceled in projection: canceled=%v err=%v", canceled, err)
	}
	// Tasks b and c should still be queued (no rows yet → not canceled).
	if canceled, _ := s.IsTaskCanceledInRun("R1", "b"); canceled {
		t.Fatalf("b should not be canceled — no cascade was passed")
	}
}

func TestCancelTask_WithCascade(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()

	feed(t, s, `{"time":"2026-05-12T12:00:00Z","entry_type":"schedule_start","run_id":"R2","schedule":"chain","tasks":["a","b","c"]}`)
	feed(t, s, `{"time":"2026-05-12T12:00:01Z","entry_type":"task_start","run_id":"R2","schedule":"chain","task":"a","attempt":1}`)

	// Cancel a, cascade b and c (the dependents).
	res, err := s.CancelTask("R2", "a", []string{"b", "c"})
	if err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if len(res.CanceledTasks) != 3 {
		t.Fatalf("expected 3 canceled (a,b,c), got %+v", res)
	}
	for _, name := range []string{"a", "b", "c"} {
		canceled, _ := s.IsTaskCanceledInRun("R2", name)
		if !canceled {
			t.Fatalf("%s not canceled after cascade", name)
		}
	}
}

func TestCancelTask_RespectsTerminalTasks(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()

	feed(t, s, `{"time":"2026-05-12T12:00:00Z","entry_type":"schedule_start","run_id":"R3","schedule":"daily","tasks":["a","b","c"]}`)
	feed(t, s, `{"time":"2026-05-12T12:00:01Z","entry_type":"task_start","run_id":"R3","schedule":"daily","task":"a","attempt":1}`)
	feed(t, s, `{"time":"2026-05-12T12:00:02Z","entry_type":"shell_run","run_id":"R3","schedule":"daily","task":"a","exit":0,"duration_ms":50,"success":true}`)
	// a is now terminal (succeeded). Cancel attempt should reject it
	// as a head, but with c as head + a as cascade entry, c gets
	// canceled and a is skipped-terminal.
	feed(t, s, `{"time":"2026-05-12T12:00:03Z","entry_type":"task_start","run_id":"R3","schedule":"daily","task":"b","attempt":1}`)

	// Head=b (in-flight), cascade=[c, a]. c should land canceled,
	// a should be reported as skipped_terminal because it succeeded.
	res, err := s.CancelTask("R3", "b", []string{"c", "a"})
	if err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if len(res.CanceledTasks) != 2 {
		t.Fatalf("expected b+c canceled, got %+v", res)
	}
	if len(res.SkippedTerminal) != 1 || res.SkippedTerminal[0] != "a" {
		t.Fatalf("expected a as skipped_terminal, got %+v", res.SkippedTerminal)
	}
}

func TestCancelTask_RejectsTerminalHead(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()

	feed(t, s, `{"time":"2026-05-12T12:00:00Z","entry_type":"schedule_start","run_id":"R4","schedule":"daily","tasks":["a"]}`)
	feed(t, s, `{"time":"2026-05-12T12:00:01Z","entry_type":"task_start","run_id":"R4","schedule":"daily","task":"a","attempt":1}`)
	feed(t, s, `{"time":"2026-05-12T12:00:02Z","entry_type":"shell_run","run_id":"R4","schedule":"daily","task":"a","exit":0,"duration_ms":50,"success":true}`)

	_, err := s.CancelTask("R4", "a", nil)
	if err != ErrTaskNotCancelable {
		t.Fatalf("expected ErrTaskNotCancelable, got %v", err)
	}
}

func TestCancelTask_RejectsMissingHead(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()

	feed(t, s, `{"time":"2026-05-12T12:00:00Z","entry_type":"schedule_start","run_id":"R5","schedule":"daily","tasks":["a"]}`)

	_, err := s.CancelTask("R5", "ghost", nil)
	if err == nil {
		t.Fatalf("expected error for missing head task")
	}
}

// TestCancelTask_StickyAgainstLateTerminal: a task is canceled before
// its body finishes; when the late shell_run arrives with success=true,
// the projection must keep status='canceled' (sticky-cancel rule).
func TestCancelTask_StickyAgainstLateTerminal(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()

	feed(t, s, `{"time":"2026-05-12T12:00:00Z","entry_type":"schedule_start","run_id":"R6","schedule":"daily","tasks":["a"]}`)
	feed(t, s, `{"time":"2026-05-12T12:00:01Z","entry_type":"task_start","run_id":"R6","schedule":"daily","task":"a","attempt":1}`)

	if _, err := s.CancelTask("R6", "a", nil); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	// Late terminal event (success=true, but we already canceled).
	feed(t, s, `{"time":"2026-05-12T12:00:02Z","entry_type":"shell_run","run_id":"R6","schedule":"daily","task":"a","exit":0,"duration_ms":1,"success":true}`)

	r, _ := s.GetRun("R6")
	if r.Tasks[0].Status != StatusCanceled {
		t.Fatalf("expected sticky canceled, got %s", r.Tasks[0].Status)
	}
}
