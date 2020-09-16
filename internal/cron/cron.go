package cron

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/jshiv/cronicle/internal/bash"
	"github.com/jshiv/cronicle/internal/config"

	"github.com/fatih/color"

	"path/filepath"

	cron "github.com/robfig/cron/v3"
	log "github.com/sirupsen/logrus"

	"gopkg.in/src-d/go-git.v4"
	c "gopkg.in/src-d/go-git.v4/config"
	"gopkg.in/src-d/go-git.v4/plumbing"
)

// Run is the main function of the cron package
func Run(cronicleFile string) {

	cronicleFileAbs, err := filepath.Abs(cronicleFile)
	if err != nil {
		log.Fatal(err)
	}
	// croniclePath := filepath.Dir(cronicleFileAbs)

	conf, _ := config.GetConfig(cronicleFileAbs)
	hcl := conf.Hcl()
	slantyedCyan := color.New(color.FgCyan, color.Italic).SprintFunc()
	fmt.Printf("%s", slantyedCyan(string(hcl.Bytes)))

	RunConfig(*conf)
}

//RunConfig starts cron
func RunConfig(conf config.Config) {
	log.WithFields(log.Fields{"cronicle": "start"}).Info("Starting Scheduler...")
	queue := make(chan []byte, 1000)
	c := cron.New()
	c.AddFunc("@every 6m", func() { log.WithFields(log.Fields{"cronicle": "heartbeat"}).Info("Running...") })
	for _, schedule := range conf.Schedules {
		cronID, err := c.AddFunc(schedule.Cron, QueueSchedule(schedule, queue))
		// cronID, err := c.AddFunc(schedule.Cron, AddSchedule(schedule))

		fmt.Println(cronID)
		if err != nil {
			fmt.Printf("\x1b[31;1m%s\x1b[0m\n", fmt.Sprintf("schedule cron format error: %s", schedule.Name))
			log.Fatal(err)
		}
	}
	c.Start()

	for scheduleBytes := range queue {
		// fmt.Println(string(scheduleBytes))
		var s config.Schedule
		err := json.Unmarshal(scheduleBytes, &s)
		s.PropigateTaskProperties("./")

		if err != nil {
			fmt.Println(err)
		}
		// fmt.Println(s.Tasks[0])
		ExecuteTasks(s)()
	}
	runtime.Goexit()
}

// AddSchedule retuns a function primed with the given schedules commands
func AddSchedule(schedule config.Schedule) func() {
	log.WithFields(log.Fields{"schedule": schedule.Name}).Info("Running...")
	return ExecuteTasks(schedule)
}

// QueueSchedule produces the json of a schdule to the buffered queue channel
func QueueSchedule(schedule config.Schedule, queue chan []byte) func() {
	log.WithFields(log.Fields{"schedule": schedule.Name}).Info("Queuing...")
	return func() {
		schedule.Now = time.Now().In(time.Local)
		schedule.CleanGit()
		queue <- schedule.JSON()
	}
}

// ExecuteTasks handels the execution of all tasks in a given schedule.
// By default tasks execute in parallel unless wait_for is given
func ExecuteTasks(schedule config.Schedule) func() {
	return func() {
		// TODO: added location specification in schedule struct
		// https://godoc.org/github.com/robfig/cron
		var now time.Time
		if (schedule.Now == time.Time{}) {
			now = time.Now().In(time.Local)
		} else {
			now = schedule.Now
		}
		fmt.Println("Schedule exec time: ", now)
		for _, task := range schedule.Tasks {
			go func(task config.Task) {
				r, err := ExecuteTask(&task, now)
				fmt.Println(err)
				LogTask(&task, r)
			}(task)

		}
	}
}

// ExecTasks parses the cronicle.hcl config, filters for a specified task
// and executes the task
func ExecTasks(cronicleFile string, taskName string, scheduleName string, now time.Time) {

	cronicleFileAbs, err := filepath.Abs(cronicleFile)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Reading from: " + cronicleFileAbs)

	conf, _ := config.GetConfig(cronicleFileAbs)

	tasks := conf.TaskArray().FilterTasks(taskName, scheduleName)
	slantyedCyan := color.New(color.FgCyan, color.Italic).SprintFunc()

	var c config.Config
	execSchedule := config.Schedule{Name: "exec", Cron: now.String(), Tasks: tasks}
	c.Schedules = []config.Schedule{execSchedule}
	fmt.Printf("%s", slantyedCyan(string(c.Hcl().Bytes)))

	for _, task := range tasks {
		r, _ := ExecuteTask(&task, now)
		LogTask(&task, r)
	}

}

// ExecuteTask does a git pull, git checkout and exec's the given command
func ExecuteTask(task *config.Task, t time.Time) (bash.Result, error) {
	// log.WithFields(log.Fields{"task": task.Name}).Info(task.Command)
	task.SetGit()
	fmt.Println(string(task.JSON()))

	if task.Repo != "" {
		// task.SetGit()
		var branch string
		if task.Branch != "" {
			branch = task.Branch
		} else {
			branch = "master"
		}

		var commit string
		if task.Commit != "" {
			commit = task.Commit
		}

		err := task.Git.Repository.Fetch(&git.FetchOptions{
			RefSpecs: []c.RefSpec{"refs/*:refs/*", "HEAD:refs/heads/HEAD"},
		})
		if err != nil {
			switch err {
			case git.NoErrAlreadyUpToDate:
			default:
				return bash.Result{}, err
			}
		}

		var checkoutOptions git.CheckoutOptions
		if commit != "" {
			h := plumbing.NewHash(commit)
			checkoutOptions = git.CheckoutOptions{
				Create: false, Force: false, Hash: h,
			}
		} else {
			b := plumbing.NewBranchReferenceName(branch)
			checkoutOptions = git.CheckoutOptions{
				Create: false, Force: false, Branch: b,
			}
		}

		if err := task.Git.Worktree.Checkout(&checkoutOptions); err != nil {
			return bash.Result{}, err
		}

		// } else if task.Commit != "" {
		// 	cn := plumbing.NewHash(task.Commit)
		// 	task.Git.Worktree.Pull(&git.PullOptions{})
		// 	task.Git.Worktree.Checkout(&git.CheckoutOptions{Hash: cn, Force: true})
		// } else {
		// 	task.Git.Worktree.Pull(&git.PullOptions{})
		// }
	}

	if task.Git.Repository != nil {
		task.Git.Head, _ = task.Git.Repository.Head()
		task.Git.Commit, _ = task.Git.Repository.CommitObject(task.Git.Head.Hash())
	}

	var result bash.Result
	r := strings.NewReplacer(
		"${date}", t.Format(config.TimeArgumentFormatMap["${date}"]),
		"${datetime}", t.Format(config.TimeArgumentFormatMap["${datetime}"]),
		"${timestamp}", t.Format(config.TimeArgumentFormatMap["${timestamp}"]),
	)
	if len(task.Command) > 0 {
		cmd := make([]string, len(task.Command))
		for i, s := range task.Command {
			s = r.Replace(s)
			cmd[i] = s
		}

		result = bash.Bash(cmd, task.Path)
	}

	return result, nil
}

//LogTask logs the exit status, stderr, git commit and other logging data.
func LogTask(task *config.Task, res bash.Result) {

	if res.ExitStatus == 0 {
		log.WithFields(log.Fields{
			"schedule": task.ScheduleName,
			"task":     task.Name,
			"exit":     res.ExitStatus,
			"commit":   task.Git.Commit.Hash.String()[:11],
			"email":    task.Git.Commit.Author.Email,
			"success":  true,
		}).Info(res.Stdout)
	} else if res.ExitStatus == 1 {
		log.WithFields(log.Fields{
			"schedule": task.ScheduleName,
			"task":     task.Name,
			"exit":     res.ExitStatus,
			"commit":   task.Git.Commit.Hash.String()[:11],
			"email":    task.Git.Commit.Author.Email,
			"success":  false,
		}).Error(res.Stderr)
	} else {
		log.WithFields(log.Fields{
			"schedule": task.ScheduleName,
			"task":     task.Name,
			"exit":     res.ExitStatus,
			"commit":   task.Git.Commit.Hash.String()[:11],
			"email":    task.Git.Commit.Author.Email,
			"success":  false,
		}).Error(res.Stderr)
	}
}

func Dummy(in string) string {
	return in
}
