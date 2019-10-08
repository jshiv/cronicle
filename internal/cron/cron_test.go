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

	It("Should return an empty bash.Result", func() {
		conf := config.Default()
		task := conf.Schedules[0].Tasks[0]
		task.Command = []string{}
		err := config.SetConfig(&conf, croniclePath)
		fmt.Println(err)
		r, err := cron.ExecuteTask(&task)
		fmt.Println(err)
		g := config.GetGit(croniclePath)
		h, err := g.Repository.Head()
		Expect(h.Name().String()).To(Equal("refs/heads/master"))
		Expect(r).To(Equal(bash.Result{}))
	})

	It("Should fetch and checkout branch feature/test-branch", func() {
		conf := config.Default()
		conf.Schedules[0].Repo = "https://github.com/jshiv/cronicle-sample.git"
		conf.Schedules[0].Tasks[0].Branch = "feature/test-branch"

		config.SetConfig(&conf, croniclePath)
		task := conf.Schedules[0].Tasks[0]
		task.Command = []string{}
		r, err := cron.ExecuteTask(&task)
		fmt.Println(err)
		h, err := task.Git.Repository.Head()
		Expect(h.Name().String()).To(Equal("refs/heads/feature/test-branch"))
		Expect(r).To(Equal(bash.Result{}))
	})
})
