package cron_test

import (
	"fmt"
	"time"

	"github.com/jshiv/cronicle/internal/bash"
	"github.com/jshiv/cronicle/internal/config"
	"github.com/jshiv/cronicle/internal/cron"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Cron", func() {

	It("Should return an empty bash.Result", func() {
		conf := config.Default()
		task := conf.Schedules[0].Tasks[0]
		task.Command = []string{}
		err := config.SetConfig(&conf, croniclePath)
		t, _ := time.Parse(time.RFC3339, "2020-11-01T22:08:41+00:00")
		r, err := cron.ExecuteTask(&task, t)
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
		t, _ := time.Parse(time.RFC3339, "2020-11-01T22:08:41+00:00")
		r, err := cron.ExecuteTask(&task, t)
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
		t, _ := time.Parse(time.RFC3339, "2020-11-01T22:08:41+00:00")
		r, err := cron.ExecuteTask(&task, t)
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
		t, _ := time.Parse(time.RFC3339, "2020-11-01T22:08:41+00:00")
		r, err := cron.ExecuteTask(&task, t)
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
		t, _ := time.Parse(time.RFC3339, "2020-11-01T22:08:41+00:00")
		r, err := cron.ExecuteTask(&task, t)
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

	It("Should replace ${date}, ${datetime}, and ${timestamp} bash arguments with 2020-11-01T22:08:41+00:00", func() {
		conf := config.Default()
		conf.Schedules[0].Repo = testRepoPath
		// conf.Schedules[0].Tasks[0].Branch = "test/checkout_specific_branch"

		config.SetConfig(&conf, croniclePath)
		task := conf.Schedules[0].Tasks[0]
		task.Command = []string{"/bin/echo", "${date}", "${datetime}", "${timestamp}"}
		t, _ := time.Parse(time.RFC3339, "2020-11-01T22:08:41+00:00")
		r, err := cron.ExecuteTask(&task, t)
		fmt.Println(err)
		h, err := task.Git.Repository.Head()
		Expect(h.Name().String()).To(Equal("refs/heads/master"))
		Expect(r).To(Equal(bash.Result{
			Command: []string{
				"/bin/echo",
				"2020-11-01",
				"2020-11-01T22:08:41Z",
				"2020-11-01 22:08:41Z",
			},
			Stdout:     "2020-11-01 2020-11-01T22:08:41Z 2020-11-01 22:08:41Z\n",
			Stderr:     "",
			ExitStatus: 0,
		}))
	})

	It("Should replace duplicate values of ${date} bash arguments with 2020-11-01", func() {
		conf := config.Default()
		conf.Schedules[0].Repo = testRepoPath
		// conf.Schedules[0].Tasks[0].Branch = "test/checkout_specific_branch"

		config.SetConfig(&conf, croniclePath)
		task := conf.Schedules[0].Tasks[0]
		task.Command = []string{
			"/bin/echo",
			"${date}, ${date}",
			"${datetime}, ${datetime}",
			"${timestamp}, ${timestamp}"}
		t, _ := time.Parse(time.RFC3339, "2020-11-01T22:08:41+00:00")
		r, err := cron.ExecuteTask(&task, t)
		fmt.Println(err)
		h, err := task.Git.Repository.Head()
		Expect(h.Name().String()).To(Equal("refs/heads/master"))
		Expect(r).To(Equal(bash.Result{
			Command: []string{
				"/bin/echo",
				"2020-11-01, 2020-11-01",
				"2020-11-01T22:08:41Z, 2020-11-01T22:08:41Z",
				"2020-11-01 22:08:41Z, 2020-11-01 22:08:41Z",
			},
			Stdout:     "2020-11-01, 2020-11-01 2020-11-01T22:08:41Z, 2020-11-01T22:08:41Z 2020-11-01 22:08:41Z, 2020-11-01 22:08:41Z\n",
			Stderr:     "",
			ExitStatus: 0,
		}))
	})
})
