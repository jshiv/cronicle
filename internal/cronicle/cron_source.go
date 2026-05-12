package cronicle

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hashicorp/hcl/v2/hclparse"
	cron "github.com/robfig/cron/v3"

	"github.com/jshiv/cronicle/internal/cronicle/configsource"
)

// StartCronFromSource is the source-aware counterpart of StartCron. It
// performs the initial Fetch synchronously (so a missing source fails
// startup, not the first refresh tick), then registers the heartbeat
// and config_refresh cron entries.
//
// On every refresh tick the source is consulted via etag; only changed
// content reaches the HCL parser, so a sub-second refresh interval is
// cheap.
//
// Cancelling ctx stops the scheduler and (best-effort) closes any
// sources implementing io.Closer.
func StartCronFromSource(ctx context.Context, src configsource.Source, queue chan<- []byte) error {
	if src == nil {
		return fmt.Errorf("StartCronFromSource: source is nil")
	}
	slog.Info("Starting Scheduler...", "cronicle", "start", "source", src.String())

	state := newReloadState(src)
	conf, err := state.fetchAndParse(ctx, true)
	if err != nil {
		return fmt.Errorf("initial config fetch: %w", err)
	}
	if conf == nil {
		return fmt.Errorf("initial config fetch returned nil")
	}

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
		heartbeat = "@every 30s"
	}
	refresh := conf.ConfigRefresh
	if refresh == "" {
		refresh = "@every 1s"
	}

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

	// Force the initial schedule registration. We already have the parsed
	// conf in hand from the startup fetch, so this is cheap.
	state.loadInto(ctx, c, queue, true)

	// Best-effort cleanup on ctx cancel: closes any source that holds
	// background resources (FileSource's fsnotify watcher,
	// PostgresSource's pool).
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
