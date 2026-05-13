package state

import "testing"

func TestRunPauseRoundTrip(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if paused, _ := s.IsRunPaused("R1"); paused {
		t.Fatalf("fresh run should not be paused")
	}

	if err := s.PauseRun("R1", "alice", "investigation"); err != nil {
		t.Fatalf("PauseRun: %v", err)
	}
	paused, _ := s.IsRunPaused("R1")
	if !paused {
		t.Fatalf("expected paused")
	}

	st, _ := s.GetRunState("R1")
	if !st.Paused || st.PausedBy != "alice" || st.Reason != "investigation" || st.PausedAt.IsZero() {
		t.Fatalf("state shape wrong: %+v", st)
	}

	// Idempotent re-pause preserves paused_at.
	first := st.PausedAt
	if err := s.PauseRun("R1", "bob", "different"); err != nil {
		t.Fatalf("re-pause: %v", err)
	}
	st2, _ := s.GetRunState("R1")
	if !st2.PausedAt.Equal(first) {
		t.Fatalf("paused_at moved on idempotent pause: %v vs %v", first, st2.PausedAt)
	}
	if st2.PausedBy != "bob" || st2.Reason != "different" {
		t.Fatalf("latest actor/reason should win: %+v", st2)
	}

	// Resume.
	if err := s.ResumeRun("R1", "alice"); err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}
	if paused, _ := s.IsRunPaused("R1"); paused {
		t.Fatalf("expected active after ResumeRun")
	}

	// Idempotent resume.
	if err := s.ResumeRun("R1", "alice"); err != nil {
		t.Fatalf("idempotent resume: %v", err)
	}
}

func TestRunPauseEmptyArgsNoop(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if err := s.PauseRun("", "u", ""); err == nil {
		t.Fatalf("expected error for empty run_id")
	}
	if err := s.ResumeRun("", "u"); err == nil {
		t.Fatalf("expected error for empty run_id")
	}
	paused, err := s.IsRunPaused("")
	if err != nil {
		t.Fatalf("IsRunPaused empty: %v", err)
	}
	if paused {
		t.Fatalf("empty run_id should report not paused")
	}
}
