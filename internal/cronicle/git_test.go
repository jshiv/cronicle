package cronicle_test

import (
	"os"
	"path/filepath"

	"github.com/go-git/go-git/v5/plumbing"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/jshiv/cronicle/internal/cronicle"
)

var _ = Describe("git", func() {

	It("cronicle.Clone should fetch and populate the a Git object into task.Path from https://github.com/jshiv/cronicle-sample.git", func() {
		conf := cronicle.Default()
		path, _ := filepath.Abs("./test_clone")
		conf.Schedules[0].Repo = &cronicle.Repo{URL: "https://github.com/jshiv/cronicle-sample.git"}

		conf.PropigateTaskProperties(path)
		task := conf.Schedules[0].Tasks[0]
		auth, err := task.Repo.Auth()
		Expect(err).To(BeNil())
		g, err := cronicle.Clone(task.Path, task.Repo.URL, &auth)
		Expect(err).To(BeNil())
		task.Git = g

		err = task.Git.Checkout(task.Repo.Branch, task.Repo.Commit)
		Expect(err).To(BeNil())

		Expect(task.Path).To(Equal(path + "/.cronicle/repos/jshiv/cronicle-sample.git/foo/bar"))
		Expect(task.Repo.URL).To(Equal("https://github.com/jshiv/cronicle-sample.git"))
		Expect(task.Git.Head.Name()).To(Equal(plumbing.NewBranchReferenceName("master")))
		Expect(cronicle.DirExists(path + "/.cronicle/repos/jshiv/cronicle-sample.git/foo/bar/.git")).To(Equal(true))
		os.RemoveAll(path)
	})

	It("cronicle.Clone should authenticate, fetch and populate the a Git object into task.Path from git@github.com:jshiv/cronicle-sample.git", func() {
		conf := cronicle.Default()
		path, _ := filepath.Abs("./test_ssh_auth")
		conf.Schedules[0].Repo = &cronicle.Repo{URL: "git@github.com:jshiv/cronicle-sample.git", DeployKey: "./test/test_deploy_key"}

		conf.PropigateTaskProperties(path)
		task := conf.Schedules[0].Tasks[0]
		auth, err := task.Repo.Auth()
		Expect(err).To(BeNil())
		g, err := cronicle.Clone(task.Path, task.Repo.URL, &auth)
		Expect(err).To(BeNil())
		task.Git = g

		err = g.Checkout(task.Repo.Branch, task.Repo.Commit)
		Expect(err).To(BeNil())

		Expect(task.Path).To(Equal(path + "/.cronicle/repos/jshiv/cronicle-sample.git/foo/bar"))
		Expect(task.Repo.URL).To(Equal("git@github.com:jshiv/cronicle-sample.git"))
		Expect(task.Git.Head.Name()).To(Equal(plumbing.NewBranchReferenceName("master")))
		Expect(cronicle.DirExists(path + "/.cronicle/repos/jshiv/cronicle-sample.git/foo/bar/.git")).To(Equal(true))
		os.RemoveAll(path)
	})

	It("Git.Open should populate the Git from cloned taskPath from testRepo", func() {
		conf := cronicle.Default()
		conf.Schedules[0].Repo = &cronicle.Repo{}
		conf.Schedules[0].Repo.URL = testRepoPath

		conf.PropigateTaskProperties(taskPath)
		task := conf.Schedules[0].Tasks[0]
		repo := cronicle.Repo{URL: task.Repo.URL, DeployKey: ""}
		auth, err := repo.Auth()
		Expect(err).To(BeNil())
		g, err := cronicle.Clone(task.Path, task.Repo.URL, &auth)
		Expect(err).To(BeNil())
		task.Git = g
		err = task.Git.Checkout(task.Repo.Branch, task.Repo.Commit)
		Expect(err).To(BeNil())
		task.CleanGit()
		cleanGit := cronicle.Git{}
		err = cleanGit.Open(task.Path)
		Expect(err).To(BeNil())

		Expect(g.Head.Name()).To(Equal(plumbing.NewBranchReferenceName("master")))
		Expect(cronicle.DirExists(task.Path)).To(Equal(true))
	})

	It("Git.Open should populate the Git from task.CroniclePath from conf.Remote", func() {
		conf := cronicle.Default()
		// conf.Schedules[0].Repo = testRepoPath
		conf.Repo = &cronicle.Repo{}
		conf.Repo.URL = "https://github.com/jshiv/cronicle-sample.git"

		conf.Init("./cronicle-sample")
		// conf.PropigateTaskProperties("./cronicle-sample/")
		task := conf.Schedules[0].Tasks[0]
		Expect(task.CroniclePath).To(Equal("./cronicle-sample"))
		Expect(task.Path).To(Equal(task.CroniclePath))
		Expect(cronicle.DirExists(task.Path)).To(Equal(true))
		Expect(cronicle.DirExists(filepath.Join(task.Path, ".git"))).To(Equal(true))

		task.CleanGit()
		err := task.Git.Open(task.CroniclePath)
		Expect(err).To(BeNil())

		Expect(task.Git.Head.Name()).To(Equal(plumbing.NewBranchReferenceName("master")))
		// Expect(task.Git.Commit.).To(Equal(""))
		Expect(cronicle.DirExists(task.Path)).To(Equal(true))
		os.RemoveAll("./cronicle-sample")
	})

})
