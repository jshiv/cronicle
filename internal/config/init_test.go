package config_test

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/jshiv/cronicle/internal/config"
)

var _ = Describe("Init", func() {
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
				conf.Schedules[0].Repo = "https://github.com/jshiv/cronicle-sample.git"
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
				conf.Schedules[0].Tasks[0].Repo = "https://github.com/jshiv/cronicle-sample.git"
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
				conf.Schedules[0].Repo = "https://github.com/jshiv/cronicle-sample.git"
				conf.Schedules[0].Tasks[0].Repo = "https://github.com/jshiv/cronicle-sample.git"
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
				conf.Schedules[0].Repo = "https://github.com/jshiv/cronicle-sample1.git"
				conf.Schedules[0].Tasks[0].Repo = "https://github.com/jshiv/cronicle-sample2.git"
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
})
