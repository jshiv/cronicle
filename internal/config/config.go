package config

import (
	"errors"
	"log"
	"path/filepath"
	"time"

	"gopkg.in/src-d/go-git.v4/plumbing"
)

// Config is the configuration structure for the cronicle checker.
// https://raw.githubusercontent.com/mitchellh/golicense/master/config/config.go
type Config struct {
	Version   string     `hcl:"version,optional"`
	Git       string     `hcl:"git"`
	Schedules []Schedule `hcl:"schedule,block"`
	// Repos points at external dependent repos that maintain their own schedules remotly.
	Repos []string `hcl:"repos,optional"`
}

// Schedule is the configuration structure that defines a cron job consisting of tasks.
type Schedule struct {
	// Cron is the schedule interval. The field accepts standard cron
	// and other configurations listed here https://godoc.org/gopkg.in/robfig/cron.v2
	// i.e. ["@hourly", "@every 1h30m", "0 30 * * * *", "TZ=Asia/Tokyo 30 04 * * * *"]
	Name      string `hcl:"name,label"`
	Cron      string `hcl:"cron,optional"`
	Repo      string `hcl:"repo,optional"`
	StartDate string `hcl:"start_date,optional"`
	EndDate   string `hcl:"end_date,optional"`
	Owner     *Owner `hcl:"owner,block"`
	Tasks     []Task `hcl:"task,block"`
	//Now is the execution time of the given schedule that will be used to
	//fill variable task command ${datetime}. The cron scheduler generally provides
	//the value.
	Now time.Time
}

// Task is the configuration structure that defines a task (i.e., a command)
type Task struct {
	Name         string   `hcl:"name,label"`
	Command      []string `hcl:"command,optional"`
	Depends      []string `hcl:"depends,optional"`
	Owner        *Owner   `hcl:"owner,block"`
	Repo         string   `hcl:"repo,optional"`
	Branch       string   `hcl:"branch,optional"`
	Commit       string   `hcl:"commit,optional"`
	Path         string
	Git          Git
	ScheduleName string
}

// Owner is the configuration structure that defines an owner of a schedule or task
type Owner struct {
	Name  string `hcl:"name"`
	Email string `hcl:"email,optional"`
}

var (
	//ErrBranchAndCommitGiven is thrown because commit and branch are mutually exclusive to identify a repo
	ErrBranchAndCommitGiven = errors.New("branch and commit can not both be populated")
	//ErrScheduleNameEmpty is thrown because schedule.Name == "", hcl can not be given with schedule "" {}
	ErrScheduleNameEmpty = errors.New("schedule name can not be an empty string")
	//ErrTaskNameEmpty is thrown because task.Name == "", hcl can not be given with task "" {}
	ErrTaskNameEmpty = errors.New("task name can not be an empty string")
)

// Validate validates the fields and sets the default values.
func (task *Task) Validate() error {
	if task.Branch != "" {
		if task.Commit != "" {
			return ErrBranchAndCommitGiven
		}
	}

	if task.Branch != "" {
		task.Git.ReferenceName = plumbing.NewBranchReferenceName(task.Branch)
	} else {
		task.Git.ReferenceName = plumbing.HEAD

	}

	return nil
}

//Validate checks that schedule.Name is not empty and assigns task.ScheduleName
//on a whole config struct.
func (conf *Config) Validate() error {

	for _, schedule := range conf.Schedules {
		if schedule.Name == "" {
			return ErrScheduleNameEmpty
		}

		for _, task := range schedule.Tasks {
			if task.Name == "" {
				return ErrTaskNameEmpty
			}
		}
	}
	return nil
}

//PropigateProperties Pushes the given croniclePath
func (conf *Config) PropigateProperties(croniclePath string) {
	// Assign the path for each task or schedule repo
	for sdx, schedule := range conf.Schedules {
		for tdx, task := range schedule.Tasks {
			if task.Branch != "" {
				task.Git.ReferenceName = plumbing.NewBranchReferenceName(task.Branch)
			} else {
				task.Git.ReferenceName = plumbing.HEAD

			}

			var path string
			var taskPath string
			var repo string

			// If the task is associated to a repo
			if task.Repo != "" {
				repo = task.Repo
				// If a Schedule is associated to a repo, all sub tasks are by default associated
			} else if schedule.Repo != "" {
				repo = schedule.Repo
				// Else the repo is the cronicle repo
			} else {
				//TODO: make remote cronicle repo rathar than ""
				repo = ""
			}
			// If the task is associated to a repo, put it in the repos directory
			if task.Repo != "" {
				path, _ = LocalRepoDir(croniclePath, task.Repo)
				// If a Schedule is associated to a repo, all sub tasks are by default associated
			} else if schedule.Repo != "" {
				path, _ = LocalRepoDir(croniclePath, schedule.Repo)
				// Else the path is the root croniclePath
			} else {
				path = croniclePath
			}

			// If the given task is associatated to a repo, clone the task to an independent path
			if repo != "" {
				taskPath = filepath.Join(path, schedule.Name, task.Name)
				// Else the task is associated to the root croniclePath
			} else {
				taskPath = croniclePath
			}
			conf.Schedules[sdx].Tasks[tdx].Path = taskPath
			conf.Schedules[sdx].Tasks[tdx].Repo = repo
			conf.Schedules[sdx].Tasks[tdx].ScheduleName = schedule.Name
		}
	}
}

//Default returns a basic default Config
//it includes a single schedule that runs every 5 seconds
//and a single "Hello World" task.
func Default() Config {

	var task Task
	task.Name = "hello"
	task.Command = []string{"/bin/echo", "Hello World --date=${date}"}

	var schedule Schedule
	schedule.Name = "example"
	schedule.Cron = "@every 5s"
	schedule.Tasks = []Task{task}

	var conf Config
	conf.Schedules = []Schedule{schedule}

	return conf
}

//TaskArray is an array of Task structs,
//calling config.TaskArra() ensures that each task.ScheduleName is filled
type TaskArray []Task

//TaskArray exports a TaskArray all tasks in a given config,
//additionally, it ensures that task.ScheduleName is propigated
func (conf *Config) TaskArray() TaskArray {

	err := conf.Validate() // ensure that schedule.Name and task.ScheduleName are not empty
	if err != nil {
		log.Fatal(err)
	}
	tasks := TaskArray{}

	for _, schedule := range conf.Schedules {
		for _, task := range schedule.Tasks {
			tasks = append(tasks, task)
		}
	}
	return tasks
}

//FilterTasks returns a task array where
//only matching task.Name = taskName and schedule.Name=scheduleName
//if taskName = "" and scheduleName = "" then all tasks will be returned
//empty strings are intrepreted as no filtering requested.
func (t TaskArray) FilterTasks(taskName string, scheduleName string) TaskArray {

	tasks := TaskArray{}
	if taskName == "" && scheduleName != "" {
		// if taskName is "", retrun all tasks with a matching scheduleName
		for _, task := range t {
			if task.ScheduleName == scheduleName {
				tasks = append(tasks, task)
			}
		}
	} else if taskName != "" && scheduleName == "" {
		// if scheduleName is "", retrun all tasks with a matching taskName
		for _, task := range t {
			if task.Name == taskName {
				tasks = append(tasks, task)
			}
		}
	} else if taskName != "" && scheduleName != "" {
		// if taskName and scheduleName are both gicen, return only tasks that match on both
		for _, task := range t {
			if task.Name == taskName && task.ScheduleName == scheduleName {
				tasks = append(tasks, task)
			}
		}
	} else {
		//if both arguments are "", return all tasks
		tasks = t
	}

	return tasks
}
