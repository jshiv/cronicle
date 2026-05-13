package cronicle

import (
	"reflect"
	"sort"
	"testing"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

func openState(t *testing.T) (*state.Store, func()) {
	t.Helper()
	s, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	closeFn := func() { _ = s.Close() }
	t.Cleanup(closeFn)
	return s, closeFn
}

func feedEvent(t *testing.T, s *state.Store, line string) {
	t.Helper()
	ev, ok := state.DecodeEvent([]byte(line))
	if !ok {
		t.Fatalf("DecodeEvent rejected: %s", line)
	}
	if err := s.Apply(ev); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

// TestDownstreamTasks verifies the transitive-dependents helper used by
// the task-cancel listener endpoint to compute the cascade set.
//
// DAG:
//	a → b → c → e
//	  ↘ d ↗
//
// Dependents of a: b, d
// Dependents of b: c
// Dependents of c: e
// Dependents of d: c
// Full downstream of a: {b, d, c, e}
func TestDownstreamTasks(t *testing.T) {
	sch := Schedule{
		Tasks: []Task{
			{Name: "a"},
			{Name: "b", Depends: []string{"a"}},
			{Name: "d", Depends: []string{"a"}},
			{Name: "c", Depends: []string{"b", "d"}},
			{Name: "e", Depends: []string{"c"}},
		},
	}

	tests := []struct {
		root string
		want []string
	}{
		{"a", []string{"b", "c", "d", "e"}},
		{"b", []string{"c", "e"}},
		{"c", []string{"e"}},
		{"d", []string{"c", "e"}},
		{"e", nil},
		{"ghost", nil},
	}
	for _, tt := range tests {
		got := sch.downstreamTasks(tt.root)
		// downstreamTasks returns BFS order; sort for stable compare.
		sort.Strings(got)
		want := append([]string(nil), tt.want...)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("downstreamTasks(%q) = %v, want %v", tt.root, got, want)
		}
	}
}

// TestDAG_CancelGate_BlocksLaunch: a task flagged canceled in the
// projection must not execute when the walker reaches it.
func TestDAG_CancelGate_BlocksLaunch(t *testing.T) {
	store, _ := openState(t)
	prev := stateStore
	stateStore = store
	t.Cleanup(func() { stateStore = prev })

	// Seed a run + task row in canceled state.
	feedEvent(t, store, `{"time":"2026-05-12T12:00:00Z","entry_type":"schedule_start","run_id":"R_C","schedule":"daily","tasks":["a"]}`)
	if _, err := store.CancelTask("R_C", "a", nil); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}

	if !taskCanceledInRun("R_C", "a") {
		t.Fatalf("expected gate to report canceled")
	}
	if taskCanceledInRun("R_C", "b") {
		t.Fatalf("untouched task should not be flagged canceled")
	}
}
