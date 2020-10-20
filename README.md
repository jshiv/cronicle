# cronicle
git integrated distributed workflow scheduler that provides a pull model for CI/CD and versioning on job execution.

Usage

The tool will use a cronicle.hcl file to maintain a `schedule as code`.

`cronicle init --path cron` will produce a default file:
```hcl
//cronicle.hcl
queue {}

schedule "example" {
  cron       = "@every 5s"

  task "hello" {
    command = ["python", "run.py"]
    repo    = "https://github.com/jshiv/cronicle-sample.git"
    retry {}
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

## Breakdown of `cronicle.hcl`

### `remote` (optional)
__Note: setting remote requires that any changes to the cronicle repo to be made through 
the remote git repo, any local changes will be removed by `git checkout`.__
```hcl
// remote enables the cronicle.hcl file to be tracked by a remote git repo
// a heartbeat process will fetch and refresh the config from this remote.
remote = "https://github.com/jshiv/cronicle-sample.git"
```

### `repos` (optional)
```
// repos is a list of remote repositories containing schedules
// that will be added to the main cron.
repos = [
    "https://github.com/jshiv/cronicle-sample.git",
]
```

### timezone (optional)
```
// timezone sets the timezone location to run cron and execute tasks by.
// default local
timezone = ""
```

### schedule
```
schedule "foo" {
  cron       = "@every 5s"
  timezone   = ""
  start_date = ""
  end_date   = ""
  repo       = ""

  task "bar" {
    command = ["/bin/echo", "Hello World --date=${date}"]
    depends = null
    
	  repo {
		  url = ""
      key = ""
      branch = ""
      commit = ""
	  }

    retry {
      count   = 0
      seconds = 0
      minutes = 0
      hours   = 0
    }
  }
}
```


# Bash Commands

The init command sets up a new schedule repository with a sample conicle.hcl file
```bash
cronicle init
tree
.
├── cronicle.hcl
└── .repos
```

the run command starts the scheduler.
```bash
cronicle run
```

the exec command will execute a named task/schedule for a given time or daterange.
```bash
cronicle exec --task bar
```

the worker will start a schedule consumer when `cronicle run --queue ` is in distributed mode.
```bash
cronicle worker --queue redis
```




