package cronicle

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/hcl/v2/hclparse"
	cron "github.com/robfig/cron/v3"

	"github.com/jshiv/cronicle/internal/cronicle/configsource"
)

// RunFromSource is the Source-aware counterpart of Run. Used by
// `cronicle run --config-source <spec>` to load the config from a
// non-file backing store (http / s3 / postgres) while keeping every
// other Run-time concern intact: file logging, the state-plane DB,
// the listener + queue + worker plumbing.
//
// workdir anchors the runner's filesystem artifacts (.cronicle/state.db,
// .cronicle/log/cronicle.jsonl, scratch dirs). File-based runs derived
// it from the HCL's parent directory; non-file sources have no inherent
// workdir, so the caller supplies one. Defaults to cwd when empty.
//
// Returns when ctx is canceled or the listener errors. The blocking
// behavior matches Run().
func RunFromSource(ctx context.Context, spec, workdir string, runOptions RunOptions) error {
	if spec == "" {
		return fmt.Errorf("RunFromSource: spec is empty")
	}
	if workdir == "" {
		workdir = "."
	}
	workdirAbs, err := filepath.Abs(workdir)
	if err != nil {
		return fmt.Errorf("workdir abspath: %w", err)
	}
	if err := os.MkdirAll(workdirAbs, 0o755); err != nil {
		return fmt.Errorf("workdir mkdir: %w", err)
	}

	src, err := configsource.Open(ctx, spec)
	if err != nil {
		return fmt.Errorf("open config source: %w", err)
	}

	// Initial fetch synchronously so startup fails on a missing or
	// unreachable source rather than silently waiting for the first
	// refresh tick to log a warning.
	body, etag, _, err := src.Fetch(ctx, "")
	if err != nil {
		return fmt.Errorf("initial fetch from %s: %w", src.String(), err)
	}
	parser := hclparse.NewParser()
	conf, diags := ParseBytes(body, "cronicle.hcl", parser)
	if diags.HasErrors() {
		return fmt.Errorf("parse initial config: %s", diags.Error())
	}
	if conf == nil {
		return fmt.Errorf("parse returned nil config")
	}
	// Seed confPriorGlobal so the first refresh tick sees identical
	// bytes and skips a redundant reload pass.
	confPriorGlobal = conf

	taskCount := 0
	for _, s := range conf.Schedules {
		taskCount += len(s.Tasks)
	}
	slog.Info("config loaded",
		"source", src.String(),
		"workdir", workdirAbs,
		"schedules", len(conf.Schedules),
		"tasks", taskCount,
	)

	if runOptions.LogToFile {
		if err := EnableFileLog(workdirAbs); err != nil {
			return fmt.Errorf("enable file log: %w", err)
		}
	}

	// Same state-plane setup as Run(): .cronicle/state.db lives under
	// the workdir.
	stateDir := filepath.Join(workdirAbs, ".cronicle")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		slog.Warn("state dir mkdir failed; projection disabled", "error", err.Error())
	} else if err := EnableStateStore(filepath.Join(stateDir, "state.db")); err != nil {
		slog.Warn("state store open failed; projection disabled", "error", err.Error())
	}

	if runOptions.PostStateStoreHook != nil {
		if err := runOptions.PostStateStoreHook(); err != nil {
			return fmt.Errorf("post-state-store hook: %w", err)
		}
	}

	var wg sync.WaitGroup
	var (
		triggerQueue chan<- []byte
		closeQueue   func()
	)
	if runOptions.ListenAddr != "" && runOptions.ListenToken != "" && stateStore != nil {
		enqueueChan := make(chan []byte, 64)
		triggerQueue = enqueueChan
		closeQueue = func() { close(enqueueChan) }
		go enqueueAdapter(enqueueChan, stateStore)
		if err := startSchedulerFromSource(ctx, src, conf, etag, enqueueChan); err != nil {
			return fmt.Errorf("scheduler start: %w", err)
		}
		if runOptions.RunWorker {
			selfWorkerPool(ctx, stateStore, workdirAbs, runOptions.WorkerCount, &wg)
		}
		go reaperLoop(ctx, stateStore)
	} else {
		queue := make(chan []byte)
		triggerQueue = queue
		closeQueue = func() { close(queue) }
		if err := startSchedulerFromSource(ctx, src, conf, etag, queue); err != nil {
			return fmt.Errorf("scheduler start: %w", err)
		}
		go ConsumeSchedule(queue, workdirAbs, &wg)
	}

	listenerDone := startListener(ctx, runOptions.ListenAddr, runOptions.ListenToken, triggerQueue)

	// Same shutdown sequence as Run(): listener drains, trigger queue
	// closes (consumers exit), wait up to 30s for in-flight tasks, then
	// close the state store. See Run() for rationale.
	<-ctx.Done()
	slog.Info("shutdown: ctx canceled, draining")

	<-listenerDone
	closeQueue()

	drained := make(chan struct{})
	go func() {
		wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(30 * time.Second):
		slog.Warn("shutdown: in-flight tasks did not finish within 30s, exiting anyway")
	}

	if err := CloseStateStore(); err != nil {
		slog.Warn("shutdown: state store close failed", "error", err.Error())
	}
	slog.Info("shutdown: complete")
	return nil
}

// startSchedulerFromSource is the inner helper shared by RunFromSource:
// register heartbeat + config_refresh cron entries, seed the reload
// state with the pre-fetched conf so the first tick is a true diff
// rather than a redundant parse.
func startSchedulerFromSource(ctx context.Context, src configsource.Source, conf *Config, etag string, queue chan<- []byte) error {
	loc, err := pickLocation(conf.Timezone)
	if err != nil {
		return err
	}
	ApplyTimezone(loc)
	logSchedules(conf)

	c := cron.New(cron.WithLocation(loc))
	c.Start()

	heartbeat := conf.Heartbeat
	if heartbeat == "" {
		heartbeat = "@every 5m"
	}
	refresh := conf.ConfigRefresh
	if refresh == "" {
		refresh = "@every 1s"
	}

	state := newReloadState(src)
	// Seed the reload state with the already-fetched conf so the first
	// refresh tick sees `changed=false` and we don't re-parse.
	state.mu.Lock()
	state.lastEtag = etag
	state.lastConf = conf
	state.mu.Unlock()

	hbID, err := c.AddFunc(heartbeat, func() {
		slog.Info("heartbeat",
			"cronicle", "alive",
			"source", src.String(),
			"schedules", state.scheduleCount(),
		)
	})
	if err != nil {
		return fmt.Errorf("register heartbeat (%q): %w", heartbeat, err)
	}
	refreshID, err := c.AddFunc(refresh, func() { state.loadInto(ctx, c, queue, false) })
	if err != nil {
		return fmt.Errorf("register config refresh (%q): %w", refresh, err)
	}
	state.static = map[cron.EntryID]bool{hbID: true, refreshID: true}

	// Initial dynamic-schedule registration. Force=true so the static
	// confPriorGlobal == conf check doesn't short-circuit.
	state.loadInto(ctx, c, queue, true)

	go func() {
		<-ctx.Done()
		c.Stop()
		if closer, ok := src.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}()
	return nil
}

// reloadState holds the per-runner state for source-driven reloads:
// the source itself, the last etag, the last parsed config, and the
// set of static cron entries that must survive a re-register pass.
//
// All access is serialized through the cron loop (single goroutine),
// but a mutex guards the fields for safe inspection from heartbeat
// callbacks that fire concurrently.
type reloadState struct {
	src       configsource.Source
	mu        sync.Mutex
	lastEtag  string
	lastConf  *Config
	static    map[cron.EntryID]bool
	lastError error
}

func newReloadState(src configsource.Source) *reloadState {
	return &reloadState{src: src, static: map[cron.EntryID]bool{}}
}

func (s *reloadState) scheduleCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastConf == nil {
		return 0
	}
	return len(s.lastConf.Schedules)
}

// fetchAndParse pulls from the source, parses on change, and updates
// the state's lastEtag/lastConf. Returns the current config (which may
// be the cached one when changed=false). Errors are surfaced; callers
// decide whether to abort or warn-and-keep-previous.
func (s *reloadState) fetchAndParse(ctx context.Context, initial bool) (*Config, error) {
	s.mu.Lock()
	prev := s.lastEtag
	cached := s.lastConf
	s.mu.Unlock()

	body, etag, changed, err := s.src.Fetch(ctx, prev)
	if err != nil {
		return cached, err
	}
	if !changed {
		return cached, nil
	}
	parser := hclparse.NewParser()
	conf, diags := ParseBytes(body, "cronicle.hcl", parser)
	if diags.HasErrors() {
		return cached, fmt.Errorf("parse: %s", diags.Error())
	}

	// On the very first fetch we don't have a directory context to
	// run conf.Init against (the file-based path passes the file's
	// dir). Sources have no inherent directory; pass an empty string
	// and rely on conf.Init to no-op repos that need a workdir.
	if initial {
		_ = conf.Init("")
	}

	s.mu.Lock()
	s.lastEtag = etag
	s.lastConf = conf
	s.mu.Unlock()
	return conf, nil
}

// loadInto runs on every refresh tick. Pulls fresh bytes; if changed,
// stops the cron, removes dynamic entries, re-registers from the new
// schedules, restarts.
func (s *reloadState) loadInto(ctx context.Context, c *cron.Cron, queue chan<- []byte, force bool) {
	fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	conf, err := s.fetchAndParse(fetchCtx, false)
	if err != nil {
		slog.Warn("config refresh failed; keeping previous schedules in place",
			"source", s.src.String(), "err", err.Error())
		s.mu.Lock()
		s.lastError = err
		s.mu.Unlock()
		return
	}
	if conf == nil {
		return // no cache yet and fetch returned no body
	}

	// Diff at the rendered-HCL level. Identical bytes → no-op.
	if !force {
		s.mu.Lock()
		isSame := confPriorGlobal != nil && string(confPriorGlobal.Hcl().Bytes) == string(conf.Hcl().Bytes)
		s.mu.Unlock()
		if isSame {
			return
		}
	}

	slog.Info("Refreshing config...", "source", s.src.String(),
		"schedules", len(conf.Schedules))

	c.Stop()
	for _, entry := range c.Entries() {
		if s.static[entry.ID] {
			continue
		}
		c.Remove(entry.ID)
	}
	for _, schedule := range conf.Schedules {
		switch {
		case schedule.Cron == "@once":
			slog.Info("@once execution complete at 'cronicle run'",
				"schedule", schedule.Name, "cron", schedule.Cron)
		case schedule.Cron == "":
			slog.Warn("Skip execution. Use 'cronicle exec' to run.",
				"schedule", schedule.Name, "cron", schedule.Cron)
		default:
			if _, err := c.AddFunc(schedule.Cron, ProduceSchedule(schedule, queue)); err != nil {
				slog.Error("schedule cron format error",
					"schedule", schedule.Name, "cron", schedule.Cron, "err", err.Error())
			}
		}
	}
	c.Start()
	confPriorGlobal = conf
}

func pickLocation(tz string) (*time.Location, error) {
	if tz == "" {
		return time.Local, nil
	}
	return time.LoadLocation(tz)
}

func logSchedules(conf *Config) {
	for _, schedule := range conf.Schedules {
		switch {
		case schedule.Cron == "@once":
			slog.Info("Executing @Once",
				"schedule", schedule.Name, "cron", schedule.Cron)
		case schedule.Cron == "":
			slog.Info("Skip execution. Use 'cronicle exec' to run.",
				"schedule", schedule.Name, "cron", schedule.Cron)
		default:
			slog.Info("Starting cron...",
				"schedule", schedule.Name, "cron", schedule.Cron)
		}
	}
}
