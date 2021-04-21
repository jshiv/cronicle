package cronicle

import (
	"strings"
	"time"

	"github.com/jshiv/cronicle/pkg/exec"
	"gopkg.in/matryer/try.v1"

	log "github.com/sirupsen/logrus"
)

//Exec executes task.Command at task.Path and returns the exec.Result struct
//prior to execution, the command will replace any ${date}, ${datetime}, ${timestamp}
//with time t given in the bash command
func (task *Task) Exec(t time.Time) exec.Result {
	var result exec.Result
	r := strings.NewReplacer(
		"${date}", t.Format(TimeArgumentFormatMap["${date}"]),
		"${datetime}", t.Format(TimeArgumentFormatMap["${datetime}"]),
		"${timestamp}", t.Format(TimeArgumentFormatMap["${timestamp}"]),
		"${path}", task.Path,
	)
	if len(task.Command) > 0 {
		cmd := make([]string, len(task.Command))
		for i, s := range task.Command {
			s = r.Replace(s)
			cmd[i] = s
		}

		result = exec.Execute(cmd, task.Path, task.Env)
	}
	return result
}

// Execute does a git pull, git checkout and exec's the given command
func (task *Task) Execute(t time.Time) (exec.Result, error) {

	//Validate the task
	if err := task.Validate(); err != nil {
		return exec.Result{}, err
	}

	//Test if the given task should execute in the root croniclePath and the croncilePath is a git repo
	taskPathIsCroniclePathWithGit := (task.Path == task.CroniclePath) && task.CronicleRepo != nil

	//If a repo is given, clone the repo and task.Git.Open(task.Path)
	if task.Repo != nil {
		auth, err := task.Repo.Auth()
		if err != nil {
			return exec.Result{}, err
		}
		g, err := Clone(task.Path, task.Repo.URL, &auth)
		// g, err := Clone(task.Path, task.Repo.URL, task.Repo.DeployKey)
		if err != nil {
			return exec.Result{}, err
		}
		task.Git = g
		err = task.Git.Checkout(task.Repo.Branch, task.Repo.Commit)
		if err != nil {
			return exec.Result{}, err
		}
	} else if taskPathIsCroniclePathWithGit {
		auth, err := task.CronicleRepo.Auth()
		if err != nil {
			return exec.Result{}, err
		}
		task.Git, err = Clone(task.CroniclePath, task.CronicleRepo.URL, &auth)
		// var err error
		// task.Git, err = Clone(task.CroniclePath, task.CronicleRepo.URL, task.CronicleRepo.DeployKey)
		if err != nil {
			log.Error(err)
			return exec.Result{}, err
		}
	}

	//Execute task.Command in bash at time t with retry
	var result exec.Result
	err := try.Do(func(attempt int) (bool, error) {

		log.WithFields(log.Fields{
			"schedule": task.ScheduleName,
			"task":     task.Name,
			"attempt":  attempt,
			"clock":    t.Format(time.Kitchen),
			"date":     t.Format(time.RFC850),
		}).Info("Executing...")
		var err error
		result = task.Exec(t)
		err = result.Error
		task.Log(result)
		if err != nil && task.Retry != nil {
			duration := time.Duration(task.Retry.Seconds) * time.Second
			duration += time.Duration(task.Retry.Minutes) * time.Minute
			duration += time.Duration(task.Retry.Hours) * time.Hour
			time.Sleep(duration)
		}

		var retryCount int
		switch task.Retry {
		case nil:
			retryCount = 0
		default:
			retryCount = task.Retry.Count
		}

		return attempt < retryCount, err
	})
	if err != nil {
		return result, err
	}

	return result, nil
}

//Log logs the exit status, stderr, git commit and other logging data.
func (task *Task) Log(res exec.Result) {

	var commit string
	var email string
	if task.Git.Commit != nil {
		commit = task.Git.Commit.Hash.String()[:11]
		email = task.Git.Commit.Author.Email
	} else {
		commit = "null"
		email = "null"
	}

	if res.Error != nil {
		log.WithFields(log.Fields{
			"schedule": task.ScheduleName,
			"task":     task.Name,
			"path":     task.Path,
			"exit":     res.ExitStatus,
			"error":    res.Error,
			"commit":   commit,
			"email":    email,
			"success":  false,
			"command":  strings.Join(res.Command, " "),
		}).Error(res.Stderr)
	} else {
		log.WithFields(log.Fields{
			"schedule": task.ScheduleName,
			"task":     task.Name,
			"path":     task.Path,
			"exit":     res.ExitStatus,
			"commit":   commit,
			"email":    email,
			"success":  true,
			"command":  strings.Join(res.Command, " "),
		}).Info(res.Stdout)
	}

}
