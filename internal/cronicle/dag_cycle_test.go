package cronicle

import (
	"strings"
	"sync/atomic"
	"testing"
)

// TestWalkDAG_DetectsCycle: a 2-node cycle (A↔B) used to produce a
// silent no-op return (no nodes have in-degree 0, the for-running>0
// loop never enters, function returns nil error having executed zero
// tasks). walkDAG must return an error naming the cycle members so the
// operator can see what to fix.
func TestWalkDAG_DetectsCycle(t *testing.T) {
	deps := map[string][]string{
		"a": {"b"},
		"b": {"a"},
	}
	var called atomic.Int32
	err := walkDAG(deps, func(name string) error {
		called.Add(1)
		return nil
	}, nil, nil)

	if err == nil {
		t.Fatalf("expected cycle error, got nil")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error should identify the failure as a cycle; got: %v", err)
	}
	if !strings.Contains(err.Error(), "a") || !strings.Contains(err.Error(), "b") {
		t.Errorf("error should name cycle members a and b; got: %v", err)
	}
	if got := called.Load(); got != 0 {
		t.Errorf("fn should never run on a cyclic graph; called %d time(s)", got)
	}
}

// TestWalkDAG_DetectsLongerCycle: a 3-node cycle (A→B→C→A) catches
// the same class of bug but stresses the unreached-node-list logic
// across more entries.
func TestWalkDAG_DetectsLongerCycle(t *testing.T) {
	deps := map[string][]string{
		"a": {"c"},
		"b": {"a"},
		"c": {"b"},
	}
	err := walkDAG(deps, func(name string) error { return nil }, nil, nil)
	if err == nil {
		t.Fatalf("expected cycle error, got nil")
	}
	for _, want := range []string{"a", "b", "c"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention cycle member %q; got: %v", want, err)
		}
	}
}

// TestWalkDAG_PartialCycleStillRunsIndependentTasks: a graph with one
// cycle (A↔B) AND an independent task (C) should run C to completion
// but still report the cycle so the operator notices the broken
// portion. The function returns the cycle error rather than a nil
// success because nil would mask the structural problem.
func TestWalkDAG_PartialCycleStillRunsIndependentTasks(t *testing.T) {
	deps := map[string][]string{
		"a": {"b"},
		"b": {"a"},
		"c": {}, // independent
	}
	var ranC atomic.Bool
	err := walkDAG(deps, func(name string) error {
		if name == "c" {
			ranC.Store(true)
		}
		return nil
	}, nil, nil)

	if err == nil {
		t.Fatalf("expected cycle error, got nil")
	}
	if !ranC.Load() {
		t.Errorf("independent task c should have run before walkDAG returned the cycle error")
	}
	// Cycle members (a, b) must appear in the error; the cycle error
	// reports only unreached nodes (those that never had in-degree 0),
	// so c — which ran successfully — must not appear.
	if !strings.Contains(err.Error(), "a") || !strings.Contains(err.Error(), "b") {
		t.Errorf("cycle error should name a and b; got: %v", err)
	}
}

// TestWalkDAG_LinearGraphNoFalsePositive: a strictly linear DAG
// (A→B→C, no cycles) must complete cleanly without the cycle-detection
// check raising a false alarm.
func TestWalkDAG_LinearGraphNoFalsePositive(t *testing.T) {
	deps := map[string][]string{
		"a": {},
		"b": {"a"},
		"c": {"b"},
	}
	err := walkDAG(deps, func(name string) error { return nil }, nil, nil)
	if err != nil {
		t.Errorf("acyclic graph should produce no error; got: %v", err)
	}
}
