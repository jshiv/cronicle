package cronicle

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	cron "github.com/robfig/cron/v3"
)

// redactDSN masks the password in a postgres:// DSN for safe logging.
func redactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return dsn
	}
	if _, hasPw := u.User.Password(); hasPw {
		u.User = url.UserPassword(u.User.Username(), "***")
		return u.String()
	}
	return dsn
}

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
	globalRuntime.storeConf(conf)

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

	// State plane: projection of slog events + the durable job queue.
	// Default is a local SQLite at .cronicle/state.db (ephemeral in managed
	// pods). When --state-source / CRONICLE_STATE_SOURCE points at a
	// postgres:// DSN, the producer uses a durable, restart-surviving store
	// instead — so run history (and ${last_run}) persists across pod
	// recreation. The worker is unaffected: it still reaches all state
	// through the producer over HTTP.
	if runOptions.StateSource != "" {
		if err := EnableStateStore(runOptions.StateSource); err != nil {
			slog.Warn("state store open failed; projection disabled",
				"source", redactDSN(runOptions.StateSource), "error", err.Error())
		} else {
			slog.Info("state store: external", "source", redactDSN(runOptions.StateSource))
		}
	} else {
		stateDir := filepath.Join(croniclePath, ".cronicle")
		if err := os.MkdirAll(stateDir, 0o755); err != nil {
			slog.Warn("state dir mkdir failed; projection disabled", "error", err.Error())
		} else if err := EnableStateStore(filepath.Join(stateDir, "state.db")); err != nil {
			slog.Warn("state store open failed; projection disabled", "error", err.Error())
		}
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
			selfWorkerPool(ctx, stateStore, croniclePath, runOptions.WorkerCount, &wg)
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
	// WorkerCount is how many in-process self-workers to run in queue
	// mode. Each runs the shared (serial) consume loop, so N workers =
	// up to N jobs executing concurrently. <=1 means one worker (serial).
	// Ignored when RunWorker is false or in non-listener mode.
	WorkerCount int
	// StateSource overrides where the state plane lives. Empty → local
	// .cronicle/state.db (SQLite). A postgres:// DSN → durable Postgres,
	// so run history / ${last_run} survive pod recreation. Set via
	// --state-source or CRONICLE_STATE_SOURCE; in managed deployments the
	// control plane injects a per-deployment DSN for the PRODUCER pod.
	StateSource string
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
	// Record static-entry IDs BEFORE starting the scheduler. With the
	// previous order (c.Start() first, AddFunc, then set the map), a
	// refresh tick firing in the gap between AddFunc returning and the
	// map being populated would observe an empty static set and strip
	// the heartbeat + refresh entries it was registered against. By
	// populating the map before c.Start(), no tick can fire until both
	// the entries and the static-set mask are in place.
	globalRuntime.setStaticEntries(map[cron.EntryID]bool{hbID: true, refreshID: true})
	// Initial dynamic-schedule registration. LoadCron no longer touches
	// the cron lifecycle (used to call Stop/Start, which raced its own
	// run goroutine), so AddFunc against the not-yet-started scheduler
	// is a plain append — no ticks can fire until c.Start() below.
	LoadCron(cronicleFile, c, queue, true)
	c.Start()

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

	if !globalRuntime.hclEquals(rawHCL) || force {
		slog.Info("Refreshing config...", "cronicle", "heartbeat", "path", cronicleFile)
		// NOTE: do NOT call c.Stop() / c.Start() here. Stop() returns
		// before the run goroutine has actually exited (it returns a
		// context the caller can wait on), so a Start() immediately
		// after races a second run goroutine against the still-draining
		// first one. AddFunc / Remove are documented safe-to-call while
		// the cron is running — they coordinate with the run goroutine
		// via the c.add / c.remove channels — so the swap can happen
		// in-place.
		for _, entry := range c.Entries() {
			// Preserve the heartbeat + config_refresh static entries; only
			// remove the dynamic per-schedule entries so we can re-register
			// them from the new conf.
			if globalRuntime.isStaticEntry(entry.ID) {
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
	}
	globalRuntime.storeConfAndHCL(conf, rawHCL)

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
				// Bail before propagating / executing a zero-value Schedule
				// — would otherwise silently no-op (no tasks) with no
				// retry signal, hiding malformed payloads on the queue.
				slog.Error("schedule unmarshal failed", "error", err.Error())
				return
			}
			schedule.PropigateTaskProperties(p)
			// In-process dispatch (non-queue, single-node): resolve
			// ${last_run} here, where StateStore() is the authoritative
			// store, before ExecuteTasks (which never reads state itself).
			resolveLastRun(StateStore(), &schedule)
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
			var err error
			loc, err = time.LoadLocation(schedule.Timezone)
			if err != nil || loc == nil {
				// Bad IANA name (typo, unknown zone) returns (nil, err).
				// Using a nil Location with time.Now().In(nil) panics; fall
				// back to time.Local so the schedule still ticks and log the
				// problem so the operator can fix the HCL.
				errMsg := "nil location"
				if err != nil {
					errMsg = err.Error()
				}
				slog.Warn("invalid timezone; falling back to local",
					"schedule", schedule.Name,
					"timezone", schedule.Timezone,
					"error", errMsg)
				loc = time.Local
			}
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
			var err error
			endDate, err = time.Parse("2006-01-02", schedule.EndDate)
			if err != nil {
				// Malformed end_date (e.g. "2026/01/01") used to silently
				// produce zero time, making Now.After(endDate) true for
				// any real time → the schedule never fires. Log and skip
				// the tick so the operator can see something's wrong.
				slog.Warn("invalid end_date; skipping tick",
					"schedule", schedule.Name,
					"end_date", schedule.EndDate,
					"error", err.Error())
				return
			}
		}
		var startDate time.Time
		if schedule.StartDate != "" {
			var err error
			startDate, err = time.Parse("2006-01-02", schedule.StartDate)
			if err != nil {
				slog.Warn("invalid start_date; skipping tick",
					"schedule", schedule.Name,
					"start_date", schedule.StartDate,
					"error", err.Error())
				return
			}
		}
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
				res, err := task.Execute(nowInLoc)
				// Propagate failure as a non-zero exit code so CI / cron
				// scripts that wrap `cronicle exec --task` actually see
				// the task failed. Without this, the process exited 0
				// regardless of subprocess outcome.
				//
				// Check ExitStatus before err: when a subprocess exits
				// non-zero, exec.Result.Error mirrors the ExitStatus, and
				// falling through to Fatal would clobber a real "42" into
				// the generic "1". Only when no exit status was produced
				// (validation error, couldn't spawn, agent setup failure
				// with no subprocess) do we Fatal with 1.
				if res.ExitStatus != 0 {
					os.Exit(res.ExitStatus)
				}
				if err != nil {
					Fatal(err)
				}
			}
		} else {
			schedule.Now = nowInLoc
			schedule.RunID = newRunID()
			schedule.Source = "exec"
			// Foreground one-shot dispatch: resolve ${last_run} from the
			// authoritative store before ExecuteTasks (which never reads it).
			resolveLastRun(StateStore(), &schedule)
			schedule.ExecuteTasks()
		}
	}
}
