package cronicle

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	cron "github.com/robfig/cron/v3"
)

// TestRuntimeState_NoRaceUnderConcurrentLoad drives the same access
// pattern that previously raced: Run()'s LoadCron goroutine writes
// (conf, lastRawHCL) on every refresh tick while another goroutine
// (today: the HTTP listener's confSrc closure; here: a tight loop)
// reads conf. Before the C1 fix, both touched the package-level vars
// without synchronization. The test is meant to be run with -race.
func TestRuntimeState_NoRaceUnderConcurrentLoad(t *testing.T) {
	SetupLogging(LogFormatText)
	defer resetRuntimeStateForTest()

	dir := t.TempDir()
	hcl := filepath.Join(dir, "cronicle.hcl")
	// 200ms refresh so LoadCron writes happen frequently during the test
	// window. Long cron / heartbeat so neither fires.
	initial := `config_refresh = "@every 200ms"
heartbeat      = "@every 1h"
schedule "s" {
  cron = "@every 1h"
  task "t" { command = ["true"] }
}
`
	if err := os.WriteFile(hcl, []byte(initial), 0o644); err != nil {
		t.Fatalf("write hcl: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Producer goroutine: Run() — spawns StartCron + listener + queue.
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		Run(ctx, hcl, RunOptions{})
	}()

	// Give the scheduler a moment to spin up and complete the initial
	// force-load before the readers start hammering snapshotConf.
	time.Sleep(150 * time.Millisecond)

	var wg sync.WaitGroup
	wg.Add(2)

	// Reader goroutine: tight loop hitting the same code path the HTTP
	// listener's confSrc closure runs on every /v1/schedules call.
	go func() {
		defer wg.Done()
		for i := 0; i < 5000; i++ {
			_ = globalRuntime.snapshotConf()
			time.Sleep(100 * time.Microsecond)
		}
	}()

	// File-mutator goroutine: rewrite the HCL with a different schedule
	// name every 50ms. Forces LoadCron's refresh tick to actually re-
	// register entries and write (conf, lastRawHCL).
	go func() {
		defer wg.Done()
		for i := 0; i < 30; i++ {
			body := fmt.Sprintf(`config_refresh = "@every 200ms"
heartbeat      = "@every 1h"
schedule "s%d" {
  cron = "@every 1h"
  task "t" { command = ["true"] }
}
`, i)
			_ = os.WriteFile(hcl, []byte(body), 0o644)
			time.Sleep(50 * time.Millisecond)
		}
	}()

	wg.Wait()
	cancel()
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s of cancel")
	}
}

// TestStartCron_StaticEntriesSurviveImmediateRefreshTick verifies the
// startup ordering fix. Before the fix, c.Start() happened BEFORE
// staticEntryIDs was populated, so a refresh tick firing in the gap
// (very plausible with @every 1s and certain with the @every 100ms
// used here) would observe an empty static set and strip the
// heartbeat + refresh entries it was registered against. After the
// fix, the static-set map is populated before c.Start() runs.
func TestStartCron_StaticEntriesSurviveImmediateRefreshTick(t *testing.T) {
	SetupLogging(LogFormatText)
	defer resetRuntimeStateForTest()

	dir := t.TempDir()
	hcl := filepath.Join(dir, "cronicle.hcl")
	body := `config_refresh = "@every 100ms"
heartbeat      = "@every 1h"
schedule "s" {
  cron = "@every 1h"
  task "t" { command = ["true"] }
}
`
	if err := os.WriteFile(hcl, []byte(body), 0o644); err != nil {
		t.Fatalf("write hcl: %v", err)
	}

	// Construct + start the cron in-process so we can poke c.Entries()
	// directly. StartCron's contract: after it returns, the cron is
	// running with heartbeat + config_refresh + per-schedule entries.
	c := cron.New()
	conf, err := GetConfig(hcl)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	globalRuntime.storeConf(conf)
	hbID, _ := c.AddFunc(conf.Heartbeat, func() {})
	refreshID, _ := c.AddFunc(conf.ConfigRefresh, func() { LoadCron(hcl, c, nil, false) })
	globalRuntime.setStaticEntries(map[cron.EntryID]bool{hbID: true, refreshID: true})
	LoadCron(hcl, c, nil, true)
	c.Start()
	defer c.Stop()

	// Let the refresh tick fire at least twice. Each tick walks
	// c.Entries() and removes anything NOT in staticEntryIDs. If the
	// static-set was empty when the tick fired, both static entries are
	// gone after the first tick.
	time.Sleep(350 * time.Millisecond)

	// Expect both static IDs to still be registered plus one dynamic
	// per-schedule entry (the "s" schedule).
	wantStatic := map[cron.EntryID]bool{hbID: true, refreshID: true}
	staticFound := 0
	for _, e := range c.Entries() {
		if wantStatic[e.ID] {
			staticFound++
		}
	}
	if staticFound != 2 {
		t.Fatalf("static entries stripped: found %d of 2 (entries: %v)", staticFound, entryIDs(c))
	}
}

func entryIDs(c *cron.Cron) []cron.EntryID {
	ids := make([]cron.EntryID, 0, len(c.Entries()))
	for _, e := range c.Entries() {
		ids = append(ids, e.ID)
	}
	return ids
}
