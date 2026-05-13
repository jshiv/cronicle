package state

import "testing"

// TestStoreSatisfiesBackend is a runtime guard mirroring the
// compile-time `var _ Backend = (*Store)(nil)` assertion in backend.go.
// If Store ever loses a method the interface declares, this test (and
// the package) will fail to compile — but having an explicit test makes
// the contract visible in `go test` output.
func TestStoreSatisfiesBackend(t *testing.T) {
	var b Backend = &Store{}
	if b == nil {
		t.Fatal("Store does not satisfy Backend")
	}
}

// TestBackendCompositionLoop exercises the role-interface composition.
// A consumer that takes only the slice it needs (e.g. ControlReader)
// should accept a *Store directly.
func TestBackendCompositionLoop(t *testing.T) {
	var (
		r  RunReader      = &Store{}
		cr ControlReader  = &Store{}
		cw ControlWriter  = &Store{}
		jq JobQueue       = &Store{}
		wr WorkerRegistry = &Store{}
		ra RetryAPI       = &Store{}
		cc ControlChannel = &Store{}
		es EventSink      = &Store{}
	)
	// Reference each variable so the compiler doesn't complain about
	// unused vars; the assignment above is the test.
	_, _, _, _, _, _, _, _ = r, cr, cw, jq, wr, ra, cc, es
}
