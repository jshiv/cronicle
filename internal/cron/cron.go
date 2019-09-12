package cron

import (
	"runtime"

	"github.com/jshiv/cronicle/internal/bash"
	"github.com/jshiv/cronicle/internal/config"

	log "github.com/sirupsen/logrus"
	"gopkg.in/robfig/cron.v2"
)

// Run is the main function of the cron package
func Run(filename string) {

	conf, err := config.ParseFile(filename)
	if err != nil {
		panic(err)
	}

	RunConfig(*conf)
}

//RunConfig starts cron
func RunConfig(conf config.Config) {
	log.WithFields(log.Fields{"cronicle": "start"}).Info("Starting Scheduler...")

	c := cron.New()
	c.AddFunc("@every 10s", func() { log.WithFields(log.Fields{"cronicle": "heartbeat"}).Info("Running...") })
	for _, schedule := range conf.Schedules {
		c.AddFunc(schedule.Cron, AddSchedule(schedule))
	}
	c.Start()
	runtime.Goexit()
}

// AddSchedule retuns a function primed with the given schedules commands
func AddSchedule(schedule config.Schedule) func() {
	log.WithFields(log.Fields{"schedule": schedule.Name}).Info("Running...")

	return func() {
		for _, task := range schedule.Tasks {
			log.WithFields(log.Fields{"task": task.Name}).Info(task.Command)
			result := bash.Bash(task.Command)
			if result.ExitStatus == 0 {
				log.WithFields(log.Fields{
					"task": task.Name,
					"exit": result.ExitStatus,
				}).Info(result.Stdout)
			} else if result.ExitStatus == 1 {
				log.WithFields(log.Fields{
					"task": task.Name,
					"exit": result.ExitStatus,
				}).Error(result.Stderr)
			}
		}
	}
}

func Dummy(in string) string {
	return in
}
