package cronicle

import (
	"encoding/json"
	"fmt"
	"path"
	"runtime"
	"time"

	"github.com/go-redis/redis"
	"github.com/matryer/vice"
	nsqvice "github.com/matryer/vice/queues/nsq"
	redisvice "github.com/matryer/vice/queues/redis"
	"github.com/nsqio/go-nsq"
	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/fatih/color"

	"path/filepath"

	cron "github.com/robfig/cron/v3"
	log "github.com/sirupsen/logrus"
)

// QueueArgs enables the runtime configuration of the distributed message queue
type QueueArgs struct {
	RunWorker bool
	QueueType string
	QueueName string
	Addr      string
}

// Run is the main function of the cron package
func Run(cronicleFile string, logToFile bool, queueArgs QueueArgs) {

	cronicleFileAbs, err := filepath.Abs(cronicleFile)
	if err != nil {
		log.Fatal(err)
	}

	if !fileExists(cronicleFileAbs) {
		log.Fatal("file does not exist: ", cronicleFileAbs)
	}
	croniclePath := filepath.Dir(cronicleFileAbs)

	conf, err := GetConfig(cronicleFileAbs)
	if err != nil {
		log.Fatal(err)
	}
	confPriorGlobal = conf
	hcl := conf.Hcl()
	slantyedCyan := color.New(color.FgCyan, color.Italic).SprintFunc()
	fmt.Printf("%s", slantyedCyan(string(hcl.Bytes)))

	if logToFile {
		logPath := path.Join(croniclePath, path.Join(".cronicle", "log"))
		logFile := path.Join(logPath, "cronicle.log")
		log.SetOutput(&lumberjack.Logger{
			Filename:   logFile,
			MaxSize:    500, // megabytes
			MaxBackups: 3,
			MaxAge:     28,   //days
			Compress:   true, // disabled by default
		})
	}

	if queueArgs.QueueType == "" {
		if conf.Queue != nil {
			queueArgs.QueueType = conf.Queue.Type
		}
	}

	if queueArgs.QueueType == "" {
		queue := make(chan []byte)
		go StartCron(cronicleFileAbs, queue)
		go ConsumeSchedule(queue, croniclePath)
	} else {
		transport := MakeViceTransport(queueArgs.QueueType, queueArgs.Addr)
		go StartCron(cronicleFileAbs, transport.Send(queueArgs.QueueName))
		if queueArgs.RunWorker {
			go ConsumeSchedule(transport.Receive(queueArgs.QueueName), croniclePath)
		}
	}

	runtime.Goexit()
}

// ExecTasks parses the cronicle.hcl config, filters for a specified task
// and executes the task
func ExecTasks(cronicleFile string, taskName string, scheduleName string, now time.Time, queueArgs QueueArgs) {

	cronicleFileAbs, err := filepath.Abs(cronicleFile)
	if err != nil {
		log.Fatal(err)
	}
	log.Info("Loading " + cronicleFileAbs)
	if !fileExists(cronicleFileAbs) {
		log.Fatal("file does not exist: ", cronicleFileAbs)
	}

	conf, err := GetConfig(cronicleFileAbs)
	if err != nil {
		log.Fatal(err)
	}

	var loc *time.Location
	if conf.Timezone != "" {
		loc, err = time.LoadLocation(conf.Timezone)
		if err != nil {
			log.Fatal(err)
		}
	} else {
		loc = time.Local
	}

	log.SetFormatter(TZFormatter{Formatter: &log.TextFormatter{
		FullTimestamp: true,
	}, loc: loc})
	log.WithFields(log.Fields{"cronicle": "exec"}).Info("executing tasks...")

	nowInLoc := now.In(loc)
	var schedules []Schedule
	if scheduleName != "" {
		schedules = []Schedule{conf.ScheduleMap()[scheduleName]}
	} else {
		schedules = conf.Schedules
	}

	if queueArgs.QueueType == "" {
		if conf.Queue != nil {
			queueArgs.QueueType = conf.Queue.Type
		}
	}

	for _, schedule := range schedules {
		taskMap := schedule.TaskMap()
		if taskName != "" {
			if task, ok := taskMap[taskName]; ok {
				if queueArgs.QueueType == "" {
					task.Execute(nowInLoc)
				} else {
					transport := MakeViceTransport(queueArgs.QueueType, queueArgs.Addr)
					// Create a new schedule containing only the requested task for publishing to the queue
					singleTaskSchedule := Schedule{
						Name:         schedule.Name,
						Cron:         schedule.Cron,
						Timezone:     schedule.Timezone,
						Repo:         schedule.Repo,
						CronicleRepo: schedule.CronicleRepo,
						Now:          nowInLoc,
						Tasks:        []Task{task},
					}
					ProduceSchedule(singleTaskSchedule, transport.Send(queueArgs.QueueName))()
				}

			}
		} else {
			schedule.Now = nowInLoc
			if queueArgs.QueueType == "" {
				schedule.ExecuteTasks()
			} else {
				transport := MakeViceTransport(queueArgs.QueueType, queueArgs.Addr)
				ProduceSchedule(schedule, transport.Send(queueArgs.QueueName))()
			}
		}
	}
}

// StartWorker listens to a vice transport queue for schedules
// produced by cronicle run
func StartWorker(path string, queueArgs QueueArgs) {

	pathAbs, err := filepath.Abs(path)
	if err != nil {
		log.Fatal(err)
	}

	if queueArgs.QueueType == "" {
		log.Error("--queue must be specified in distributed mode. [Options: redis, nsq]")
	}
	transport := MakeViceTransport(queueArgs.QueueType, queueArgs.Addr)
	schedules := transport.Receive(queueArgs.QueueName)
	go ConsumeSchedule(schedules, pathAbs)

	runtime.Goexit()

}

//StartCron pushes all schedules in the given config to the cron scheduler
//starts the cron scheduler which publishes the serialzied
//schedules to the message queue for execution.
func StartCron(cronicleFile string, queue chan<- []byte) {

	conf, err := GetConfig(cronicleFile)
	if err != nil {
		log.Fatal(err)
	}
	var loc *time.Location
	if conf.Timezone != "" {
		loc, err = time.LoadLocation(conf.Timezone)
		if err != nil {
			log.Fatal(err)
		}
	} else {
		loc = time.Local
	}

	log.SetFormatter(TZFormatter{Formatter: &log.TextFormatter{
		FullTimestamp: true,
	}, loc: loc})
	log.WithFields(log.Fields{"cronicle": "start"}).Info("Starting Scheduler...")

	for _, schedule := range conf.Schedules {
		switch {
		case schedule.Cron == "@once":
			log.WithFields(log.Fields{"schedule": schedule.Name, "cron": schedule.Cron}).Info("Executing @Once")
			ProduceSchedule(schedule, queue)()
		case schedule.Cron == "":
			log.WithFields(log.Fields{"schedule": schedule.Name, "cron": schedule.Cron}).Info("Skip execution. Use 'cronicle exec' to run.")
		default:
			log.WithFields(log.Fields{"schedule": schedule.Name, "cron": schedule.Cron}).Info("Starting cron...")
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

	log.WithFields(log.Fields{"cronicle": "heartbeat", "path": cronicleFile}).Info("Loading config...")
	conf, err := GetConfig(cronicleFile)
	if err != nil {
		log.Error(err)
	}

	if string(confPriorGlobal.Hcl().Bytes) != string(conf.Hcl().Bytes) || force {
		log.WithFields(log.Fields{"cronicle": "heartbeat", "path": cronicleFile}).Info("Refreshing config...")
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
				log.WithFields(log.Fields{"schedule": schedule.Name, "cron": schedule.Cron}).Info("@once execution complete at 'cronicle run'")
			case schedule.Cron == "":
				log.WithFields(log.Fields{"schedule": schedule.Name, "cron": schedule.Cron}).Warn("Skip execution. Use 'cronicle exec' to run.")
			default:
				_, err := c.AddFunc(schedule.Cron, ProduceSchedule(schedule, queue))
				if err != nil {
					fmt.Printf("\x1b[31;1m%s\x1b[0m\n", fmt.Sprintf("schedule cron format error: %s", schedule.Name))
					log.Fatal(err)
				}
			}

		}
		c.Start()
	}
	confPriorGlobal = conf

}

//ConsumeSchedule consumes the byte array of a
//schedule from the message queue for execution
func ConsumeSchedule(queue <-chan []byte, path string) {
	var p string
	if path == "" {
		p, _ = filepath.Abs("./")
	} else {
		p = path
	}
	for scheduleBytes := range queue {

		var schedule Schedule
		err := json.Unmarshal(scheduleBytes, &schedule)
		if err != nil {
			log.Error(err)
		}
		schedule.PropigateTaskProperties(p)
		schedule.ExecuteTasks()

	}
}

//ProduceSchedule produces the json of a
//schdule to the message queue for consumption
func ProduceSchedule(schedule Schedule, queue chan<- []byte) func() {
	return func() {
		log.WithFields(log.Fields{"schedule": schedule.Name}).Info("Queuing...")
		var loc *time.Location
		if schedule.Timezone != "" {
			loc, _ = time.LoadLocation(schedule.Timezone)
		} else {
			loc = time.Local
		}

		schedule.Now = time.Now().In(loc)

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
			log.WithFields(log.Fields{
				"schedule": schedule.Name,
			}).Warn(s)
		} else {
			schedule.CleanGit()
			queue <- schedule.JSON()
		}

	}
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
