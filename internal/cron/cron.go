package cron

import (
	"fmt"
	"runtime"

	"github.com/jshiv/cronicle/internal/bash"
	"github.com/jshiv/cronicle/internal/config"

	log "github.com/sirupsen/logrus"
	"gopkg.in/robfig/cron.v2"
)

// Run is the main function of the run package
func Run() {
	var conf config.Config

	fmt.Println("run called")
	fmt.Println("This is something else.")
	conf.Version = "1.2.3"
	conf.Schedule.Cron = "@every 2s"
	conf.Schedule.Command = "echo Hello World"
	fmt.Println(conf)

	RunSchedule(conf.Schedule)
}

//RunSchedule starts cron
func RunSchedule(schedule config.Schedule) {
	log.WithFields(log.Fields{"cronicle": "start"}).Info("Starting Scheduler...")

	c := cron.New()
	c.AddFunc("@every 1s", func() { log.WithFields(log.Fields{"cronicle": "heartbeat"}).Info("Running...") })
	c.AddFunc(schedule.Cron, AddSchedule(schedule))
	c.Start()
	runtime.Goexit()
}

// AddSchedule retuns a function primed with the given schedules commands
func AddSchedule(schedule config.Schedule) func() {
	return func() {
		log.WithFields(log.Fields{"command": schedule.Command}).Info("Start")
		out := bash.Bash(schedule.Command)
		log.WithFields(log.Fields{
			"command":     out.Command,
			"exit-status": out.ExitStatus,
			"stderr":      out.Stderr,
			"stdout":      out.Stdout,
		}).Info("End")
	}
}

func Dummy(in string) string {
	return in
}
