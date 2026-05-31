package cronicle

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// writeLastRunTask returns a task that records the substituted ${last_run}
// token to outFile, so a test can assert what value ExecuteTasks fed the
// task without scraping logs.
func writeLastRunTask(outFile string) Task {
	return Task{
		Name:    "t1",
		Command: []string{"bash", "-c", `printf '%s' "${last_run}" > ` + outFile},
	}
}

// withStaleStore points the process-wide StateStore at store for the
// duration of the test, restoring the prior store afterward. Simulates a
// worker whose local store is NOT the authoritative one the dispatcher used.
func withStaleStore(t *testing.T, store state.Backend) {
	t.Helper()
	prev := StateStore()
	SetStateStore(store)
	t.Cleanup(func() { SetStateStore(prev) })
}

// TestExecuteTasks_NeverReadsStateForLastRun is the worker-side half of the
// ${last_run} contract: ExecuteTasks must use the baked Task.LastRun as-is
// and NEVER read StateStore() itself. This is the exact regression that
// shipped in #126 — on a distributed worker StateStore() is a
// non-authoritative in-memory projection with no run history, so reading it
// would clobber the dispatcher-resolved value with an empty/wrong one.
func TestExecuteTasks_NeverReadsStateForLastRun(t *testing.T) {
	// A store holding a DIFFERENT "succeeded" run that ExecuteTasks must
	// ignore: the value was already resolved upstream and baked into the
	// task. (Stands in for a worker's non-authoritative local store.)
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	staleStoreTime := time.Now().Add(-99 * time.Hour).Truncate(time.Second)
	seedRun(t, store, "RX", "daily", "succeeded", staleStoreTime)
	withStaleStore(t, store)

	out := filepath.Join(t.TempDir(), "last_run.txt")
	baked := time.Now().Add(-26 * time.Hour).Truncate(time.Second)

	task := writeLastRunTask(out)
	task.LastRun = baked // already resolved by the dispatcher
	sch := Schedule{Name: "daily", RunID: "R2", Tasks: []Task{task}}
	sch.ExecuteTasks()

	got := readFile(t, out)
	want := baked.UTC().Format(time.RFC3339)
	if got != want {
		t.Errorf("${last_run} = %q, want the baked %q (ExecuteTasks read the store: %q)",
			got, want, staleStoreTime.UTC().Format(time.RFC3339))
	}
}

// TestResolveLastRun_FeedsExecuteTasks covers the dispatch side for the
// in-process paths (legacy ConsumeSchedule, foreground exec): resolveLastRun
// reads the authoritative store and stamps Task.LastRun before ExecuteTasks,
// which then substitutes it into ${last_run}.
func TestResolveLastRun_FeedsExecuteTasks(t *testing.T) {
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	prior := time.Now().Add(-26 * time.Hour).Truncate(time.Second)
	seedRun(t, store, "R1", "daily", "succeeded", prior)

	out := filepath.Join(t.TempDir(), "last_run.txt")
	sch := Schedule{Name: "daily", RunID: "R2", Tasks: []Task{writeLastRunTask(out)}}

	resolveLastRun(store, &sch) // dispatch-time resolution
	sch.ExecuteTasks()

	got := readFile(t, out)
	want := prior.UTC().Format(time.RFC3339)
	if got != want {
		t.Errorf("${last_run} = %q, want %q resolved from the authoritative store", got, want)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}
