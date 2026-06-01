package cronicle_test

import (
	"testing"

	"github.com/hashicorp/hcl/v2/hclparse"

	"github.com/jshiv/cronicle/internal/cronicle"
)

// commandTokens is every template token the exec-time replacer (exec.go)
// substitutes into a task command. Each MUST also be registered in the HCL
// eval context (parse.go CommandEvalContext) or it can't be used from a
// .hcl file at all — HCL rejects the unknown variable and the whole config
// fails to parse.
//
// >> When you add a token to exec.go's strings.NewReplacer, add it here. <<
var commandTokens = []string{
	"date", "datetime", "timestamp",
	"path", "scratch",
	"last_run", "last_run_epoch",
}

// TestParse_CommandTokensSurviveHCL is the regression guard for the class
// of bug shipped in the original ${last_run} feature: a token wired into
// the exec replacer but NOT the HCL eval context, so it worked from
// Go-built commands (every existing test) yet failed to parse from a real
// .hcl file. For each token, parse an HCL command using it and assert the
// config parses and the token survives literally for the exec replacer.
func TestParse_CommandTokensSurviveHCL(t *testing.T) {
	for _, name := range commandTokens {
		tok := "${" + name + "}"
		t.Run(name, func(t *testing.T) {
			src := []byte(`schedule "s" {
  cron = "@once"
  task "t" {
    command = ["echo", "X=` + tok + `"]
  }
}`)
			conf, diags := cronicle.ParseBytes(src, "cronicle.hcl", hclparse.NewParser())
			if diags.HasErrors() {
				t.Fatalf("HCL parse failed for %s: %s", tok, diags.Error())
			}
			if conf == nil || len(conf.Schedules) != 1 || len(conf.Schedules[0].Tasks) != 1 {
				t.Fatalf("unexpected parse result for %s: %+v", tok, conf)
			}
			got := conf.Schedules[0].Tasks[0].Command
			if len(got) != 2 || got[1] != "X="+tok {
				t.Errorf("%s not preserved through HCL: command = %v, want [echo X=%s]", tok, got, tok)
			}
		})
	}
}

// TestCommandEvalContext_HasEveryCommandToken is the cheap structural twin
// of the round-trip test: every command token must be a variable in the
// HCL eval context. Catches a token removed/renamed/missed (the #133 bug)
// without needing to parse anything.
func TestCommandEvalContext_HasEveryCommandToken(t *testing.T) {
	for _, name := range commandTokens {
		if _, ok := cronicle.CommandEvalContext.Variables[name]; !ok {
			t.Errorf("HCL eval context missing %q — ${%s} would fail to parse from a .hcl file", name, name)
		}
	}
}
