package cron

import (
	"fmt"
	"runtime"

	"github.com/jshiv/cronicle/internal/bash"
	"github.com/jshiv/cronicle/internal/config"

	"github.com/fatih/color"

	"path/filepath"

	log "github.com/sirupsen/logrus"
	cron "github.com/robfig/cron/v3"

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
	hcl := config.GetHcl(*conf)
	slantyedCyan := color.New(color.FgCyan, color.Italic).SprintFunc()
	fmt.Printf("%s", slantyedCyan(string(hcl.Bytes())))

	RunConfig(*conf)
}

//RunConfig starts cron
func RunConfig(conf config.Config) {
	log.WithFields(log.Fields{"cronicle": "start"}).Info("Starting Scheduler...")

	c := cron.New()
	c.AddFunc("@every 6m", func() { log.WithFields(log.Fields{"cronicle": "heartbeat"}).Info("Running...") })
	for _, schedule := range conf.Schedules {
		cronID, err := c.AddFunc(schedule.Cron, AddSchedule(schedule))
		fmt.Println(cronID)
		if err != nil {
			fmt.Printf("\x1b[31;1m%s\x1b[0m\n", fmt.Sprintf("schedule cron format error: %s", schedule.Name))
			log.Fatal(err)
		}
	}
	c.Start()
	runtime.Goexit()
}

// AddSchedule retuns a function primed with the given schedules commands
func AddSchedule(schedule config.Schedule) func() {
	log.WithFields(log.Fields{"schedule": schedule.Name}).Info("Running...")

	return ExecuteTasks(schedule)
}

// ExecuteTasks handels the execution of all tasks in a given schedule.
// By default tasks execute in parallel unless wait_for is given
func ExecuteTasks(schedule config.Schedule) func() {
	return func() {
		for _, task := range schedule.Tasks {
			go func(task config.Task) {
				r, err := ExecuteTask(&task)
				fmt.Println(err)
				LogTask(&task, r)
			}(task)

		}
	}
}

// ExecuteTask does a git pull, git checkout and exec's the given command
func ExecuteTask(task *config.Task) (bash.Result, error) {
	// log.WithFields(log.Fields{"task": task.Name}).Info(task.Command)

	if task.Repo != "" {
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
	if len(task.Command) > 0 {
		result = bash.Bash(task.Command, task.Path)
	}

	return result, nil
}

//LogTask logs the exit status, stderr, git commit and other logging data.
func LogTask(task *config.Task, res bash.Result) {
	if res.ExitStatus == 0 {
		log.WithFields(log.Fields{
			"task":   task.Name,
			"exit":   res.ExitStatus,
			"commit": task.Git.Commit.Hash.String()[:11],
			"email":  task.Git.Commit.Author.Email,
		}).Info(res.Stdout)
	} else if res.ExitStatus == 1 {
		log.WithFields(log.Fields{
			"task":   task.Name,
			"exit":   res.ExitStatus,
			"commit": task.Git.Commit.Hash.String()[:11],
			"email":  task.Git.Commit.Author.Email,
		}).Error(res.Stderr)
	} else {
		log.WithFields(log.Fields{
			"task":   task.Name,
			"exit":   res.ExitStatus,
			"commit": task.Git.Commit.Hash.String()[:11],
			"email":  task.Git.Commit.Author.Email,
		}).Error(res.Stderr)
	}
}

func Dummy(in string) string {
	return in
}
