package cron

import (
	"runtime"

	log "github.com/sirupsen/logrus"
	"gopkg.in/robfig/cron.v2"
	//https://github.com/distribworks/dkron
)

//RunSchedule starts cron
func RunSchedule() {
	log.SetFormatter(&log.TextFormatter{
		DisableColors: false,
		FullTimestamp: true,
	})
	log.WithFields(log.Fields{"cronicle": "start"}).Info("Starting Scheduler...")

	c := cron.New()
	c.AddFunc("@every 10s", func() { log.WithFields(log.Fields{"cronicle": "core"}).Info("Running...") })
	c.Start() // Stop the scheduler (does not stop any jobs already running).
	runtime.Goexit()
}
