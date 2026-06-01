package cronicle

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2/hclparse"

	"github.com/jshiv/cronicle/internal/cronicle/state"
)

// TestLastRun_HCLToExec_Integration is the full-stack guard for ${last_run}:
// parse it from a real HCL command, resolve it at dispatch from the state
// store (the producer's job), execute, and check the value the task
// actually received. It ties together every layer that has had a bug:
//
//   - HCL eval context registration (#133 — without it, parse fails here)
//   - dispatch-time resolution from the authoritative store (#127)
//   - exec-time substitution of task.LastRun (#126)
//
// First run (no prior success) -> empty (full backfill). After a prior
// successful run exists -> that run's start time.
func TestLastRun_HCLToExec_Integration(t *testing.T) {
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	out := filepath.Join(t.TempDir(), "lr.txt")
	hcl := fmt.Sprintf(`schedule "lr" {
  cron = "@once"
  task "show" {
    command = ["bash", "-c", "printf %%s ${last_run} > %s"]
  }
}`, out)

	conf, diags := ParseBytes([]byte(hcl), "cronicle.hcl", hclparse.NewParser())
	if diags.HasErrors() {
		t.Fatalf("HCL parse failed (is ${last_run} registered in the eval context?): %s", diags.Error())
	}
	sch := conf.Schedules[0]

	// First fire: no prior successful run -> ${last_run} empty.
	sch.RunID = "R-first"
	resolveLastRun(store, &sch)
	sch.ExecuteTasks()
	if got := readFile(t, out); got != "" {
		t.Errorf("first run: ${last_run} = %q, want empty (full backfill)", got)
	}

	// Record a prior successful run, fire again: ${last_run} == its start.
	prior := time.Now().Add(-26 * time.Hour).Truncate(time.Second)
	seedRun(t, store, "R-prior", "lr", "succeeded", prior)
	sch.RunID = "R-second"
	resolveLastRun(store, &sch)
	sch.ExecuteTasks()
	if got := readFile(t, out); got != prior.UTC().Format(time.RFC3339) {
		t.Errorf("second run: ${last_run} = %q, want prior run start %q", got, prior.UTC().Format(time.RFC3339))
	}
}
