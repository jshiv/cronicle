package config

// Config is the configuration structure for the cronicle checker.
// https://raw.githubusercontent.com/mitchellh/golicense/master/config/config.go
type Config struct {
	Version   string     `hcl:"version,optional"`
	Git       string     `hcl:"git"`
	Schedules []Schedule `hcl:"schedule,block"`
}

// Schedule is the configuration structure that defines a cron job consisting of tasks.
type Schedule struct {
	// Cron is the schedule interval. The field accepts standard cron
	// and other configurations listed here https://godoc.org/gopkg.in/robfig/cron.v2
	// i.e. ["@hourly", "@every 1h30m", "0 30 * * * *", "TZ=Asia/Tokyo 30 04 * * * *"]
	Name      string `hcl:"name,label"`
	Cron      string `hcl:"cron,optional"`
	StartDate string `hcl:"start_date,optional"`
	EndDate   string `hcl:"end_date,optional"`
	Owner     *Owner `hcl:"owner,block"`
	Tasks     []Task `hcl:"task,block"`
}

// Task is the configuration structure that defines a task (i.e., a command)
type Task struct {
	Name    string   `hcl:"name,label"`
	Command []string `hcl:"command,optional"`
	Depends []string `hcl:"depends,optional"`
	Owner   *Owner   `hcl:"owner,block"`
	Repo    string   `hcl:"repo,optional"`
	Branch  string   `hcl:"branch,optional"`
	Commit  string   `hcl:"commit,optional"`
	Path    string
}

// Owner is the configuration structure that defines an owner of a schedule or task
type Owner struct {
	Name  string `hcl:"name"`
	Email string `hcl:"email,optional"`
}

//Default returns a basic default Config
//it includes a single schedule that runs every 5 seconds
//and a single "Hello World" task.
func Default() Config {

	var task Task
	task.Name = "hello"
	task.Command = []string{"/bin/echo", "Hello World"}

	var schedule Schedule
	schedule.Name = "example"
	schedule.Cron = "@every 5s"
	schedule.Tasks = []Task{task}

	var conf Config
	conf.Schedules = []Schedule{schedule}

	return conf
}
