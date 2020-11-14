package cronicle

import (
	"errors"
	"log"
	"path/filepath"
	"time"
)

// Config is the configuration structure for the cronicle checker.
// https://raw.githubusercontent.com/mitchellh/golicense/master/config/config.go
// TODO: Add Version string `hcl:"version,optional"`
type Config struct {
	// Repo repository containing version controlled cronicle.hcl
	Repo *Repo `hcl:"repo,block"`
	// Cron expression that specifies the cronicle heartbeat and cronicle.hcl refresh
	Heartbeat string `hcl:"heartbeat,optional"`
	// Repos points at external dependent repos that maintain their own schedules remotly.
	Repos []string `hcl:"repos,optional"`
	// Timezone Location to run cron in. i.e. "America/New_York" [IANA Time Zone database]
	// https://en.wikipedia.org/wiki/List_of_tz_database_time_zones
	Timezone string `hcl:"timezone,optional"`
	// GitRemote *GitRemote `hcl:"git,block"`
	Queue     *Queue     `hcl:"queue,block"`
	Schedules []Schedule `hcl:"schedule,block"`
}

// Schedule is the configuration structure that defines a cron job consisting of tasks.
type Schedule struct {
	// Cron is the schedule interval. The field accepts standard cron
	// and other configurations listed here https://godoc.org/gopkg.in/robfig/cron.v2
	// i.e. ["@hourly", "@every 1h30m", "0 30 * * * *", "TZ=Asia/Tokyo 30 04 * * * *"]
	Name string `hcl:"name,label"`
	Cron string `hcl:"cron,optional"`
	// Timezone Location to run cron in. i.e. "America/New_York" [IANA Time Zone database]
	// https://en.wikipedia.org/wiki/List_of_tz_database_time_zones
	Timezone  string `hcl:"timezone,optional"`
	StartDate string `hcl:"start_date,optional"`
	EndDate   string `hcl:"end_date,optional"`
	Repo      *Repo  `hcl:"repo,block"`
	Tasks     []Task `hcl:"task,block"`
	//Now is the execution time of the given schedule that will be used to
	//fill variable task command ${datetime}. The cron scheduler generally provides
	//the value.
	Now time.Time
	//repo given at the config level, will be overridden by repo given at schedule or task level.
	CronicleRepo *Repo
}

// Task is the configuration structure that defines a task (i.e., a command)
type Task struct {
	Name         string   `hcl:"name,label"`
	Command      []string `hcl:"command,optional"`
	Depends      []string `hcl:"depends,optional"`
	Repo         *Repo    `hcl:"repo,block"`
	Retry        *Retry   `hcl:"retry,block"`
	Path         string
	CronicleRepo *Repo
	CroniclePath string
	Git          Git
	ScheduleName string
}

// Repo is the structure that defines a git repository
type Repo struct {
	// URL is the remote git repository, a local path to git repository
	URL string `hcl:"url,optional"`
	// DeployKey is the path to the rsa private key that enables pull access to a
	// private remote repository.
	DeployKey string `hcl:"key,optional"`
	Commit    string `hcl:"branch,optional"`
	Branch    string `hcl:"commit,optional"`
}

//Retry defines the retry count and delay in number and seconds.
type Retry struct {
	//Count: Number of retry attemts to make after first attempt
	Count int `hcl:"count,optional"`
	//Seconds: Number of seconds to wait between retry attempts
	Seconds int `hcl:"seconds,optional"`
	//Minutes: Number of minutes to wait between retry attempts
	Minutes int `hcl:"minutes,optional"`
	//Hours: Number of hours to wait between retry attempts
	Hours int `hcl:"hours,optional"`
}

// Queue is the metadata associated to the message queue for distributed operation.
// Cronicle uses vice to communicate with queues via channels.
// https://github.com/matryer/vice
type Queue struct {
	//Type names the message queue technology to be used
	//options are nsq and redis
	Type string `hcl:"type,optional"`
	//host:port of nsqd/nsqlookupd/redis queue service
	Addr string `hcl:"addr,optional"`
}

var (
	//ErrBranchAndCommitGiven is thrown because commit and branch are mutually exclusive to identify a repo
	ErrBranchAndCommitGiven = errors.New("branch and commit can not both be populated")
	//ErrRepoNotGiven is thrown because a git repo is not given, for the case where Checkout or other git
	//specific methods are called
	ErrRepoNotGiven = errors.New("git repo has not been given")
	//ErrIfRepoGivenAndPathNotGiven is thrown because a repo was given but the path to the local repo has not been provided
	ErrIfRepoGivenAndPathNotGiven = errors.New("if repo is populated, path must also be given at runtime")
	//ErrScheduleNameEmpty is thrown because schedule.Name == "", hcl can not be given with schedule "" {}
	ErrScheduleNameEmpty = errors.New("schedule name can not be an empty string")
	//ErrTaskNameEmpty is thrown because task.Name == "", hcl can not be given with task "" {}
	ErrTaskNameEmpty = errors.New("task name can not be an empty string")
	//ErrRepoGivenAndURLNotGiven is thrown because task.Name == "", hcl can not be given with task "" {}
	ErrRepoGivenAndURLNotGiven = errors.New("if repo is populated, it must have an assoicated url")
)

// Validate validates the fields and sets the default values.
func (task *Task) Validate() error {
	if task.Repo != nil {
		if task.Repo.Branch != "" && task.Repo.Commit != "" {
			return ErrBranchAndCommitGiven
		}
	}

	if task.Repo != nil {
		if task.Repo.URL == "" {
			return ErrRepoGivenAndURLNotGiven
		}
	}

	if task.Repo != nil {
		if task.Path == "" {
			return ErrIfRepoGivenAndPathNotGiven
		}
	}

	return nil
}

//Validate checks that schedule.Name is not empty and assigns task.ScheduleName
//on a whole config struct.
func (conf *Config) Validate() error {

	if conf.Timezone != "" {
		if _, err := time.LoadLocation(conf.Timezone); err != nil {
			return err
		}
	}

	for _, schedule := range conf.Schedules {
		if schedule.Timezone != "" {
			if _, err := time.LoadLocation(schedule.Timezone); err != nil {
				return err
			}
		}

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

//PropigateTaskProperties pushes schedule.Name, schedule.Repo and the repo path down to the task values.
//It also populates task.Git.ReferenceName with task.Branch or HEAD.
func (conf *Config) PropigateTaskProperties(croniclePath string) {
	for i := range conf.Schedules {
		if conf.Schedules[i].Timezone == "" {
			conf.Schedules[i].Timezone = conf.Timezone
		}
		conf.Schedules[i].CronicleRepo = conf.Repo
		conf.Schedules[i].PropigateTaskProperties(croniclePath)
	}
}

//PropigateTaskProperties pushes schedule.Name, schedule.Repo and the repo path down to the task values.
//It also populates task.Git.ReferenceName with task.Branch or HEAD.
func (schedule *Schedule) PropigateTaskProperties(croniclePath string) {
	// Assign the path for each task or schedule repo
	for i, task := range schedule.Tasks {
		// if task.Branch != "" {
		// 	task.Git.ReferenceName = plumbing.NewBranchReferenceName(task.Branch)
		// } else {
		// 	task.Git.ReferenceName = plumbing.HEAD
		// }

		var path string
		var taskPath string
		var repo *Repo

		// If the task is associated to a repo
		if task.Repo != nil {
			repo = task.Repo
			// Internally propigate REPO properties from schedule.Repo if they are defined in schedle but not in task
			if schedule.Repo != nil {
				if task.Repo.URL == "" && schedule.Repo.URL != "" {
					repo.URL = schedule.Repo.URL
				}
				if task.Repo.DeployKey == "" && schedule.Repo.DeployKey != "" {
					repo.DeployKey = schedule.Repo.DeployKey
				}
				if task.Repo.Branch == "" && schedule.Repo.Branch != "" {
					repo.Branch = schedule.Repo.Branch
				}
				if task.Repo.Commit == "" && schedule.Repo.Commit != "" {
					repo.Commit = schedule.Repo.Commit
				}
			}
			// If a Schedule is associated to a repo, all sub tasks are by default associated
		} else if schedule.Repo != nil {
			repo = schedule.Repo
			// Else the repo is the cronicle repo
		} else {
			repo = nil
		}
		// If the task is associated to a repo, put it in the repos directory
		if task.Repo != nil {
			path, _ = LocalRepoDir(croniclePath, task.Repo.URL)
			// If a Schedule is associated to a repo, all sub tasks are by default associated
		} else if schedule.Repo != nil {
			path, _ = LocalRepoDir(croniclePath, schedule.Repo.URL)
			// Else the path is the root croniclePath
		} else {
			path = croniclePath
		}

		// If the given task is associatated to a repo, clone the task to an independent path
		if repo != nil {
			taskPath = filepath.Join(path, schedule.Name, task.Name)
			// Else the task is associated to the root croniclePath
		} else {
			taskPath = croniclePath
		}

		schedule.Tasks[i].Path = taskPath
		schedule.Tasks[i].CroniclePath = croniclePath
		schedule.Tasks[i].CronicleRepo = schedule.CronicleRepo
		schedule.Tasks[i].Repo = repo
		schedule.Tasks[i].ScheduleName = schedule.Name
	}
}

//Default returns a basic default Config
//it includes a single schedule that runs every 5 seconds
//and a single "Hello World" task.
func Default() Config {

	var task Task
	task.Name = "bar"
	task.Command = []string{"/bin/echo", "Hello World --date=${date}"}

	var schedule Schedule
	schedule.Name = "foo"
	schedule.Cron = "@every 5s"
	schedule.Tasks = []Task{task}

	var conf Config
	conf.Heartbeat = "@every 30s"
	conf.Schedules = []Schedule{schedule}

	return conf
}

//TaskArray is an array of Task structs,
//calling config.TaskArray() ensures that each task.ScheduleName is filled
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

//TaskMap is an map of key=task.Name: value=Task struct,
type TaskMap map[string]Task

//TaskMap exports a TaskMap all tasks in a given config,
//additionally, it ensures that task.ScheduleName is propigated
func (schedule *Schedule) TaskMap() TaskMap {

	taskMap := TaskMap{}
	for _, task := range schedule.Tasks {
		taskMap[task.Name] = task
	}
	return taskMap
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
