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

	It("Should fetch and checkout local commit 699b2794b2b0f6ddfe8a0fe386e6013eeeec1ad1", func() {
		conf := config.Default()

		conf.Schedules[0].Repo = testRepoPath
		conf.Schedules[0].Tasks[0].Commit = "699b2794b2b0f6ddfe8a0fe386e6013eeeec1ad1"

		config.SetConfig(&conf, croniclePath)
		task := conf.Schedules[0].Tasks[0]
		task.Command = []string{"python", "test.py"}
		r, err := cron.ExecuteTask(&task)
		fmt.Println(err)
		c := task.Git.Commit
		Expect(c.Hash.String()).To(Equal("699b2794b2b0f6ddfe8a0fe386e6013eeeec1ad1"))
		Expect(r).To(Equal(bash.Result{
			Command:    []string{"python", "test.py"},
			Stdout:     "test specific commit: SUCCESS\n",
			Stderr:     "",
			ExitStatus: 0,
		}))
	})

	It("Should fetch and checkout local branch test/checkout_specific_branch", func() {
		conf := config.Default()
		conf.Schedules[0].Repo = testRepoPath
		conf.Schedules[0].Tasks[0].Branch = "test/checkout_specific_branch"

		config.SetConfig(&conf, croniclePath)
		task := conf.Schedules[0].Tasks[0]
		task.Command = []string{"python", "test.py"}
		r, err := cron.ExecuteTask(&task)
		fmt.Println(err)
		h, err := task.Git.Repository.Head()
		Expect(h.Name().String()).To(Equal("refs/heads/test/checkout_specific_branch"))
		Expect(r).To(Equal(bash.Result{
			Command:    []string{"python", "test.py"},
			Stdout:     "test specific branch: SUCCESS\n",
			Stderr:     "",
			ExitStatus: 0,
		}))
	})

	It("Should fetch and checkout local master by default", func() {
		conf := config.Default()
		conf.Schedules[0].Repo = testRepoPath
		// conf.Schedules[0].Tasks[0].Branch = "test/checkout_specific_branch"

		config.SetConfig(&conf, croniclePath)
		task := conf.Schedules[0].Tasks[0]
		task.Command = []string{"python", "test.py"}
		r, err := cron.ExecuteTask(&task)
		fmt.Println(err)
		h, err := task.Git.Repository.Head()
		Expect(h.Name().String()).To(Equal("refs/heads/master"))
		Expect(r).To(Equal(bash.Result{
			Command:    []string{"python", "test.py"},
			Stdout:     "test master: SUCCESS\n",
			Stderr:     "",
			ExitStatus: 0,
		}))
	})
})
