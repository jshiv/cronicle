package state

import (
	"testing"
)

func TestSchedulePausedRoundTrip(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Fresh store: schedule is not paused.
	paused, err := s.IsSchedulePaused("daily")
	if err != nil {
		t.Fatalf("IsSchedulePaused: %v", err)
	}
	if paused {
		t.Fatalf("expected unknown schedule to be active, got paused")
	}

	// Pause.
	if err := s.SetSchedulePaused("daily", "user@example.com", "rolling out new query"); err != nil {
		t.Fatalf("SetSchedulePaused: %v", err)
	}
	paused, err = s.IsSchedulePaused("daily")
	if err != nil {
		t.Fatalf("IsSchedulePaused: %v", err)
	}
	if !paused {
		t.Fatalf("expected paused, got active")
	}

	// GetScheduleState reflects the metadata.
	st, err := s.GetScheduleState("daily")
	if err != nil {
		t.Fatalf("GetScheduleState: %v", err)
	}
	if !st.Paused {
		t.Fatalf("state.Paused: got false, want true")
	}
	if st.PausedBy != "user@example.com" {
		t.Fatalf("PausedBy: got %q", st.PausedBy)
	}
	if st.Reason != "rolling out new query" {
		t.Fatalf("Reason: got %q", st.Reason)
	}
	if st.PausedAt.IsZero() {
		t.Fatalf("PausedAt should be set after pause")
	}

	// Resume.
	if err := s.ClearSchedulePaused("daily", "user@example.com"); err != nil {
		t.Fatalf("ClearSchedulePaused: %v", err)
	}
	paused, err = s.IsSchedulePaused("daily")
	if err != nil {
		t.Fatalf("IsSchedulePaused: %v", err)
	}
	if paused {
		t.Fatalf("expected active after clear, got paused")
	}

	// Idempotent resume.
	if err := s.ClearSchedulePaused("daily", "user@example.com"); err != nil {
		t.Fatalf("idempotent clear: %v", err)
	}

	// Idempotent pause: paused_at must not advance on the second call.
	if err := s.SetSchedulePaused("daily", "user", "first"); err != nil {
		t.Fatalf("first pause: %v", err)
	}
	first, _ := s.GetScheduleState("daily")
	if err := s.SetSchedulePaused("daily", "user", "second"); err != nil {
		t.Fatalf("re-pause: %v", err)
	}
	second, _ := s.GetScheduleState("daily")
	if !second.PausedAt.Equal(first.PausedAt) {
		t.Fatalf("paused_at moved across idempotent pauses: %v vs %v", first.PausedAt, second.PausedAt)
	}
	if second.Reason != "second" {
		t.Fatalf("reason should reflect the latest call: got %q", second.Reason)
	}
}

func TestListPausedSchedules(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Pause three, resume one.
	if err := s.SetSchedulePaused("a", "u", "r"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSchedulePaused("b", "u", "r"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSchedulePaused("c", "u", "r"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearSchedulePaused("b", "u"); err != nil {
		t.Fatal(err)
	}

	list, err := s.ListPausedSchedules()
	if err != nil {
		t.Fatalf("ListPausedSchedules: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 paused, got %d (%+v)", len(list), list)
	}
	names := map[string]bool{}
	for _, st := range list {
		names[st.Name] = true
		if !st.Paused {
			t.Fatalf("paused list entry must have Paused=true: %+v", st)
		}
	}
	if !names["a"] || !names["c"] || names["b"] {
		t.Fatalf("wrong members: %+v", names)
	}
}

func TestEmptyScheduleNameNoop(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if err := s.SetSchedulePaused("", "u", ""); err == nil {
		t.Fatalf("expected error on empty name in SetSchedulePaused")
	}
	if err := s.ClearSchedulePaused("", "u"); err == nil {
		t.Fatalf("expected error on empty name in ClearSchedulePaused")
	}
	paused, err := s.IsSchedulePaused("")
	if err != nil {
		t.Fatalf("IsSchedulePaused empty: %v", err)
	}
	if paused {
		t.Fatalf("empty name should report not paused")
	}
}
