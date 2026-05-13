package state

import "testing"

func TestRunnerStateRoundTrip(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Fresh: not drained.
	drained, err := s.IsDrained()
	if err != nil {
		t.Fatalf("IsDrained: %v", err)
	}
	if drained {
		t.Fatalf("expected fresh runner not drained")
	}

	if err := s.SetDrained("alice", "deploy in progress"); err != nil {
		t.Fatalf("SetDrained: %v", err)
	}
	if drained, _ := s.IsDrained(); !drained {
		t.Fatalf("expected drained after Set")
	}

	st, _ := s.GetRunnerState()
	if !st.Drained || st.DrainedBy != "alice" || st.Reason != "deploy in progress" || st.DrainedAt.IsZero() {
		t.Fatalf("state shape wrong: %+v", st)
	}

	// Idempotent re-drain preserves drained_at.
	first := st.DrainedAt
	if err := s.SetDrained("bob", "second"); err != nil {
		t.Fatalf("re-drain: %v", err)
	}
	st2, _ := s.GetRunnerState()
	if !st2.DrainedAt.Equal(first) {
		t.Fatalf("drained_at moved on idempotent drain: %v vs %v", first, st2.DrainedAt)
	}
	if st2.DrainedBy != "bob" || st2.Reason != "second" {
		t.Fatalf("latest actor/reason should win: %+v", st2)
	}

	// Undrain.
	if err := s.ClearDrained("alice"); err != nil {
		t.Fatalf("ClearDrained: %v", err)
	}
	if drained, _ := s.IsDrained(); drained {
		t.Fatalf("expected active after ClearDrained")
	}

	// Idempotent undrain.
	if err := s.ClearDrained("alice"); err != nil {
		t.Fatalf("idempotent undrain: %v", err)
	}
}
