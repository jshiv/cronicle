package cronicle_test

import (
	"time"

	"fmt"

	"github.com/jshiv/cronicle/internal/cronicle"
	"github.com/jshiv/cronicle/pkg/exec"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Exec", func() {

	It("task should execute with no repo given in a non .git path", func() {
		conf := cronicle.Default()
		schedule := conf.Schedules[0]
		schedule.PropigateTaskProperties(taskPath)
		task := schedule.Tasks[0]

		task.Command = []string{}
		t, _ := time.Parse(time.RFC3339, "2020-11-01T22:08:41+00:00")
		r, err := task.Execute(t)
		Expect(err).To(BeNil())
		Expect(task.Repo).To(BeNil())
		Expect(task.Git.Repository).To(BeNil())
		Expect(r).To(Equal(exec.Result{}))
	})

	It("task should execute with no repo given in in a .git path", func() {
		conf := cronicle.Default()
		schedule := conf.Schedules[0]
		schedule.PropigateTaskProperties(croniclePath)
		task := schedule.Tasks[0]

		task.Command = []string{}
		t, _ := time.Parse(time.RFC3339, "2020-11-01T22:08:41+00:00")
		r, err := task.Execute(t)
		Expect(err).To(BeNil())
		Expect(task.Repo).To(BeNil())
		Expect(task.Git.Repository).To(BeNil())
		Expect(r).To(Equal(exec.Result{}))
	})

	It("Should fetch and checkout branch feature/test-branch and Should return an empty exec.Result", func() {
		conf := cronicle.Default()
		schedule := conf.Schedules[0]
		repo := cronicle.Repo{URL: "https://github.com/jshiv/cronicle-sample.git"}
		schedule.Repo = &repo
		schedule.PropigateTaskProperties(taskPath)
		task := schedule.Tasks[0]
		task.Repo.Branch = "feature/test-branch"

		task.Command = []string{}
		t, _ := time.Parse(time.RFC3339, "2020-11-01T22:08:41+00:00")
		r, err := task.Execute(t)
		Expect(err).To(BeNil())
		h, _ := task.Git.Repository.Head()
		Expect(h.Name().String()).To(Equal("refs/heads/feature/test-branch"))
		Expect(r).To(Equal(exec.Result{}))
	})

	It("Should fetch and checkout local commit 699b2794b2b0f6ddfe8a0fe386e6013eeeec1ad1", func() {
		conf := cronicle.Default()
		schedule := conf.Schedules[0]
		repo := cronicle.Repo{URL: testRepoPath}
		schedule.Repo = &repo
		schedule.PropigateTaskProperties(taskPath)
		task := schedule.Tasks[0]
		task.Repo.Commit = "699b2794b2b0f6ddfe8a0fe386e6013eeeec1ad1"

		task.Command = []string{"python", "test.py"}
		t, _ := time.Parse(time.RFC3339, "2020-11-01T22:08:41+00:00")
		r, err := task.Execute(t)
		Expect(err).To(BeNil())
		c := task.Git.Commit
		Expect(c.Hash.String()).To(Equal("699b2794b2b0f6ddfe8a0fe386e6013eeeec1ad1"))
		Expect(r).To(Equal(exec.Result{
			Command:    []string{"python", "test.py"},
			Stdout:     "test specific commit: SUCCESS\n",
			Stderr:     "",
			ExitStatus: 0,
		}))
	})

	It("Should fetch and checkout local branch test/checkout_specific_branch", func() {
		conf := cronicle.Default()
		schedule := conf.Schedules[0]
		repo := cronicle.Repo{}
		schedule.Repo = &repo
		schedule.Repo.URL = testRepoPath
		schedule.PropigateTaskProperties(taskPath)
		task := schedule.Tasks[0]
		task.Repo.Branch = "test/checkout_specific_branch"

		task.Command = []string{"python", "test.py"}
		t, _ := time.Parse(time.RFC3339, "2020-11-01T22:08:41+00:00")
		r, err := task.Execute(t)
		Expect(err).To(BeNil())
		h, err := task.Git.Repository.Head()
		Expect(h.Name().String()).To(Equal("refs/heads/test/checkout_specific_branch"))
		Expect(r).To(Equal(exec.Result{
			Command:    []string{"python", "test.py"},
			Stdout:     "test specific branch: SUCCESS\n",
			Stderr:     "",
			ExitStatus: 0,
		}))
	})

	It("Should fetch and checkout local master by default", func() {

		conf := cronicle.Default()
		schedule := conf.Schedules[0]
		repo := cronicle.Repo{}
		schedule.Repo = &repo
		schedule.Repo.URL = testRepoPath
		schedule.PropigateTaskProperties(taskPath)
		task := schedule.Tasks[0]

		task.Command = []string{"python", "test.py"}
		t, _ := time.Parse(time.RFC3339, "2020-11-01T22:08:41+00:00")
		r, err := task.Execute(t)
		Expect(err).To(BeNil())
		h, err := task.Git.Repository.Head()
		Expect(h.Name().String()).To(Equal("refs/heads/master"))
		Expect(r).To(Equal(exec.Result{
			Command:    []string{"python", "test.py"},
			Stdout:     "test master: SUCCESS\n",
			Stderr:     "",
			ExitStatus: 0,
		}))
	})

	It("task.Exec(t) Should replace ${date}, ${datetime}, and ${timestamp} bash arguments with 2020-11-01T22:08:41+00:00", func() {
		conf := cronicle.Default()
		schedule := conf.Schedules[0]

		task := schedule.Tasks[0]
		task.Command = []string{"/bin/echo", "${date}", "${datetime}", "${timestamp}"}
		t, _ := time.Parse(time.RFC3339, "2020-11-01T22:08:41+00:00")
		r := task.Exec(t)

		Expect(r).To(Equal(exec.Result{
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

	It("task.Exec(t) Should replace ${path} with task.Path", func() {
		conf := cronicle.Default()
		schedule := conf.Schedules[0]
		repo := cronicle.Repo{}
		schedule.Repo = &repo
		schedule.Repo.URL = testRepoPath
		schedule.PropigateTaskProperties(taskPath)
		task := schedule.Tasks[0]

		task.Command = []string{"/bin/echo", "${path}"}
		t, _ := time.Parse(time.RFC3339, "2020-11-01T22:08:41+00:00")
		r := task.Exec(t)

		fmt.Println(r.Stdout)
		Expect(r.Stderr).To(Equal(""))
		Expect(r.ExitStatus).To(Equal(0))
		Expect(r.Stdout).To(ContainSubstring("cronicle/test_task/.cronicle/"))
		Expect(r.Command[1]).To(ContainSubstring("cronicle/internal/cronicle/test_repo/.git/foo/bar"))
	})

	It("Should replace duplicate values of ${date} bash arguments with 2020-11-01", func() {
		conf := cronicle.Default()
		schedule := conf.Schedules[0]
		task := schedule.Tasks[0]
		task.Command = []string{
			"/bin/echo",
			"${date}, ${date}",
			"${datetime}, ${datetime}",
			"${timestamp}, ${timestamp}"}
		t, _ := time.Parse(time.RFC3339, "2020-11-01T22:08:41+00:00")
		r := task.Exec(t)

		Expect(r).To(Equal(exec.Result{
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
