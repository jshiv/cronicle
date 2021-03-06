package cronicle_test

import (
	"errors"

	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/jshiv/cronicle/internal/cronicle"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Config", func() {

	It("Should be configurable from hcl", func() {
		// not testing anything, just an informative dummy
		testConfig := cronicle.Config{
			Repo: &cronicle.Repo{URL: "github.com/myname/schedule1"},
			Schedules: []cronicle.Schedule{
				{
					Name:      "My-Schedule",
					Cron:      "@hourly",
					StartDate: "2019-09-10",
					EndDate:   "2019-09-12",
					Tasks: []cronicle.Task{
						{
							Name: "task1",
						},
						{
							Name:    "task2",
							Command: []string{"/bin/bash", "job.sh"},
							Depends: []string{"task1"},
							Repo:    &cronicle.Repo{URL: "github.com/myname/schedulerepo1"},
						},
					},
				},
			},
		}

		f := hclwrite.NewEmptyFile()
		gohcl.EncodeIntoBody(&testConfig, f.Body())
		Expect(string(f.Bytes())).ToNot(BeNil())
	})

	It("conf.PropigateTaskProperties(./path/) should propigate task properties ScheduleName, Repo, Branch, and Path", func() {
		conf := cronicle.Default()
		conf.Schedules[0].Repo = &cronicle.Repo{}
		conf.Schedules[0].Repo.URL = "https://github.com/jshiv/cronicle-sample.git"
		conf.Schedules[0].Repo.DeployKey = "~/.ssh/id_rsa"

		conf.Schedules[0].Tasks[0].Repo = &cronicle.Repo{}
		conf.Schedules[0].Tasks[0].Repo.Branch = "feature/test-branch"

		conf.PropigateTaskProperties("./path/")
		Expect(conf.Schedules[0].Tasks[0].ScheduleName).To(Equal("foo"))
		Expect(conf.Schedules[0].Tasks[0].Repo.URL).To(Equal("https://github.com/jshiv/cronicle-sample.git"))
		Expect(conf.Schedules[0].Tasks[0].Repo.DeployKey).To(Equal("~/.ssh/id_rsa"))
		Expect(conf.Schedules[0].Tasks[0].Repo.Branch).To(Equal("feature/test-branch"))
		Expect(conf.Schedules[0].Tasks[0].Path).To(Equal("path/.cronicle/repos/jshiv/cronicle-sample.git/foo/bar"))

	})

	It("conf.PropigateTaskProperties(./path/) should propigate config properties croniclePath and cronicleRepo", func() {
		conf := cronicle.Default()
		conf.Repo = &cronicle.Repo{}
		conf.Repo.URL = "https://github.com/jshiv/cronicle-sample.git"

		conf.PropigateTaskProperties("./path/")
		Expect(conf.Schedules[0].Tasks[0].CronicleRepo.URL).To(Equal("https://github.com/jshiv/cronicle-sample.git"))
		Expect(conf.Schedules[0].Tasks[0].Path).To(Equal("./path/"))

	})

	It("conf.PropigateTaskProperties(./path/) should propigate config Location", func() {
		conf := cronicle.Default()
		conf.Timezone = "America/New_York"

		conf.PropigateTaskProperties("./path/")
		Expect(conf.Schedules[0].Timezone).To(Equal("America/New_York"))

	})

	It("conf.PropigateTaskProperties(./path/) should propigate config Location", func() {
		conf := cronicle.Default()
		conf.Timezone = "Not_a_Timezone"

		err := conf.Validate()
		Expect(err).To(Equal(errors.New("unknown time zone Not_a_Timezone")))

	})

	It("conf.Validate() should error if two schedules have the same name", func() {
		conf := cronicle.Default()
		conf.Schedules = append(conf.Schedules, cronicle.Default().Schedules[0])

		err := conf.Validate()
		Expect(err).To(Equal(errors.New("schedule \"foo\" {} is listed 2 times, please change the name")))

	})

	It("conf.PropigateTaskProperties(./path/) should propigate config Location and not overwrite given schedule.Timezone", func() {
		conf := cronicle.Default()
		conf.Timezone = "America/New_York"
		conf.Schedules[0].Timezone = "Asia/Tokyo"

		conf.PropigateTaskProperties("./path/")
		Expect(conf.Timezone).To(Equal("America/New_York"))
		Expect(conf.Schedules[0].Timezone).To(Equal("Asia/Tokyo"))

	})

	It("Should return an TaskArray", func() {
		conf := cronicle.Default()
		conf.Schedules[0].Tasks = append(conf.Schedules[0].Tasks, cronicle.Task{Name: "task2"})
		tasks := conf.TaskArray()
		Expect(len(tasks)).To(Equal(2))
		task := tasks[0]
		Expect(task.Name).To(Equal("bar"))
	})

	It("Should FilterTask to all tasks if taskName is empty and scheduleName is empty", func() {
		conf := cronicle.Default()
		conf.Schedules[0].Tasks = append(conf.Schedules[0].Tasks, cronicle.Task{Name: "task2"})
		tasks := conf.TaskArray().FilterTasks("", "")
		Expect(len(tasks)).To(Equal(2))
		task := tasks[1]
		Expect(task.Name).To(Equal("task2"))
	})

	It("Should FilterTask to task2 if taskName = task2 and scheduleName is empty", func() {
		conf := cronicle.Default()
		conf.Schedules[0].Tasks = append(conf.Schedules[0].Tasks, cronicle.Task{Name: "task2"})
		tasks := conf.TaskArray().FilterTasks("task2", "")
		Expect(len(tasks)).To(Equal(1))
		task := tasks[0]
		Expect(task.Name).To(Equal("task2"))
	})

	It("Should FilterTask to both if taskName = hello and scheduleName = example", func() {
		conf := cronicle.Default()
		conf.Schedules[0].Tasks = append(conf.Schedules[0].Tasks, cronicle.Task{Name: "task2"})
		conf.PropigateTaskProperties("./path")
		tasks := conf.TaskArray().FilterTasks("bar", "foo")
		Expect(len(tasks)).To(Equal(1))
		task := tasks[0]
		Expect(task.Name).To(Equal("bar"))
	})

	It("Should FilterTask to none if taskName = hello and scheduleName = ex", func() {
		conf := cronicle.Default()
		conf.Schedules[0].Tasks = append(conf.Schedules[0].Tasks, cronicle.Task{Name: "task2"})
		tasks := conf.TaskArray().FilterTasks("bar", "fo")
		Expect(len(tasks)).To(Equal(0))
	})

	It("task.Validate() should return nil", func() {
		conf := cronicle.Default()
		conf.PropigateTaskProperties("./path")
		task := conf.Schedules[0].Tasks[0]
		err := task.Validate()
		Expect(err).To(BeNil())
	})

	It("task.Validate() should return ErrBranchAndCommitGiven if branch and commit are given", func() {
		conf := cronicle.Default()
		conf.PropigateTaskProperties("./path")
		task := conf.Schedules[0].Tasks[0]
		repo := cronicle.Repo{}
		task.Repo = &repo
		task.Repo.Branch = "feature/test-branch"
		task.Repo.Commit = "8e9f30a6c3598203c73c0fd393081d2e84961da9"
		err := task.Validate()
		Expect(err).To(Equal(cronicle.ErrBranchAndCommitGiven))
	})

	It("task.Validate() should return ErrIfRepoGivenAndPathNotGiven if repo is given and path is not given", func() {
		conf := cronicle.Default()
		// conf.PropigateTaskProperties("./path")
		task := conf.Schedules[0].Tasks[0]
		repo := cronicle.Repo{URL: "https://github.com/jshiv/cronicle-sample.git"}
		task.Repo = &repo
		err := task.Validate()
		Expect(err).To(Equal(cronicle.ErrIfRepoGivenAndPathNotGiven))
	})

	It("task.Validate() should return nil if repo is given and path is given via PropigateTaskProperties", func() {
		conf := cronicle.Default()
		conf.PropigateTaskProperties("./path")
		task := conf.Schedules[0].Tasks[0]
		repo := cronicle.Repo{URL: "https://github.com/jshiv/cronicle-sample.git"}
		task.Repo = &repo
		err := task.Validate()
		Expect(err).To(BeNil())
	})

	It("task.Validate() should return nil if path is given and repo is not given", func() {
		conf := cronicle.Default()
		task := conf.Schedules[0].Tasks[0]
		task.Path = "./path/"
		err := task.Validate()
		Expect(err).To(BeNil())
	})

	It("schedule.TaskMap() should return a map of taskName,Task", func() {
		conf := cronicle.Default()
		schedule := conf.Schedules[0]
		taskMap := schedule.TaskMap()
		err := conf.Validate()
		Expect(err).To(BeNil())
		task := taskMap["bar"]
		Expect(task.Name).To(Equal("bar"))
	})

	It("config.ScheduleMap() should return a map of scheduleName,Schedule", func() {
		conf := cronicle.Default()
		scheduleMap := conf.ScheduleMap()
		schedule := scheduleMap["foo"]
		taskMap := schedule.TaskMap()
		task := taskMap["bar"]
		Expect(schedule.Name).To(Equal("foo"))
		Expect(task.Name).To(Equal("bar"))
	})
})
