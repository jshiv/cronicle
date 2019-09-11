package cron_test

import (
	"github.com/jshiv/cronicle/internal/config"
	"github.com/jshiv/cronicle/internal/cron"
	log "github.com/sirupsen/logrus"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Cron", func() {
	It("Should return an error if the sides don't make up a triangle", func() {

		got := cron.Dummy("what")
		Expect(got).To(Equal("what"))
	})

	It("Should return an function", func() {
		var conf config.Config

		conf.Schedule.Cron = "@every 2s"
		conf.Schedule.Command = "echo Hello World"

		got := cron.AddSchedule(conf.Schedule)

		Expect(got).To(Equal(func() {
			log.WithFields(log.Fields{"job": "Schedule"}).Info(conf.Schedule.Command)
		}))
	})
})
