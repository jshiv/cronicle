package cronicle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	cron "github.com/robfig/cron/v3"
)

// Run is the main function of the cron package.
//
// Returns when ctx is canceled. On cancel: the HTTP listener stops
// accepting new connections and drains in-flight handlers (30s grace),
// the cron loop stops firing new ticks, the trigger queue is closed so
// the consumer goroutines see EOF, then we wait up to 30s for in-flight
// task goroutines to finish before closing the state store. context.Background()
// is acceptable for tests / direct callers that don't need shutdown.
func Run(ctx context.Context, cronicleFile string, runOptions RunOptions) {

	cronicleFileAbs, err := filepath.Abs(cronicleFile)
	if err != nil {
		Fatal(err)
	}

	if !fileExists(cronicleFileAbs) {
		Fatal("file does not exist", "path", cronicleFileAbs)
	}
	croniclePath := filepath.Dir(cronicleFileAbs)

	conf, err := GetConfig(cronicleFileAbs)
	if err != nil {
		Fatal(err)
	}
	confPriorGlobal = conf

	taskCount := 0
	for _, s := range conf.Schedules {
		taskCount += len(s.Tasks)
	}
	slog.Info("config loaded",
		"path", cronicleFileAbs,
		"schedules", len(conf.Schedules),
		"tasks", taskCount,
	)

	if runOptions.LogToFile {
		if err := EnableFileLog(croniclePath); err != nil {
			Fatal(err)
		}
	}

	// State plane: SQLite-backed projection of slog events. Lives at
	// .cronicle/state.db so the lifetime matches the schedule directory.
	// Currently the store is built into the producer; once Phase 2 lands
	// the listener and the API will share this same handle.
	stateDir := filepath.Join(croniclePath, ".cronicle")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		slog.Warn("state dir mkdir failed; projection disabled", "error", err.Error())
	} else if err := EnableStateStore(filepath.Join(stateDir, "state.db")); err != nil {
		slog.Warn("state store open failed; projection disabled", "error", err.Error())
	}

	if runOptions.PostStateStoreHook != nil {
		if err := runOptions.PostStateStoreHook(); err != nil {
			Fatal(err)
		}
	}

	var wg sync.WaitGroup
	// triggerQueue is the same send-side queue StartCron pushes to on each
	// cron tick. The listener (if enabled) writes to it for remote-trigger
	// fires, so triggered and cron-fired runs are identical from the
	// consumer's perspective — same JSON, same DAG walk, same logs.
	//
	// Queue mode is derived, not flagged: when the HTTP listener is
	// configured (`--listen + --listen-token`), the producer runs the
	// SQLite-backed jobs queue so remote workers can long-poll /v1/jobs
	// AND so cancel/retry/resume can persist payloads. When the listener
	// is off, it's a single-process operation — in-memory chan, in-process
	// consumer. Same operator intent: "if I'm exposing HTTP, I need
	// durable queueing; if I'm not, I don't."
	//
	// Hold a reference to the writable end so we can close it during
	// shutdown — closing signals enqueueAdapter / ConsumeSchedule that no
	// more work is coming, letting their range loops exit cleanly.
	var (
		triggerQueue chan<- []byte
		closeQueue   func()
	)
	if runOptions.ListenAddr != "" && runOptions.ListenToken != "" && stateStore != nil {
		enqueueChan := make(chan []byte, 64)
		triggerQueue = enqueueChan
		closeQueue = func() { close(enqueueChan) }
		go enqueueAdapter(enqueueChan, stateStore)
		go StartCron(ctx, cronicleFileAbs, enqueueChan)
		if runOptions.RunWorker {
			go selfWorker(ctx, loopbackURL(runOptions.ListenAddr), runOptions.ListenToken, croniclePath, &wg)
		}
		go reaperLoop(ctx, stateStore)
	} else {
		queue := make(chan []byte)
		triggerQueue = queue
		closeQueue = func() { close(queue) }
		go StartCron(ctx, cronicleFileAbs, queue)
		go ConsumeSchedule(queue, croniclePath, &wg)
	}

	listenerDone := startListener(ctx, runOptions.ListenAddr, runOptions.ListenToken, triggerQueue)

	// Block until ctx is canceled, then run the shutdown sequence in
	// order: listener (stops accepting), trigger queue (consumers exit
	// their for-range loops), in-flight task drain (wg, capped at 30s
	// so a stuck task doesn't keep cronicle from exiting), state store.
	<-ctx.Done()
	slog.Info("shutdown: ctx canceled, draining")

	slog.Debug("shutdown: waiting for listener")
	<-listenerDone
	slog.Debug("shutdown: closing trigger queue")
	closeQueue()

	drained := make(chan struct{})
	go func() {
		wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		slog.Debug("shutdown: drained ok")
	case <-time.After(30 * time.Second):
		slog.Warn("shutdown: in-flight tasks did not finish within 30s, exiting anyway")
	}

	slog.Debug("shutdown: closing state store")
	if err := CloseStateStore(); err != nil {
		slog.Warn("shutdown: state store close failed", "error", err.Error())
	}
	slog.Info("shutdown: complete")
}

// RunOptions controls cronicle run's process-level behavior. Queue mode
// is no longer a knob: it's derived from ListenAddr/ListenToken — when
// the HTTP listener is configured, the producer runs the SQLite-backed
// jobs queue so workers can long-poll /v1/jobs and cancel/retry/resume
// have a payload to replay. Otherwise it's an in-memory chan in a
// single process.
type RunOptions struct {
	// RunWorker controls whether the in-process consumer runs alongside
	// the producer. With listener mode it's the self-worker; without
	// listener it's the chan-based ConsumeSchedule. Set to false on
	// dedicated producers so external workers do all execution.
	RunWorker bool
	// LogToFile mirrors structured logs to .cronicle/log/cronicle.jsonl
	// rotated by lumberjack. Independent of stdout.
	LogToFile bool
	// ListenAddr / ListenToken expose the remote-trigger HTTP API. Empty
	// addr disables the listener entirely; non-empty addr REQUIRES a token
	// (the listener refuses to bind otherwise — see internal/cronicle/listen.go).
	ListenAddr  string
	ListenToken string
	// PostStateStoreHook fires after EnableStateStore returns
	// (successfully or not — caller can check StateStore() != nil) and
	// before the cron loop, listener, or queue start. Lets the cmd
	// layer bind the secret-source pump to the state.Backend without
	// requiring cronicle.Run to know about secretsource internals.
	// Errors abort startup.
	PostStateStoreHook func() error
}

//StartCron pushes all schedules in the given config to the cron scheduler
//starts the cron scheduler which publishes the serialzied
//schedules to the message queue for execution.
//
//On ctx cancel the cron loop is stopped; in-flight tick handlers finish
//but no new ticks fire. Caller is responsible for draining the queue
//and any consumer goroutines.
func StartCron(ctx context.Context, cronicleFile string, queue chan<- []byte) {

	conf, err := GetConfig(cronicleFile)
	if err != nil {
		Fatal(err)
	}
	var loc *time.Location
	if conf.Timezone != "" {
		loc, err = time.LoadLocation(conf.Timezone)
		if err != nil {
			Fatal(err)
		}
	} else {
		loc = time.Local
	}

	ApplyTimezone(loc)
	slog.Info("Starting Scheduler...", "cronicle", "start")

	for _, schedule := range conf.Schedules {
		switch {
		case schedule.Cron == "@once":
			slog.Info("Executing @Once", "schedule", schedule.Name, "cron", schedule.Cron)
			ProduceSchedule(schedule, queue)()
		case schedule.Cron == "":
			slog.Info("Skip execution. Use 'cronicle exec' to run.", "schedule", schedule.Name, "cron", schedule.Cron)
		default:
			slog.Info("Starting cron...", "schedule", schedule.Name, "cron", schedule.Cron)
		}
	}

	c := cron.New(cron.WithLocation(loc))
	c.Start()
	if conf.Heartbeat == "" {
		conf.Heartbeat = "@every 5m"
	}
	// Heartbeat is now purely an observability signal — it emits a log
	// line at its cadence so monitors can confirm the runner is alive.
	// HCL reload has moved to the ConfigRefresh ticker below so the two
	// cadences can differ (slow heartbeat for sane logs, fast refresh
	// for snappy edits).
	hbID, _ := c.AddFunc(conf.Heartbeat, func() {
		slog.Info("heartbeat", "cronicle", "alive", "schedules", len(conf.Schedules))
	})
	if conf.ConfigRefresh == "" {
		conf.ConfigRefresh = "@every 1s"
	}
	refreshID, _ := c.AddFunc(conf.ConfigRefresh, func() { LoadCron(cronicleFile, c, queue, false) })
	staticEntryIDs = map[cron.EntryID]bool{hbID: true, refreshID: true}
	LoadCron(cronicleFile, c, queue, true)

	// Shut down the cron loop on ctx cancel. c.Stop() returns a context
	// that signals when all in-flight tick handlers (heartbeat, refresh,
	// per-schedule ProduceSchedule) have returned — but those handlers
	// only push onto the queue, so the wait is short. Run() / caller is
	// responsible for draining the queue and the consumer goroutines.
	go func() {
		<-ctx.Done()
		c.Stop()
	}()
}

// staticEntryIDs tracks the cron entry IDs registered at startup
// (heartbeat + config_refresh). LoadCron uses this set as the "do
// not delete" mask when it re-registers the dynamic schedule entries
// on a config change. Previously this check was a hard-coded
// `entry.ID > 1`, which broke as soon as we added the second static
// entry for decoupled heartbeat / refresh.
var staticEntryIDs = map[cron.EntryID]bool{}

//confPrior stores a gloabal state of the previosly loaded config for diff checking
var confPriorGlobal *Config

// lastRawHCL holds the raw file bytes from the previous successful load.
// Compared byte-for-byte against the current file to detect changes.
// The previous approach used Hcl() round-trip (encode → bytes), which
// is lossy — gohcl drops fields it doesn't model (repo block, comments)
// and normalizes formatting, so two different files could produce
// identical round-trip bytes and the reload would never trigger.
var lastRawHCL []byte

//LoadCron exeutes GetConfig(cronicleFile) to load the current config from file,
//checks the given config against the global confPrior, and if there is a change,
//stops the cron, removes all of the confPrior cron entries and adds the new conf
//schedules to the cron.
func LoadCron(cronicleFile string, c *cron.Cron, queue chan<- []byte, force bool) {

	slog.Info("Loading config...", "cronicle", "heartbeat", "path", cronicleFile)

	rawHCL, err := os.ReadFile(cronicleFile)
	if err != nil {
		slog.Error("config read failed; keeping previous schedules in place",
			"path", cronicleFile, "error", err.Error())
		return
	}

	conf, err := GetConfig(cronicleFile)
	if err != nil {
		slog.Error("config reload failed; keeping previous schedules in place",
			"path", cronicleFile, "error", err.Error())
		return
	}
	if conf == nil {
		slog.Error("config reload returned nil config; keeping previous schedules in place",
			"path", cronicleFile)
		return
	}

	if !bytes.Equal(lastRawHCL, rawHCL) || force {
		slog.Info("Refreshing config...", "cronicle", "heartbeat", "path", cronicleFile)
		c.Stop()
		for _, entry := range c.Entries() {
			// Preserve the heartbeat + config_refresh static entries; only
			// remove the dynamic per-schedule entries so we can re-register
			// them from the new conf.
			if staticEntryIDs[entry.ID] {
				continue
			}
			c.Remove(entry.ID)
		}

		for _, schedule := range conf.Schedules {
			switch {
			case schedule.Cron == "@once":
				slog.Info("@once execution complete at 'cronicle run'", "schedule", schedule.Name, "cron", schedule.Cron)
			case schedule.Cron == "":
				slog.Warn("Skip execution. Use 'cronicle exec' to run.", "schedule", schedule.Name, "cron", schedule.Cron)
			default:
				cronExpr := schedule.Cron
				if schedule.Timezone != "" && !strings.HasPrefix(cronExpr, "CRON_TZ=") && !strings.HasPrefix(cronExpr, "@") {
					cronExpr = "CRON_TZ=" + schedule.Timezone + " " + cronExpr
				}
				_, err := c.AddFunc(cronExpr, ProduceSchedule(schedule, queue))
				if err != nil {
					fmt.Printf("\x1b[31;1m%s\x1b[0m\n", fmt.Sprintf("schedule cron format error: %s", schedule.Name))
					Fatal(err)
				}
			}

		}
		c.Start()
	}
	lastRawHCL = rawHCL
	confPriorGlobal = conf

}

//ConsumeSchedule consumes the byte array of a
//schedule from the message queue for execution
func ConsumeSchedule(queue <-chan []byte, path string, wg *sync.WaitGroup) {
	var p string
	if path == "" {
		p, _ = filepath.Abs("./")
	} else {
		p = path
	}
	for scheduleBytes := range queue {
		wg.Add(1)
		go func(scheduleBytes []byte) {
			defer wg.Done()
			var schedule Schedule
			err := json.Unmarshal(scheduleBytes, &schedule)
			if err != nil {
				slog.Error("schedule unmarshal failed", "error", err.Error())
			}
			schedule.PropigateTaskProperties(p)
			schedule.ExecuteTasks()
		}(scheduleBytes)
	}
}

//ProduceSchedule produces the json of a
//schdule to the message queue for consumption
func ProduceSchedule(schedule Schedule, queue chan<- []byte) func() {
	return func() {
		// Runner-wide drain gate takes precedence: when the runner is
		// drained no schedules tick, regardless of per-schedule state.
		if st := StateStore(); st != nil {
			if drained, err := st.IsDrained(); err != nil {
				slog.Warn("drain check failed; proceeding with schedule",
					"schedule", schedule.Name, "error", err.Error())
			} else if drained {
				slog.Info("Skipping tick: runner drained",
					"entry_type", "schedule_skipped",
					"schedule", schedule.Name,
					"reason", "drained")
				return
			}
		}
		// Pause gate: a schedule with an active schedule_state.paused=1
		// row is silently skipped at tick time. This is independent of
		// the HCL definition — operators can pause without round-tripping
		// through the config source. Resume flips the row, no reload
		// needed. Missing rows (the common case) default to "active".
		if st := StateStore(); st != nil {
			if paused, err := st.IsSchedulePaused(schedule.Name); err != nil {
				slog.Warn("pause check failed; proceeding with schedule",
					"schedule", schedule.Name, "error", err.Error())
			} else if paused {
				slog.Info("Skipping tick: schedule paused",
					"entry_type", "schedule_skipped",
					"schedule", schedule.Name,
					"reason", "paused")
				return
			}
		}

		slog.Info("Queuing...", "schedule", schedule.Name)
		var loc *time.Location
		if schedule.Timezone != "" {
			loc, _ = time.LoadLocation(schedule.Timezone)
		} else {
			loc = time.Local
		}

		schedule.Now = time.Now().In(loc)
		schedule.RunID = newRunID()
		if schedule.Source == "" {
			schedule.Source = "cron"
		}

		var endDate time.Time
		if schedule.EndDate == "" {
			//if EndDate is not given, default to 1 Year from now
			endDate = schedule.Now.Add(time.Duration(1) * time.Hour * 24 * 365)
		} else {
			endDate, _ = time.Parse("2006-01-02", schedule.EndDate)
		}
		startDate, _ := time.Parse("2006-01-02", schedule.StartDate)
		if schedule.Now.After(endDate) || schedule.Now.Before(startDate) {
			s := fmt.Sprintf("now=%s is not between start_date=%s and end_date=%s... Schedule will not execute.", schedule.Now, startDate, endDate)
			slog.Warn(s, "schedule", schedule.Name)
		} else {
			schedule.CleanGit()
			queue <- schedule.JSON()
		}

	}
}

// ExecTasks parses the cronicle.hcl config, filters for a specified task
// and executes the task
func ExecTasks(cronicleFile string, taskName string, scheduleName string, now time.Time) {

	cronicleFileAbs, err := filepath.Abs(cronicleFile)
	if err != nil {
		Fatal(err)
	}
	// Foreground one-shot: use the already-open state store (opened by
	// cmd/exec.go for secret resolution) if available. Otherwise fall
	// back to an ephemeral in-memory projection.
	if StateStore() == nil {
		if err := EnableStateStore(":memory:"); err != nil {
			slog.Warn("state store open failed; projection disabled", "error", err.Error())
		}
	}
	slog.Info("Loading " + cronicleFileAbs)
	if !fileExists(cronicleFileAbs) {
		Fatal("file does not exist", "path", cronicleFileAbs)
	}

	conf, err := GetConfig(cronicleFileAbs)
	if err != nil {
		Fatal(err)
	}

	var loc *time.Location
	if conf.Timezone != "" {
		loc, err = time.LoadLocation(conf.Timezone)
		if err != nil {
			Fatal(err)
		}
	} else {
		loc = time.Local
	}

	ApplyTimezone(loc)
	slog.Info("executing tasks...", "cronicle", "exec")

	nowInLoc := now.In(loc)
	var schedules []Schedule
	if scheduleName != "" {
		schedules = []Schedule{conf.ScheduleMap()[scheduleName]}
	} else {
		schedules = conf.Schedules
	}

	for _, schedule := range schedules {
		taskMap := schedule.TaskMap()
		if taskName != "" {
			if task, ok := taskMap[taskName]; ok {
				// Set the schedule-scoped scratch dir so single-task
				// `cronicle exec --task X` produces the same ${scratch}
				// artifacts as a full schedule run. The dir is created
				// best-effort; ${scratch} substitution silently no-ops on
				// failure.
				if scratch := schedule.scratchDirFor(nowInLoc); scratch != "" {
					_ = os.MkdirAll(scratch, 0o755)
					task.ScratchDir = scratch
				}
				task.Execute(nowInLoc)
			}
		} else {
			schedule.Now = nowInLoc
		schedule.RunID = newRunID()
		schedule.Source = "exec"
			schedule.ExecuteTasks()
		}
	}
}
