package cronicle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	cron "github.com/robfig/cron/v3"
)

// TestLoadCron_MalformedHCLDoesNotPanic exercises the heartbeat
// reload path with a file that's been corrupted on disk. Before this
// fix, GetConfig returned (nil, err); LoadCron logged the error but
// fell through to conf.Hcl(), dereffing nil and panicking. The
// scheduler is supposed to keep the previous good config in place
// until the operator fixes the file.
func TestLoadCron_MalformedHCLDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	hclPath := filepath.Join(dir, "cronicle.hcl")
	good := []byte(`
heartbeat = "@every 60s"
schedule "ok" {
  cron = "@every 1h"
  task "say" {
    command = ["/bin/echo", "hi"]
    depends = null
    env     = null
  }
}
`)
	if err := os.WriteFile(hclPath, good, 0o644); err != nil {
		t.Fatalf("write good: %v", err)
	}

	// Seed confPriorGlobal as the producer's Run() does, so LoadCron has
	// a previous-config snapshot to compare against.
	conf, err := GetConfig(hclPath)
	if err != nil {
		t.Fatalf("GetConfig good: %v", err)
	}
	confPriorGlobal = conf
	defer func() { confPriorGlobal = nil }()

	c := cron.New()
	queue := make(chan []byte, 1)
	defer close(queue)

	// Force the initial load (analogous to startup).
	LoadCron(hclPath, c, queue, true)
	if got := len(c.Entries()); got == 0 {
		t.Fatalf("expected entries after good load, got 0")
	}
	priorEntryCount := len(c.Entries())

	// Now corrupt the file. A truncated block with an unclosed `{` makes
	// hcl's parser error out hard.
	if err := os.WriteFile(hclPath, []byte(`schedule "broken" { cron = "@every 1s"`), 0o644); err != nil {
		t.Fatalf("write bad: %v", err)
	}

	// LoadCron used to panic here. Now it should log and return,
	// leaving confPriorGlobal and the cron entries intact.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("LoadCron panicked on malformed HCL: %v", r)
		}
	}()
	LoadCron(hclPath, c, queue, false)

	// Previous good config is still authoritative.
	if confPriorGlobal == nil {
		t.Fatalf("confPriorGlobal cleared on bad reload — should be preserved")
	}
	if !strings.Contains(string(confPriorGlobal.Hcl().Bytes), `"ok"`) {
		t.Fatalf("confPriorGlobal mutated to broken state: %s",
			confPriorGlobal.Hcl().Bytes)
	}
	if got := len(c.Entries()); got != priorEntryCount {
		t.Fatalf("cron entries mutated: got %d, want %d", got, priorEntryCount)
	}
}
