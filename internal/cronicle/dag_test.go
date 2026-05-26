package cronicle

import (
	"errors"
	"sync"
	"testing"
)

// TestWalkDAG_DepFailureSkipsDownstream: when a root task fails, all
// transitive dependents are skipped (fn never called) and onDepFailed
// fires for each. walkDAG returns the original error.
//
// DAG: a -> b -> c
func TestWalkDAG_DepFailureSkipsDownstream(t *testing.T) {
	deps := map[string][]string{
		"a": {},
		"b": {"a"},
		"c": {"b"},
	}

	var mu sync.Mutex
	called := map[string]bool{}
	depFailed := map[string]string{} // node -> failedDep

	errA := errors.New("task a exploded")

	err := walkDAG(deps, func(name string) error {
		mu.Lock()
		called[name] = true
		mu.Unlock()
		if name == "a" {
			return errA
		}
		return nil
	}, nil, func(node, failedDep string) {
		mu.Lock()
		depFailed[node] = failedDep
		mu.Unlock()
	})

	if err == nil {
		t.Fatalf("expected walkDAG to return an error, got nil")
	}
	if err.Error() != errA.Error() {
		t.Fatalf("expected error %q, got %q", errA, err)
	}

	if !called["a"] {
		t.Fatalf("fn(a) should have been called")
	}
	if called["b"] {
		t.Fatalf("fn(b) should NOT have been called (depends on failed a)")
	}
	if called["c"] {
		t.Fatalf("fn(c) should NOT have been called (transitively depends on failed a)")
	}

	if dep, ok := depFailed["b"]; !ok || dep != "a" {
		t.Fatalf("onDepFailed for b: got (%q, %v), want (\"a\", true)", dep, ok)
	}
	if _, ok := depFailed["c"]; !ok {
		t.Fatalf("onDepFailed should have been called for c (cascade from b)")
	}
}

// TestWalkDAG_DepFailureDoesNotAffectIndependent: an independent task
// with no dependency on the failed node should still run.
//
// DAG: a -> b, c (independent)
func TestWalkDAG_DepFailureDoesNotAffectIndependent(t *testing.T) {
	deps := map[string][]string{
		"a": {},
		"b": {"a"},
		"c": {},
	}

	var mu sync.Mutex
	called := map[string]bool{}
	depFailed := map[string]string{}

	err := walkDAG(deps, func(name string) error {
		mu.Lock()
		called[name] = true
		mu.Unlock()
		if name == "a" {
			return errors.New("a failed")
		}
		return nil
	}, nil, func(node, failedDep string) {
		mu.Lock()
		depFailed[node] = failedDep
		mu.Unlock()
	})

	if err == nil {
		t.Fatalf("expected walkDAG to return an error, got nil")
	}

	if !called["a"] {
		t.Fatalf("fn(a) should have been called")
	}
	if called["b"] {
		t.Fatalf("fn(b) should NOT have been called (depends on failed a)")
	}
	if !called["c"] {
		t.Fatalf("fn(c) SHOULD have been called (independent of a)")
	}

	if dep, ok := depFailed["b"]; !ok || dep != "a" {
		t.Fatalf("onDepFailed for b: got (%q, %v), want (\"a\", true)", dep, ok)
	}
	if _, ok := depFailed["c"]; ok {
		t.Fatalf("onDepFailed should NOT have been called for c (independent)")
	}
}

// TestWalkDAG_ContinueOnFailureOverride: a dependent with
// continueOnFailure=true should still be launched even when its
// upstream dependency failed.
//
// DAG: a -> b (continueOnFailure)
func TestWalkDAG_ContinueOnFailureOverride(t *testing.T) {
	deps := map[string][]string{
		"a": {},
		"b": {"a"},
	}

	continueOnFailure := map[string]bool{
		"b": true,
	}

	var mu sync.Mutex
	called := map[string]bool{}
	depFailed := map[string]string{}

	err := walkDAG(deps, func(name string) error {
		mu.Lock()
		called[name] = true
		mu.Unlock()
		if name == "a" {
			return errors.New("a failed")
		}
		return nil
	}, continueOnFailure, func(node, failedDep string) {
		mu.Lock()
		depFailed[node] = failedDep
		mu.Unlock()
	})

	if err == nil {
		t.Fatalf("expected walkDAG to return an error from a, got nil")
	}

	if !called["a"] {
		t.Fatalf("fn(a) should have been called")
	}
	if !called["b"] {
		t.Fatalf("fn(b) SHOULD have been called (continueOnFailure is true)")
	}
	if _, ok := depFailed["b"]; ok {
		t.Fatalf("onDepFailed should NOT have been called for b (continueOnFailure)")
	}
}

// TestWalkDAG_DiamondWithFailure: in a diamond DAG where the root fails,
// all downstream nodes should be skipped.
//
// DAG:
//
//	a -> b, a -> c, b -> d, c -> d
func TestWalkDAG_DiamondWithFailure(t *testing.T) {
	deps := map[string][]string{
		"a": {},
		"b": {"a"},
		"c": {"a"},
		"d": {"b", "c"},
	}

	var mu sync.Mutex
	called := map[string]bool{}
	depFailed := map[string]string{}

	err := walkDAG(deps, func(name string) error {
		mu.Lock()
		called[name] = true
		mu.Unlock()
		if name == "a" {
			return errors.New("a failed")
		}
		return nil
	}, nil, func(node, failedDep string) {
		mu.Lock()
		depFailed[node] = failedDep
		mu.Unlock()
	})

	if err == nil {
		t.Fatalf("expected walkDAG to return an error, got nil")
	}

	if !called["a"] {
		t.Fatalf("fn(a) should have been called")
	}
	if called["b"] {
		t.Fatalf("fn(b) should NOT have been called")
	}
	if called["c"] {
		t.Fatalf("fn(c) should NOT have been called")
	}
	if called["d"] {
		t.Fatalf("fn(d) should NOT have been called")
	}

	if _, ok := depFailed["b"]; !ok {
		t.Fatalf("onDepFailed should have been called for b")
	}
	if _, ok := depFailed["c"]; !ok {
		t.Fatalf("onDepFailed should have been called for c")
	}
	if _, ok := depFailed["d"]; !ok {
		t.Fatalf("onDepFailed should have been called for d (cascade)")
	}
}
