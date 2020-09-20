package config

import (
	"strings"
	"time"

	"github.com/jshiv/cronicle/internal/bash"

	log "github.com/sirupsen/logrus"
)

//Exec executes task.Command at task.Path and returns the bash.Result struct
//prior to execution, the command will replace any ${date}, ${datetime}, ${timestamp}
//with time t given in the bash command
func (task *Task) Exec(t time.Time) bash.Result {
	var result bash.Result
	r := strings.NewReplacer(
		"${date}", t.Format(TimeArgumentFormatMap["${date}"]),
		"${datetime}", t.Format(TimeArgumentFormatMap["${datetime}"]),
		"${timestamp}", t.Format(TimeArgumentFormatMap["${timestamp}"]),
	)
	if len(task.Command) > 0 {
		cmd := make([]string, len(task.Command))
		for i, s := range task.Command {
			s = r.Replace(s)
			cmd[i] = s
		}

		result = bash.Bash(cmd, task.Path)
	}
	return result
}

// Execute does a git pull, git checkout and exec's the given command
func (task *Task) Execute(t time.Time) (bash.Result, error) {

	//Validate the task
	if err := task.Validate(); err != nil {
		return bash.Result{}, err
	}

	//If a repo is given, clone the repo and task.Git.Open(task.Path)
	if task.Repo != "" {
		if err := task.Clone(); err != nil {
			return bash.Result{}, err
		}
	}

	//Set HEAD and commit state after checkout branch/commit
	if task.Git.Repository != nil {
		if err := task.Checkout(); err != nil {
			return bash.Result{}, err
		}
	}

	//Execute task.Command in bash at time t
	result := task.Exec(t)

	return result, nil
}

//Log logs the exit status, stderr, git commit and other logging data.
func (task *Task) Log(res bash.Result) {

	var commit string
	var email string
	if task.Git.Repository != nil {
		commit = task.Git.Commit.Hash.String()[:11]
		email = task.Git.Commit.Author.Email
	} else {
		commit = "null"
		email = "null"

	}
	if res.ExitStatus == 0 {
		log.WithFields(log.Fields{
			"schedule": task.ScheduleName,
			"task":     task.Name,
			"exit":     res.ExitStatus,
			"commit":   commit,
			"email":    email,
			"success":  true,
		}).Info(res.Stdout)
	} else if res.ExitStatus == 1 {
		log.WithFields(log.Fields{
			"schedule": task.ScheduleName,
			"task":     task.Name,
			"exit":     res.ExitStatus,
			"commit":   commit,
			"email":    email,
			"success":  false,
		}).Error(res.Stderr)
	} else {
		log.WithFields(log.Fields{
			"schedule": task.ScheduleName,
			"task":     task.Name,
			"exit":     res.ExitStatus,
			"commit":   commit,
			"email":    email,
			"success":  false,
		}).Error(res.Stderr)
	}
}
