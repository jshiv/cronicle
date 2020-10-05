# cronicle
git integrated distributed workflow scheduler that provides a pull model for CI/CD and versioning on job execution.

Usage

The tool will use a cronicle.hcl file to maintain a `schedule as code`.

`cronicle init --path cron` will produce a default file:
```hcl
#cronicle.hcl 

schedule "foo" {
  cron       = "@every 5s"

  task "bar" {
    command = ["/bin/echo", "Hello World --date=${date}"]
  }
}
```

`cronicle run --path cron/cronicle.hcl`
```
INFO[2020-10-05T05:24:33Z] Starting Scheduler...                         cronicle=start
INFO[2020-10-05T05:24:33Z] Loading config...                             cronicle=heartbeat path=./cron/cronicle.hcl
INFO[2020-10-05T05:24:33Z] Refreshing config...                          cronicle=heartbeat path=./cron/cronicle.hcl
INFO[2020-10-05T05:24:38Z] Queuing...                                    schedule=foo
INFO[2020-10-05T05:24:38Z] Hello World --date=2020-10-05                 commit=null email=null exit=0 schedule=foo success=true task=bar
```

## Breakdown of `cronicle.hcl`

### remote (optional)
```hcl
// remote enables the cronicle.hcl file to be tracked by a remote git repo
// a heartbeat process will fetch and refresh the config from this remote.
remote = "https://github.com/jshiv/cronicle-sample.git"
```

### repos (optional)
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
    repo    = ""
    branch  = ""
    commit  = ""

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


