package cronicle_test

import (
	"testing"

	"github.com/hashicorp/hcl/v2/hclparse"

	"github.com/jshiv/cronicle/internal/cronicle"
)

// TestParse_LastRunTokenSurvivesHCL guards that ${last_run} /
// ${last_run_epoch} are usable from an actual .hcl file. They must be
// registered in the HCL eval context so they survive parse as their
// literal tokens (for the exec-time replacer to substitute at dispatch).
// Without that registration HCL rejects "${last_run}" as an unknown
// variable and the whole config fails to parse — even though the
// exec-time replacer knows the token. (Regression: shipped that way in
// the original ${last_run} feature; no test parsed it from HCL.)
func TestParse_LastRunTokenSurvivesHCL(t *testing.T) {
	src := []byte(`
schedule "lr" {
  cron = "@every 3s"
  task "show" {
    command = ["bash", "-c", "echo since=${last_run} epoch=${last_run_epoch}"]
  }
}
`)
	conf, diags := cronicle.ParseBytes(src, "cronicle.hcl", hclparse.NewParser())
	if diags.HasErrors() {
		t.Fatalf("HCL parse errored on ${last_run}: %s", diags.Error())
	}
	if conf == nil || len(conf.Schedules) != 1 || len(conf.Schedules[0].Tasks) != 1 {
		t.Fatalf("unexpected parse result: %+v", conf)
	}
	got := conf.Schedules[0].Tasks[0].Command
	want := "echo since=${last_run} epoch=${last_run_epoch}"
	if len(got) != 3 || got[2] != want {
		t.Errorf("command = %v\n want token preserved literally: %q", got, want)
	}
}
