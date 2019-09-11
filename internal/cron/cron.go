package cron

import (
	"fmt"
	"runtime"

	"github.com/jshiv/cronicle/internal/bash"
	"github.com/jshiv/cronicle/internal/config"

	log "github.com/sirupsen/logrus"
	"gopkg.in/robfig/cron.v2"
)

// Run is the main function of the cron package
func Run() {
	var conf config.Config

	testSchedule1 := config.Schedule{
		Name: "schedule-1",
		Cron: "@every 2s",
		Tasks: []config.Task{
			{
				Name:    "task1",
				Command: []string{"/bin/echo", "Hello World"},
			},
			{
				Name:    "task2",
				Command: []string{"/bin/echo", "This is Task2"},
			},
		},
	}
	testSchedule2 := config.Schedule{
		Name: "schedule-2",
		Cron: "@every 5s",
		Tasks: []config.Task{
			{
				Name:    "dice",
				Command: []string{"python", "-c", "import random; print(random.randint(1, 6))"},
			},
		},
	}
	conf.Schedules = []config.Schedule{testSchedule1, testSchedule2}
	fmt.Println(conf)

	RunConfig(conf)
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
			log.WithFields(log.Fields{
				"task": task.Name,
				"exit": result.ExitStatus,
			}).Info(result.Stdout)
		}
	}
}

func Dummy(in string) string {
	return in
}
