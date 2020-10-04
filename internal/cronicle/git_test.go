package cronicle_test

import (
	"path/filepath"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"gopkg.in/src-d/go-git.v4/plumbing"

	"github.com/jshiv/cronicle/internal/cronicle"
)

var _ = Describe("git", func() {

	It("cronicle.Clone should fetch and populate the a Git object into task.Path from https://github.com/jshiv/cronicle-sample.git", func() {
		conf := cronicle.Default()
		conf.Schedules[0].Repo = "https://github.com/jshiv/cronicle-sample.git"

		conf.PropigateTaskProperties(taskPath)
		task := conf.Schedules[0].Tasks[0]
		// err := task.Clone()
		g, err := cronicle.Clone(task.Path, task.Repo)
		Expect(err).To(BeNil())
		task.Git = g

		err = task.Git.Checkout(task.Branch, task.Commit)
		Expect(err).To(BeNil())

		Expect(task.Path).To(Equal(taskPath + "/.repos/jshiv/cronicle-sample.git/example/hello"))
		Expect(task.Repo).To(Equal("https://github.com/jshiv/cronicle-sample.git"))
		Expect(task.Git.Head.Name()).To(Equal(plumbing.NewBranchReferenceName("master")))
		Expect(cronicle.DirExists(taskPath + "/.repos/jshiv/cronicle-sample.git/example/hello/.git")).To(Equal(true))
	})

	It("Git.Open should populate the Git from cloned taskPath from testRepo", func() {
		conf := cronicle.Default()
		conf.Schedules[0].Repo = testRepoPath

		conf.PropigateTaskProperties(taskPath)
		task := conf.Schedules[0].Tasks[0]
		g, err := cronicle.Clone(task.Path, task.Repo)
		Expect(err).To(BeNil())
		task.Git = g
		err = task.Git.Checkout(task.Branch, task.Commit)
		Expect(err).To(BeNil())
		task.CleanGit()
		cleanGit := cronicle.Git{}
		err = cleanGit.Open(task.Path)
		Expect(err).To(BeNil())

		Expect(g.Head.Name()).To(Equal(plumbing.NewBranchReferenceName("master")))
		Expect(cronicle.DirExists(task.Path)).To(Equal(true))
	})

	It("Git.Open should populate the Git from task.CroniclePath from conf.Git", func() {
		conf := cronicle.Default()
		// conf.Schedules[0].Repo = testRepoPath
		conf.Git = "https://github.com/jshiv/cronicle-sample.git"

		conf.Init("./cronicle-sample")
		// conf.PropigateTaskProperties("./cronicle-sample/")
		task := conf.Schedules[0].Tasks[0]
		Expect(task.CroniclePath).To(Equal("./cronicle-sample"))
		Expect(task.Path).To(Equal(task.CroniclePath))
		Expect(cronicle.DirExists(task.Path)).To(Equal(true))
		Expect(cronicle.DirExists(filepath.Join(task.Path, ".git"))).To(Equal(true))

		// g, err := cronicle.Clone(task.CroniclePath, task.CronicleRepo)
		// Expect(err).To(BeNil())
		// task.Git = g
		// err := task.Git.Checkout("", "")
		// Expect(err).To(BeNil())
		task.CleanGit()
		err := task.Git.Open(task.CroniclePath)
		Expect(err).To(BeNil())

		Expect(task.Git.Head.Name()).To(Equal(plumbing.NewBranchReferenceName("master")))
		// Expect(task.Git.Commit.).To(Equal(""))
		Expect(cronicle.DirExists(task.Path)).To(Equal(true))
	})

})
