Cronicle
---
git integrated distributed workflow scheduler that provides a pull model for CI/CD and versioning on job execution.

---

[![PkgGoDev](https://pkg.go.dev/badge/github.com/jshiv/cronicle)](https://pkg.go.dev/github.com/jshiv/cronicle)

## Install

Linux
```bash
wget -c https://github.com/jshiv/cronicle/releases/download/v0.3.6/cronicle_0.3.6_Linux_x86_64.tar.gz -O - | tar -xz
sudo mv cronicle /usr/local/bin/cronicle
```

Mac/Windows download from the [releases page.](https://github.com/jshiv/cronicle/releases/latest)

## Quick start
```bash
cronicle run --command "/bin/echo cronicle" --cron "@every 5s"
```

The `cronicle.hcl` file maintains the `schedule as code` for task execution.

`cronicle init --path cron` will produce a default file:
```hcl
//cronicle.hcl
schedule "example" {
  cron       = "@every 5s"

  task "hello" {
    command = ["python", "run.py"]
    repo {
      url = "https://github.com/jshiv/cronicle-sample.git"
    }
  }
}
```

`cronicle run --path cron/cronicle.hcl`
```
INFO[2020-10-06T21:44:16-07:00] Starting Scheduler...                         cronicle=start
INFO[2020-10-06T21:44:16-07:00] Loading config...                             cronicle=heartbeat path=./cronicle.hcl
INFO[2020-10-06T21:44:21-07:00] Queuing...                                    schedule=example
INFO[2020-10-06T21:44:21-07:00]                                               attempt=1 schedule=example task=hello
INFO[2020-10-06T21:44:21-07:00] X: 0.360346904169                             commit=f99ad6af7de email=jason.shiverick@gmail.com exit=0 schedule=example success=true task=hello
```

---


## Example Deployments

* [Centralize cronicle logs on a local loki/graphana log aggregator](deploy/local/README.md)
* [Distribute cronicle tasks with nsq message broker](deploy/nsq/README.md)


---

## Breakdown of `cronicle.hcl`


### `repo` (optional)
A `repo` block is available at the `config`, `schedule` and `task` level but the behavior is different depending on which level it is assigned.
At the `config` level, a `repo` block enables the `cronicle.hcl` file to be tracked by a remote git repo, a heartbeat process will fetch and refresh the cronicle.hcl from the remote `repo`. At the `schedule` level, the `repo` block will be used as a default `repo` for any `tasks` that do not have an explicitly assigned `repo` block. At the `task` level a `repo` block will override the default `repo` with any details given.
_Note: setting remote requires that any changes to the cronicle repo to be made through 
the remote git repo, any local changes will be removed by `git checkout`._
```hcl
repo {
  // url or path to a remote git repository
  url    = "git@github.com:jshiv/cronicle-sample.git"

  // local ssh private key with read access to remote private repo
  key    = "~/.ssh/id_rsa"

  // branch to checkout for execution
  branch = ""

  // commit to checkout for execution, mutually exclusive to branch
  commit = ""
}
```


### `task`
Contains the executable command, dependency relationship between tasks, 
a repo to execute the command against, 
```hcl
task "bar" {
  //executable command
  command = ["/bin/echo", "Hello World --date=${date}"]

  //dependency relationship between tasks
  depends = ["baz"]
  
  //git repo containing source code to clone/fetch on execution
  repo ...

  // retry count and wait
  retry ...
}
```

### `schedule`
`schedule` is the block that sets the crontap. `task` blocks are contained within the `schedule` block.
```hcl
schedule "foo" {
  // crontab for scheduling execution, accpets Cron experessions, @every, @once, ""
  //cron = "@once" will execute the schedule on the first invocation of `cronicle run`
  //cron = "" will only execute the schedule/task with `cronicle exec`. Useful when useing cronicle to codify non-scheduled commands.
  cron       = "@every 5s"

  // IANA Time Zone
  timezone   = ""

  // Define the window in which the schedule is valid.
  // Outside of this window, tasks will not execute and a warring will be logged.
  start_date = ""
  end_date   = ""

  // Default repo for all tasks in schedule "foo"
  repo {
    ...
  }

  // task "bar" will execute "@every 5s"
  task "bar" {
    ...
  }
  
  // task "baz" will execute in parallel with task "bar"
  task "baz" {
    ...
  }

  // task "last" will execute only after "bar" and "baz" succeed 
  task "last" {
    ...
    depends = ["bar", "baz"]
  }
}
```


### `retry` (optional)
Number of retries and time to wait between.
```hcl
retry {
  count   = 1
  seconds = 30
  minutes = 0
  hours   = 0
}
```

### `timezone` (optional)
```hcl
// timezone sets the timezone location to run cron and execute tasks by.
// default local
timezone = "America/Los_Angeles"
```

### `heartbeat` (optional)
```hcl
// Cron expression to schedule the cronicle.hcl refresh task
heartbeat = "@every 60s"
```

---

## Bash Commands

The init command sets up a new schedule repository with a sample conicle.hcl file
```bash
cronicle init
tree
.
├── cronicle.hcl
└── .repos
```

The `run` command starts the scheduler.
```bash
cronicle run
```

The `exec` command will execute a named task/schedule for a given time or date range.
```bash
cronicle exec --task bar
```

The `worker` will start a schedule consumer when `cronicle run --queue ` is in distributed mode.
```bash
cronicle worker --queue redis
```

---

## Command Templates
The cronicle command string accepts the following template argumets
```
	 ${date}: 		  "2006-01-02"
	 ${datetime}: 	"2006-01-02T15:04:05Z07:00"
	 ${timestamp}: 	"2006-01-02 15:04:05Z07:00"
	 ${path}:       task.Path
```




