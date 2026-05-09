Cronicle
---
git integrated distributed workflow scheduler that provides a pull model for CI/CD and versioning on job execution.

---

[![PkgGoDev](https://pkg.go.dev/badge/github.com/jshiv/cronicle)](https://pkg.go.dev/github.com/jshiv/cronicle)

## Install

Linux
```bash
wget -c https://github.com/jshiv/cronicle/releases/download/v0.3.8/cronicle_0.3.8_Linux_x86_64.tar.gz -O - | tar -xz
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
21:44:16 config loaded path=./cronicle/cronicle.hcl schedules=1 tasks=1
21:44:16 Starting Scheduler... cronicle=start
21:44:16 Starting cron... schedule=example cron="@every 5s"
21:44:21 Queuing... schedule=example
──── 21:44:21 · schedule "example" ──────────────────────────────────
DAG:
  └─ hello

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
shell run · schedule=example · task=hello · python run.py
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

X: 0.360346904169

[exit=0 · 12ms]

✓ schedule "example" complete · 1 task · 0.5s
```

Agent tasks render in the same shape, with token usage and cost in the footer:
```
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
agent run · schedule=example · task=summarize · model=claude-opus-4-7
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

Today's deploy added agent task support, fixed a queue race, and bumped Go to 1.26.

[64 in / 25 out tokens · $0.001050 · 873ms · stop=end_turn]
```

### Output formats

`--log-format` controls stdout (default `auto` — pretty when at a TTY, text when piped):

| flag | shape | best for |
|---|---|---|
| `--log-format=pretty` | bordered blocks, dim lifecycle | humans at a TTY |
| `--log-format=text` | `time=... level=INFO msg=... key=val` | piping to a log consumer |
| `--log-format=json` | one JSON object per line | strictest machine parsing |

`--log-to-file` is independent of stdout: when set, structured JSON is mirrored to `.cronicle/log/cronicle.jsonl` (rotated by lumberjack: 500MB × 3 backups × 28 days, gzipped). Pretty stdout + tail-able JSON file at the same time is the intended composition.

When `--log-to-file` is on, each task execution also writes a per-run JSONL transcript at `.cronicle/runs/{ts}-{schedule}-{task}.jsonl` (request / response / accounting). Without it, `cronicle exec` is fully ephemeral and writes nothing to disk.

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

### `agent` (optional)
Run a Claude agent invocation in place of a shell command. A task may have an
`agent` block or a `command`, but not both. `${date}`, `${datetime}`, and
`${timestamp}` are substituted into `prompt` and `system` at execution time.
With `--log-to-file`, each run writes a JSONL transcript (request, response,
token usage, cost) to `.cronicle/runs/` next to the cronicle root. Requires
`ANTHROPIC_API_KEY` in the environment.

```hcl
task "summarize" {
  agent {
    prompt     = "Summarize what changed today: ${date}"
    model      = "claude-opus-4-7"
    system     = "You are a concise release-notes writer."
    max_tokens = 2048
    // abort the run if its actual cost exceeds this; 0 disables
    budget_usd = 0.50
  }
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




