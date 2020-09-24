package cron

import (
	"encoding/json"
	"fmt"
	"runtime"
	"time"

	"github.com/jshiv/cronicle/internal/config"
	"github.com/matryer/vice"
	nsqvice "github.com/matryer/vice/queues/nsq"
	"github.com/nsqio/go-nsq"
	"github.com/matryer/vice/queues/redis"

	"github.com/fatih/color"

	"path/filepath"

	cron "github.com/robfig/cron/v3"
	log "github.com/sirupsen/logrus"
)

// Run is the main function of the cron package
func Run(cronicleFile string, runOptions RunOptions) {

	cronicleFileAbs, err := filepath.Abs(cronicleFile)
	if err != nil {
		log.Fatal(err)
	}
	croniclePath := filepath.Dir(cronicleFileAbs)

	conf, _ := config.GetConfig(cronicleFileAbs)
	hcl := conf.Hcl()
	slantyedCyan := color.New(color.FgCyan, color.Italic).SprintFunc()
	fmt.Printf("%s", slantyedCyan(string(hcl.Bytes)))

	if runOptions.QueueType == "" {
		runOptions.QueueType = conf.Queue.Type
	}
	if runOptions.QueueType == "" {
		schedules := make(chan []byte)
		go StartCron(*conf, schedules)
		go ConsumeSchedule(schedules, croniclePath)
	} else {
		transport := MakeViceTransport(runOptions.QueueType, "")
		go StartCron(*conf, transport.Send("schedules"))
		if runOptions.RunWorker {
			go ConsumeSchedule(transport.Receive("schedules"), croniclePath)
		}
	}

	runtime.Goexit()

}

type RunOptions struct {
	RunWorker bool
	QueueType string
}

// StartWorker listens to a vice transport queue for schedules
// produced by cronicle run
func StartWorker(path string, runOptions RunOptions) {

	pathAbs, err := filepath.Abs(path)
	if err != nil {
		log.Fatal(err)
	}

	if runOptions.QueueType == "" {
		log.Error("--queue must be specified in distributed mode. Options: redis, nsq]")
	}
	transport := MakeViceTransport(runOptions.QueueType, "")
	schedules := transport.Receive("schedules")
	go ConsumeSchedule(schedules, pathAbs)

	runtime.Goexit()

}

//MakeViceTransport creates a vice.Transport interface from the given
//queue field in the config
func MakeViceTransport(queueType string, addr string) vice.Transport {
	// var transport *nsqvice.Transport
	
	switch queueType {
	case "redis":
		transport := redis.New()
		return transport
	case "nsq":
		transport := nsqvice.New()
		transport.ConnectConsumer = func(consumer *nsq.Consumer) error {return consumer.ConnectToNSQLookupd(addr) }
		return transport
	}

	// return transpor
	return nsqvice.New()
	
}

//StartCron pushes all schedules in the given config to the cron scheduler
//starts the cron scheduler which publishes the serialzied
//schedules to the message queue for execution.
func StartCron(conf config.Config, schedules chan<- []byte) {
	log.WithFields(log.Fields{"cronicle": "start"}).Info("Starting Scheduler...")

	c := cron.New()
	c.AddFunc("@every 6m", func() { log.WithFields(log.Fields{"cronicle": "heartbeat"}).Info("Running...") })
	for _, schedule := range conf.Schedules {
		_, err := c.AddFunc(schedule.Cron, ProduceSchedule(schedule, schedules))
		if err != nil {
			fmt.Printf("\x1b[31;1m%s\x1b[0m\n", fmt.Sprintf("schedule cron format error: %s", schedule.Name))
			log.Fatal(err)
		}
	}
	c.Start()
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

		var s config.Schedule
		err := json.Unmarshal(scheduleBytes, &s)
		s.PropigateTaskProperties(p)

		if err != nil {
			fmt.Println(err)
		}

		ExecuteTasks(s)()
	}
}

//ProduceSchedule produces the json of a
//schdule to the message queue for consumption
func ProduceSchedule(schedule config.Schedule, queue chan<- []byte) func() {
	return func() {
		log.WithFields(log.Fields{"schedule": schedule.Name}).Info("Queuing...")
		schedule.Now = time.Now().In(time.Local)
		schedule.CleanGit()
		queue <- schedule.JSON()
	}
}

// ExecuteTasks handels the execution of all tasks in a given schedule.
// By default tasks execute in parallel unless wait_for is given
func ExecuteTasks(schedule config.Schedule) func() {
	return func() {
		// TODO: added location specification in schedule struct
		// https://godoc.org/github.com/robfig/cron
		var now time.Time
		if (schedule.Now == time.Time{}) {
			now = time.Now().In(time.Local)
		} else {
			now = schedule.Now
		}
		fmt.Println("Schedule exec time: ", now)
		for _, task := range schedule.Tasks {
			//TODO: Setup dag execution
			//[[task1, task2], [task3], [task4, task5]]?
			go func(task config.Task) {
				r, err := task.Execute(now)
				fmt.Println(err)
				task.Log(r)
			}(task)

		}
	}
}

// ExecTasks parses the cronicle.hcl config, filters for a specified task
// and executes the task
func ExecTasks(cronicleFile string, taskName string, scheduleName string, now time.Time) {

	cronicleFileAbs, err := filepath.Abs(cronicleFile)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Reading from: " + cronicleFileAbs)

	conf, _ := config.GetConfig(cronicleFileAbs)

	tasks := conf.TaskArray().FilterTasks(taskName, scheduleName)
	slantyedCyan := color.New(color.FgCyan, color.Italic).SprintFunc()

	var c config.Config
	execSchedule := config.Schedule{Name: "exec", Cron: now.String(), Tasks: tasks}
	c.Schedules = []config.Schedule{execSchedule}
	fmt.Printf("%s", slantyedCyan(string(c.Hcl().Bytes)))

	for _, task := range tasks {
		r, _ := task.Execute(now)
		task.Log(r)
	}
}
