package cronicle

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	cron "github.com/robfig/cron/v3"
)

// Run is the main function of the cron package
func Run(cronicleFile string, runOptions RunOptions) {

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

	//TODO: WaitGroup is currently only used for testing, could be used in Producer
	var wg sync.WaitGroup
	wg.Add(1) //Ensure WaitGroup counter > 0
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
	var triggerQueue chan<- []byte
	if runOptions.ListenAddr != "" && runOptions.ListenToken != "" && stateStore != nil {
		enqueueChan := make(chan []byte, 64)
		triggerQueue = enqueueChan
		go enqueueAdapter(enqueueChan, stateStore)
		go StartCron(cronicleFileAbs, enqueueChan)
		if runOptions.RunWorker {
			go selfWorker(stateStore, croniclePath, &wg)
		}
		go reaperLoop(stateStore)
	} else {
		queue := make(chan []byte)
		triggerQueue = queue
		go StartCron(cronicleFileAbs, queue)
		go ConsumeSchedule(queue, croniclePath, &wg)
	}

	startListener(runOptions.ListenAddr, runOptions.ListenToken, triggerQueue)

	wg.Wait() //Wait forever

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
}

//StartCron pushes all schedules in the given config to the cron scheduler
//starts the cron scheduler which publishes the serialzied
//schedules to the message queue for execution.
func StartCron(cronicleFile string, queue chan<- []byte) {

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
		conf.Heartbeat = "@every 30s"
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

//LoadCron exeutes GetConfig(cronicleFile) to load the current config from file,
//checks the given config against the global confPrior, and if there is a change,
//stops the cron, removes all of the confPrior cron entries and adds the new conf
//schedules to the cron.
func LoadCron(cronicleFile string, c *cron.Cron, queue chan<- []byte, force bool) {

	slog.Info("Loading config...", "cronicle", "heartbeat", "path", cronicleFile)
	conf, err := GetConfig(cronicleFile)
	if err != nil {
		// HCL parse / decode failed. ParseFile returns (nil, diags) on
		// hard parse errors, so falling through to conf.Hcl() below
		// would deref a nil pointer and crash the producer. Keep the
		// previous good config in place; operator can fix the file and
		// the next heartbeat picks up the change.
		slog.Error("config reload failed; keeping previous schedules in place",
			"path", cronicleFile, "error", err.Error())
		return
	}
	if conf == nil {
		// Defensive: GetConfig contract is (nil, err) on failure, but
		// some future change might return (nil, nil). Treat that the
		// same way as an explicit error.
		slog.Error("config reload returned nil config; keeping previous schedules in place",
			"path", cronicleFile)
		return
	}

	if string(confPriorGlobal.Hcl().Bytes) != string(conf.Hcl().Bytes) || force {
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
				_, err := c.AddFunc(schedule.Cron, ProduceSchedule(schedule, queue))
				if err != nil {
					fmt.Printf("\x1b[31;1m%s\x1b[0m\n", fmt.Sprintf("schedule cron format error: %s", schedule.Name))
					Fatal(err)
				}
			}

		}
		c.Start()
	}
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
	// Foreground one-shot: ephemeral in-memory projection. The user is
	// watching the run; nothing later will query the projection. Keeps
	// disk untouched so `cronicle exec` stays write-free where the user
	// hasn't asked for --log-to-file.
	if err := EnableStateStore(":memory:"); err != nil {
		slog.Warn("state store open failed; projection disabled", "error", err.Error())
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
