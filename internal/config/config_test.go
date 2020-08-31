package config_test

import (
	"fmt"

	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclwrite"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/jshiv/cronicle/internal/config"
)

var _ = Describe("Config", func() {

	It("Should be configurable from hcl", func() {
		// not testing anything, just an informative dummy
		testConfig := config.Config{
			Version: "0.1",
			Git:     "github.com/myname/schedule1",
			Schedules: []config.Schedule{
				{
					Name:      "My-Schedule",
					Cron:      "@hourly",
					StartDate: "2019-09-10",
					EndDate:   "2019-09-12",
					Owner: &config.Owner{
						Name:  "bob",
						Email: "bob@email.com",
					},
					Tasks: []config.Task{
						{
							Name: "task1",
							Owner: &config.Owner{
								Name:  "bobby",
								Email: "bobby@email.com",
							},
						},
						{
							Name:    "task2",
							Command: []string{"/bin/bash", "job.sh"},
							Depends: []string{"task1"},
							Repo:    "github.com/myname/schedulerepo1",
						},
					},
				},
			},
		}

		f := hclwrite.NewEmptyFile()
		gohcl.EncodeIntoBody(&testConfig, f.Body())
		// fmt.Printf(string(f.Bytes()))
		Expect(string(f.Bytes())).ToNot(BeNil())
	})

	It("conf.PropigateTaskProperties(./path/) should propigate task properties ScheduleName, Repo, Branch, and Path", func() {
		conf := config.Default()
		conf.Schedules[0].Repo = "https://github.com/jshiv/cronicle-sample.git"
		conf.Schedules[0].Tasks[0].Branch = "feature/test-branch"

		conf.PropigateTaskProperties("./path/")
		Expect(conf.Schedules[0].Tasks[0].ScheduleName).To(Equal("example"))
		Expect(conf.Schedules[0].Tasks[0].Repo).To(Equal("https://github.com/jshiv/cronicle-sample.git"))
		Expect(conf.Schedules[0].Tasks[0].Branch).To(Equal("feature/test-branch"))
		Expect(conf.Schedules[0].Tasks[0].Path).To(Equal("path/repos/jshiv/cronicle-sample.git/example/hello"))

	})

	It("Should return an TaskArray", func() {
		conf := config.Default()
		conf.Schedules[0].Tasks = append(conf.Schedules[0].Tasks, config.Task{Name: "task2"})
		tasks := conf.TaskArray()
		fmt.Println(len(tasks))
		Expect(len(tasks)).To(Equal(2))
		task := tasks[0]
		Expect(task.Name).To(Equal("hello"))
	})

	It("Should FilterTask to all tasks if taskName is empty and scheduleName is empty", func() {
		conf := config.Default()
		conf.Schedules[0].Tasks = append(conf.Schedules[0].Tasks, config.Task{Name: "task2"})
		tasks := conf.TaskArray().FilterTasks("", "")
		fmt.Println(len(tasks))
		Expect(len(tasks)).To(Equal(2))
		task := tasks[1]
		Expect(task.Name).To(Equal("task2"))
	})

	It("Should FilterTask to task2 if taskName = task2 and scheduleName is empty", func() {
		conf := config.Default()
		conf.Schedules[0].Tasks = append(conf.Schedules[0].Tasks, config.Task{Name: "task2"})
		tasks := conf.TaskArray().FilterTasks("task2", "")
		fmt.Println(len(tasks))
		Expect(len(tasks)).To(Equal(1))
		task := tasks[0]
		Expect(task.Name).To(Equal("task2"))
	})

	It("Should FilterTask to both if taskName = hello and scheduleName = example", func() {
		conf := config.Default()
		conf.Schedules[0].Tasks = append(conf.Schedules[0].Tasks, config.Task{Name: "task2"})
		conf.PropigateTaskProperties("./path")
		tasks := conf.TaskArray().FilterTasks("hello", "example")
		fmt.Println(len(tasks))
		Expect(len(tasks)).To(Equal(1))
		task := tasks[0]
		Expect(task.Name).To(Equal("hello"))
	})

	It("Should FilterTask to none if taskName = hello and scheduleName = ex", func() {
		conf := config.Default()
		conf.Schedules[0].Tasks = append(conf.Schedules[0].Tasks, config.Task{Name: "task2"})
		tasks := conf.TaskArray().FilterTasks("hello", "ex")
		fmt.Println(len(tasks))
		Expect(len(tasks)).To(Equal(0))
	})
})
