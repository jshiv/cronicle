package cronicle_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/jshiv/cronicle/internal/cronicle"
	config "github.com/jshiv/cronicle/internal/cronicle"
)

var _ = Describe("Init", func() {
	Describe("Setting Config given a basic config", func() {
		Context("init.SetConfig", func() {
			var conf config.Config
			JustBeforeEach(func() {
				conf = config.Default()
			})
			It("should populate Task.Path with the croniclePath", func() {
				// err := config.SetConfig(&conf, croniclePath)
				err := conf.Init(croniclePath)
				Expect(err).To(BeNil())
				Expect(conf.Schedules[0].Tasks[0].Path).To(Equal(croniclePath))
			})
			It("should clone a sub repo from https://github.com/jshiv/cronicle-sample.git", func() {
				conf.Schedules[0].Repo = &cronicle.Repo{}
				conf.Schedules[0].Repo.URL = "https://github.com/jshiv/cronicle-sample.git"
				err := conf.Init(croniclePath)

				Expect(err).To(BeNil())
				Expect(conf.Schedules[0].Tasks[0].Path).To(Equal(croniclePath + "/.cronicle/repos/jshiv/cronicle-sample.git/foo/bar"))
				Expect(conf.Schedules[0].Tasks[0].Repo.URL).To(Equal("https://github.com/jshiv/cronicle-sample.git"))
				Expect(config.DirExists(croniclePath + "/.cronicle/repos/jshiv/cronicle-sample.git/foo/bar/.git")).To(Equal(true))
				repo := cronicle.Repo{URL: conf.Schedules[0].Tasks[0].Repo.URL, DeployKey: ""}
				auth, err := repo.Auth()
				Expect(err).To(BeNil())
				g, err := cronicle.Clone(conf.Schedules[0].Tasks[0].Path, conf.Schedules[0].Tasks[0].Repo.URL, &auth)
				// g, err := cronicle.Clone(conf.Schedules[0].Tasks[0].Path, conf.Schedules[0].Tasks[0].Repo)
				Expect(err).To(BeNil())
				Expect(g.Head.Name()).To(Equal(plumbing.NewBranchReferenceName("master")))

			})
			It("should fail if a branch and commit are given from https://github.com/jshiv/cronicle-sample.git", func() {
				conf.Schedules[0].Repo = &cronicle.Repo{}
				conf.Schedules[0].Repo.URL = "https://github.com/jshiv/cronicle-sample.git"
				conf.Schedules[0].Tasks[0].Repo = &cronicle.Repo{}
				conf.Schedules[0].Tasks[0].Repo.Branch = "feature/test-branch"
				conf.Schedules[0].Tasks[0].Repo.Commit = "8e9f30a6c3598203c73c0fd393081d2e84961da9"

				err := conf.Init(croniclePath)
				Expect(err).To(Equal(cronicle.ErrBranchAndCommitGiven))

			})

			It("conf.Init should authenticate, clone and checkout from git@github.com:jshiv/cronicle-sample.git", func() {
				conf := cronicle.Default()
				conf.Repo = &cronicle.Repo{URL: "git@github.com:jshiv/cronicle-sample.git", DeployKey: "./test/test_deploy_key", Branch: "feature/test-branch"}
				path, _ := filepath.Abs("./test_conf_init_ssh_auth")

				err := conf.Init(path)
				Expect(err).To(BeNil())

				g := cronicle.Git{}
				err = g.Open(path)
				Expect(err).To(BeNil())

				task := conf.Schedules[0].Tasks[0]

				Expect(task.Path).To(Equal(path))
				// Expect(task.CroniclePath).To(Equal(path))
				Expect(task.Repo).To(BeNil())
				Expect(g.Head.Name()).To(Equal(plumbing.NewBranchReferenceName("feature/test-branch")))
				Expect(cronicle.DirExists(path + "/.git")).To(Equal(true))
				os.RemoveAll(path)
			})
		})
	})

	Describe("Get Repos from Config.Repos", func() {
		Context("config.GetRepos", func() {
			It("should be https://github.com/jshiv/cronicle-sample.git", func() {
				conf := config.Default()
				conf.Repos = []string{"https://github.com/jshiv/cronicle-sample.git"}
				repos := config.GetRepos(&conf)
				expected := map[string]bool{
					"https://github.com/jshiv/cronicle-sample.git": true,
				}
				Expect(repos).To(Equal(expected))
			})
		})
	})

	Describe("Get Repos from Config.Schedules[0].Repo", func() {
		Context("config.GetRepos", func() {
			It("should be https://github.com/jshiv/cronicle-sample.git", func() {
				conf := config.Default()
				conf.Schedules[0].Repo = &cronicle.Repo{}
				conf.Schedules[0].Repo.URL = "https://github.com/jshiv/cronicle-sample.git"
				repos := config.GetRepos(&conf)
				expected := map[string]bool{
					"https://github.com/jshiv/cronicle-sample.git": true,
				}
				Expect(repos).To(Equal(expected))
			})
		})
	})

	Describe("Get Repos from Config.Schedules[0].Tasks[0].Repo", func() {
		Context("config.GetRepos", func() {
			It("should be https://github.com/jshiv/cronicle-sample.git", func() {
				conf := config.Default()
				conf.Schedules[0].Tasks[0].Repo = &cronicle.Repo{}
				conf.Schedules[0].Tasks[0].Repo.URL = "https://github.com/jshiv/cronicle-sample.git"
				repos := config.GetRepos(&conf)
				expected := map[string]bool{
					"https://github.com/jshiv/cronicle-sample.git": true,
				}
				Expect(repos).To(Equal(expected))
			})
		})
	})

	Describe("get the same repo from Config, Schedules and Tasks", func() {
		Context("config.GetRepos", func() {
			It("should be https://github.com/jshiv/cronicle-sample.git", func() {
				conf := config.Default()
				conf.Repos = []string{"https://github.com/jshiv/cronicle-sample.git"}
				conf.Schedules[0].Repo = &cronicle.Repo{}
				conf.Schedules[0].Repo.URL = "https://github.com/jshiv/cronicle-sample.git"
				conf.Schedules[0].Tasks[0].Repo = &cronicle.Repo{}
				conf.Schedules[0].Tasks[0].Repo.URL = "https://github.com/jshiv/cronicle-sample.git"
				repos := config.GetRepos(&conf)
				expected := map[string]bool{
					"https://github.com/jshiv/cronicle-sample.git": true,
				}
				Expect(repos).To(Equal(expected))
			})
		})
	})

	Describe("get different repos from Config, Schedules and Tasks", func() {
		Context("config.GetRepos", func() {
			It("should be https://github.com/jshiv/cronicle-sample.git", func() {
				conf := config.Default()
				conf.Repos = []string{"https://github.com/jshiv/cronicle-sample.git"}
				conf.Schedules[0].Repo = &cronicle.Repo{}
				conf.Schedules[0].Repo.URL = "https://github.com/jshiv/cronicle-sample1.git"
				conf.Schedules[0].Tasks[0].Repo = &cronicle.Repo{}
				conf.Schedules[0].Tasks[0].Repo.URL = "https://github.com/jshiv/cronicle-sample2.git"
				repos := config.GetRepos(&conf)
				expected := map[string]bool{
					"https://github.com/jshiv/cronicle-sample.git":  true,
					"https://github.com/jshiv/cronicle-sample1.git": true,
					"https://github.com/jshiv/cronicle-sample2.git": true,
				}
				Expect(repos).To(Equal(expected))
			})
		})
	})

	Describe("LocalRepoDir should get path from https, git and local path urls", func() {
		Context("cronicle.LocalRepoDir", func() {
			It("should be https://github.com/jshiv/cronicle-sample.git", func() {
				path, err := cronicle.LocalRepoDir("./cronicle/", "https://github.com/jshiv/cronicle-sample.git")
				Expect(err).To(BeNil())
				Expect(path).To(Equal("cronicle/.cronicle/repos/jshiv/cronicle-sample.git"))
			})

			It("should be git@github.com:jshiv/cronicle-sample.git", func() {
				path, err := cronicle.LocalRepoDir("./cronicle/", "git@github.com:jshiv/cronicle-sample.git")
				Expect(err).To(BeNil())
				Expect(path).To(Equal("cronicle/.cronicle/repos/jshiv/cronicle-sample.git"))
			})

			It("should be ./jshiv/cronicle-sample.git", func() {
				path, err := cronicle.LocalRepoDir("./cronicle/", "./jshiv/cronicle-sample.git")
				Expect(err).To(BeNil())
				Expect(path).To(Equal("cronicle/.cronicle/repos/jshiv/cronicle-sample.git"))
			})
		})
	})

})
