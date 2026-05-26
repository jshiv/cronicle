package cronicle_test

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-git/go-git/v6/plumbing"
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
		opts, err := task.Repo.Auth()
		Expect(err).To(BeNil())
		g, err := cronicle.Clone(task.Path, task.Repo.URL, opts)
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

	It("cronicle.Clone should authenticate, fetch and populate the a Git object into task.Path from a deploy-key SSH remote", func() {
		conf := cronicle.Default()
		path, _ := filepath.Abs("./test_ssh_auth")
		sshURL := fmt.Sprintf("ssh://git@%s/test_repo", testSSHAddr)
		conf.Schedules[0].Repo = &cronicle.Repo{URL: sshURL, DeployKey: "./test/test_deploy_key"}

		conf.PropigateTaskProperties(path)
		task := conf.Schedules[0].Tasks[0]
		opts, err := task.Repo.Auth()
		Expect(err).To(BeNil())
		g, err := cronicle.Clone(task.Path, task.Repo.URL, opts)
		Expect(err).To(BeNil())
		task.Git = g

		err = g.Checkout(task.Repo.Branch, task.Repo.Commit)
		Expect(err).To(BeNil())

		Expect(task.Path).To(Equal(path + "/.cronicle/repos/test_repo/foo/bar"))
		Expect(task.Repo.URL).To(Equal(sshURL))
		Expect(task.Git.Head.Name()).To(Equal(plumbing.NewBranchReferenceName("master")))
		Expect(cronicle.DirExists(path + "/.cronicle/repos/test_repo/foo/bar/.git")).To(Equal(true))
		os.RemoveAll(path)
	})

	It("Git.Open should populate the Git from cloned taskPath from testRepo", func() {
		conf := cronicle.Default()
		conf.Schedules[0].Repo = &cronicle.Repo{}
		conf.Schedules[0].Repo.URL = testRepoPath

		conf.PropigateTaskProperties(taskPath)
		task := conf.Schedules[0].Tasks[0]
		repo := cronicle.Repo{URL: task.Repo.URL, DeployKey: ""}
		opts, err := repo.Auth()
		Expect(err).To(BeNil())
		g, err := cronicle.Clone(task.Path, task.Repo.URL, opts)
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

	// Regression: pointing DeployKey at a missing file used to segfault
	// inside (*Repo).Auth — ssh.NewPublicKeysFromFile returns (nil, err)
	// and the code touched auth.HostKeyCallback before checking err.
	// Expect a clean error now, not a panic.
	It("Repo.Auth returns a clean error when the deploy key file does not exist", func() {
		repo := cronicle.Repo{
			URL:       "git@github.com:jshiv/cronicle-sample.git",
			DeployKey: "/nonexistent/path/to/ssh_key",
		}
		auth, err := repo.Auth()
		Expect(err).ToNot(BeNil())
		Expect(auth).To(BeNil())
		// Error message should name the offending key path so operators
		// see what to fix.
		Expect(err.Error()).To(ContainSubstring("/nonexistent/path/to/ssh_key"))
	})

})
