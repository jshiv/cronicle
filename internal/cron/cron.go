package cron

import (
	"fmt"
	"runtime"

	"github.com/jshiv/cronicle/internal/bash"
	"github.com/jshiv/cronicle/internal/config"
	ingit "github.com/jshiv/cronicle/internal/git"

	"github.com/fatih/color"

	"path/filepath"

	log "github.com/sirupsen/logrus"
	"gopkg.in/robfig/cron.v2"

	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/object"
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
	c.AddFunc("@every 10s", func() { log.WithFields(log.Fields{"cronicle": "heartbeat"}).Info("Running...") })
	for _, schedule := range conf.Schedules {
		_, err := c.AddFunc(schedule.Cron, AddSchedule(schedule))
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

	return func() {
		for _, task := range schedule.Tasks {
			log.WithFields(log.Fields{"task": task.Name}).Info(task.Command)
			ingit.Pull(task.Path)
			result := bash.Bash(task.Command, task.Path)
			commit, err := ingit.GetCommit(task.Path)
			if err != nil {
				log.WithFields(log.Fields{
					"task": task.Name,
					"exit": result.ExitStatus,
				}).Error(err)
			} else if result.ExitStatus == 0 {
				log.WithFields(log.Fields{
					"task":   task.Name,
					"exit":   result.ExitStatus,
					"commit": commit.Hash.String()[:11],
					"email":  commit.Author.Email,
				}).Info(result.Stdout)
			} else if result.ExitStatus == 1 {
				log.WithFields(log.Fields{
					"task":   task.Name,
					"exit":   result.ExitStatus,
					"commit": commit.Hash.String()[:11],
					"email":  commit.Author.Email,
				}).Error(result.Stderr)
			} else {
				log.WithFields(log.Fields{
					"task":   task.Name,
					"exit":   result.ExitStatus,
					"commit": commit.Hash.String()[:11],
					"email":  commit.Author.Email,
				}).Error(result.Stderr)
			}
		}
	}
}

type TaskMeta struct {
	config.Task
	Result     bash.Result
	Worktree   git.Worktree
	Repository git.Repository
	Head       plumbing.Reference
	Hash       plumbing.Hash
	Commit     object.Commit
}

func (task *TaskMeta) GitTask() {
	r, _ := git.PlainOpen(task.Path)
	task.Repository = *r

	h, _ := r.Head()
	task.Head = *h

	wt, _ := r.Worktree()
	task.Worktree = *wt

	task.Hash = h.Hash()

	cIter, _ := r.Log(&git.LogOptions{From: task.Hash})
	commit, _ := cIter.Next()
	task.Commit = *commit

}

func ExecuteTask(task *TaskMeta) TaskMeta {
	log.WithFields(log.Fields{"task": task.Name}).Info(task.Command)
	task.GitTask()
	task.Worktree.Pull(&git.PullOptions{ReferenceName: task.Head.Name()})
	// ingit.Pull(task.Path)
	result := bash.Bash(task.Command, task.Path)
	task.Result = result

	return *task
}

// func LogTask(task *config.Task) {
// 	log.WithFields(log.Fields{"task": task.Name}).Info(task.Command)
// 	ingit.Pull(task.Path)
// 	result := bash.Bash(task.Command, task.Path)
// 	commit, err := ingit.GetCommit(task.Path)
// }

func Dummy(in string) string {
	return in
}
