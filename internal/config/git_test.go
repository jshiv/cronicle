package config_test

import (
	"github.com/jshiv/cronicle/internal/config"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"gopkg.in/src-d/go-git.v4/plumbing"
)

var _ = Describe("git", func() {

	It("task.Clone() should fetch and populate the task.Git object into taskPath from https://github.com/jshiv/cronicle-sample.git", func() {
		conf := config.Default()
		conf.Schedules[0].Repo = "https://github.com/jshiv/cronicle-sample.git"

		conf.PropigateTaskProperties(taskPath)
		task := conf.Schedules[0].Tasks[0]
		err := task.Clone()
		Expect(err).To(BeNil())

		err = task.Checkout()
		Expect(err).To(BeNil())

		Expect(task.Path).To(Equal(taskPath + "/repos/jshiv/cronicle-sample.git/example/hello"))
		Expect(task.Repo).To(Equal("https://github.com/jshiv/cronicle-sample.git"))
		Expect(task.Git.Head.Name()).To(Equal(plumbing.NewBranchReferenceName("master")))
		Expect(config.DirExists(taskPath + "/repos/jshiv/cronicle-sample.git/example/hello/.git")).To(Equal(true))
	})

	It("Git.Open should populate the Git from cloned taskPath from testRepo", func() {
		conf := config.Default()
		conf.Schedules[0].Repo = testRepoPath

		conf.PropigateTaskProperties(taskPath)
		task := conf.Schedules[0].Tasks[0]
		err := task.Clone()
		task.Checkout()
		Expect(err).To(BeNil())
		task.CleanGit()
		g := config.Git{}
		g.Open(task.Path)

		Expect(g.Head.Name()).To(Equal(plumbing.NewBranchReferenceName("master")))
		Expect(config.DirExists(task.Path)).To(Equal(true))
	})

})