package cron_test

import (
	"fmt"

	"github.com/jshiv/cronicle/internal/bash"
	"github.com/jshiv/cronicle/internal/config"
	"github.com/jshiv/cronicle/internal/cron"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Cron", func() {
	It("Should return an error if the sides don't make up a triangle", func() {

		got := cron.Dummy("what")
		Expect(got).To(Equal("what"))
	})

	It("Should return an function", func() {
		conf := config.Default()
		task := conf.Schedules[0].Tasks[0]
		task.Command = []string{}
		err := config.SetConfig(&conf, croniclePath)
		fmt.Println(err)
		r := cron.ExecuteTask(&task)
		Expect(r).To(Equal(bash.Result{}))
	})
})
