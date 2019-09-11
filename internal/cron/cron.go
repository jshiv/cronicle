package cron

import (
	"runtime"

	"github.com/jshiv/cronicle/internal/config"
	log "github.com/sirupsen/logrus"
	"gopkg.in/robfig/cron.v2"
)

//RunSchedule starts cron
func RunSchedule(schedule config.Schedule) {
	log.WithFields(log.Fields{"cronicle": "start"}).Info("Starting Scheduler...")

	c := cron.New()
	c.AddFunc("@every 1s", func() { log.WithFields(log.Fields{"cronicle": "heartbeat"}).Info("Running...") })
	c.AddFunc(schedule.Cron,
		func() { log.WithFields(log.Fields{"job": "Schedule"}).Info("Running...") })
	c.Start()
	runtime.Goexit()
}
