package cronicle

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-redis/redis"
	"github.com/matryer/vice"
	nsqvice "github.com/matryer/vice/queues/nsq"
	redisvice "github.com/matryer/vice/queues/redis"
	"github.com/nsqio/go-nsq"

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

	if runOptions.QueueType == "" {
		if conf.Queue != nil {
			runOptions.QueueType = conf.Queue.Type
		}
	}
	//TODO: WaitGroup is currently only used for testing, could be used in Producer
	var wg sync.WaitGroup
	wg.Add(1) //Ensure WaitGroup counter > 0
	// triggerQueue is the same send-side queue StartCron pushes to on each
	// cron tick. The listener (if enabled) writes to it for remote-trigger
	// fires, so triggered and cron-fired runs are identical from the
	// consumer's perspective — same JSON, same DAG walk, same logs.
	var triggerQueue chan<- []byte
	if runOptions.QueueType == "" {
		queue := make(chan []byte)
		triggerQueue = queue
		go StartCron(cronicleFileAbs, queue)
		go ConsumeSchedule(queue, croniclePath, &wg)
	} else {
		transport := MakeViceTransport(runOptions.QueueType, runOptions.Addr)
		triggerQueue = transport.Send(runOptions.QueueName)
		go StartCron(cronicleFileAbs, triggerQueue)
		if runOptions.RunWorker {
			go ConsumeSchedule(transport.Receive(runOptions.QueueName), croniclePath, &wg)
		}
	}

	startListener(runOptions.ListenAddr, runOptions.ListenToken, triggerQueue)

	wg.Wait() //Wait forever

}

// RunOptions enables the runtime configuration of the distributed message queue
type RunOptions struct {
	RunWorker bool
	QueueType string
	QueueName string
	Addr      string
	LogToFile bool
	// ListenAddr / ListenToken expose the remote-trigger HTTP API. Empty
	// addr disables the listener entirely; non-empty addr REQUIRES a token
	// (the listener refuses to bind otherwise — see internal/cronicle/listen.go).
	ListenAddr  string
	ListenToken string
}

// StartWorker listens to a vice transport queue for schedules
// produced by cronicle run.
//
// File logging is honored on workers too — they're exactly where you want
// per-run agent transcripts and the rotated cronicle.jsonl, since the work
// happens here, not on the producer. Without this, distributed runs leave
// no audit trail on disk.
func StartWorker(path string, runOptions RunOptions) {

	pathAbs, err := filepath.Abs(path)
	if err != nil {
		Fatal(err)
	}

	if runOptions.QueueType == "" {
		slog.Error("--queue must be specified in distributed mode. [Options: redis, nsq]")
	}

	if runOptions.LogToFile {
		if err := EnableFileLog(pathAbs); err != nil {
			Fatal(err)
		}
	}

	transport := MakeViceTransport(runOptions.QueueType, runOptions.Addr)
	schedules := transport.Receive(runOptions.QueueName)
	var wg sync.WaitGroup
	wg.Add(1) //Ensure WaitGroup counter > 0
	go ConsumeSchedule(schedules, pathAbs, &wg)

	wg.Wait()

}

//MakeViceTransport creates a vice.Transport interface from the given
//queue field in the config
func MakeViceTransport(queueType string, addr string) vice.Transport {
	// var transport *nsqvice.Transport

	switch queueType {
	case "redis":
		if addr == "" {
			addr = "127.0.0.1:6379"
		}
		opts := &redis.Options{
			Network:    "tcp",
			Addr:       addr,
			Password:   "",
			DB:         0,
			MaxRetries: 0,
		}
		client := redis.NewClient(opts)
		opt := redisvice.WithClient(client)
		transport := redisvice.New(opt)
		return transport
	case "nsq":
		transport := nsqvice.New()
		transport.ConnectConsumer = func(consumer *nsq.Consumer) error {
			if addr == "" {
				return consumer.ConnectToNSQD(nsqvice.DefaultTCPAddr)
			}
			return consumer.ConnectToNSQLookupd(addr)

		}
		return transport
	}

	// return transpor
	return nsqvice.New()

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
	c.AddFunc(conf.Heartbeat, func() { LoadCron(cronicleFile, c, queue, false) })
	LoadCron(cronicleFile, c, queue, true)
}

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
		slog.Error("config load failed", "error", err.Error())
	}

	if string(confPriorGlobal.Hcl().Bytes) != string(conf.Hcl().Bytes) || force {
		slog.Info("Refreshing config...", "cronicle", "heartbeat", "path", cronicleFile)
		c.Stop()
		for _, entry := range c.Entries() {
			// assumes that LoadCron has entry.ID == 1
			if entry.ID > 1 {
				c.Remove(entry.ID)

			}
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
