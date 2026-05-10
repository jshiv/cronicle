Cronicle
---
Production-grade scheduling for AI agents. Cron triggers, git versioning, HCL config, slog audit trails, per-run cost ceilings — plus native [Anthropic Agent Skills](https://docs.claude.com/en/docs/agents-and-tools/agent-skills) and [Model Context Protocol](https://modelcontextprotocol.io) server support.

Shell tasks still work the way they always did. Agent tasks share the same scheduler, the same git/HCL config, the same audit trail — composed from one declarative runtime.

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
Run a Claude agent in place of a shell command. A task has an `agent` block
*or* a `command`, never both. `${date}`, `${datetime}`, `${timestamp}`, and
`${path}` are substituted into `prompt` and `system` at execution time. The
agent runs as a multi-turn loop: it can think, call tools, observe results,
and continue until it stops calling tools, hits `max_turns`, or the
`wallclock` deadline fires. With `--log-to-file`, each run writes a JSONL
transcript (request, response per turn, tool results, accounting) to
`.cronicle/runs/`. Requires `ANTHROPIC_API_KEY` in the environment.

```hcl
task "morning_brief" {
  agent {
    prompt     = "Compose today's morning brief for ${date}."
    model      = "claude-opus-4-7"
    system     = "You are a concise operational assistant."

    // Tools available to the agent. Omit to default to all natives.
    //   bash         — run shell commands in the task workspace
    //   text_editor  — view/create/edit files (workspace-confined)
    //   web_search   — server-side web search (billed per call)
    //   web_fetch    — server-side URL fetch  (billed per call)
    tools = ["bash", "text_editor"]

    // Anthropic Agent Skills (progressive disclosure). Each entry is a
    // SKILL.md (workspace-relative); only frontmatter name+description
    // is injected into the system prompt. The agent calls load_skill to
    // fetch the body on demand. Skills ship bundled scripts/templates
    // alongside SKILL.md.
    skills = [
      "skills/morning-brief/SKILL.md",
      "skills/report-writer/SKILL.md",
    ]

    // Model Context Protocol servers — launched as subprocesses for
    // the lifetime of this run, with their tools auto-registered as
    // `<server>__<tool>`. See "MCP servers" below.
    mcp "fs" {
      command = ["npx", "-y", "@modelcontextprotocol/server-filesystem", "/data"]
    }

    max_turns  = 12       // hard cap on loop iterations
    wallclock  = "2m"     // duration; aborts the run when fired
    max_tokens = 2000     // per-turn output cap
    budget_usd = 0.10     // abort if cumulative cost exceeds this; 0 disables
  }
}
```

`prompt` is optional when `skills` is non-empty — the loaded skill drives
the run on its own. Skill paths must resolve under the task workspace; `..`
traversal and absolute paths are rejected at config load.

#### Skill layout

Skills follow the [Anthropic Agent Skills standard](https://docs.claude.com/en/docs/agents-and-tools/agent-skills):

```
skills/
└── morning-brief/
    ├── SKILL.md         # YAML frontmatter + markdown body
    └── scripts/
        └── today.sh     # bundled executable the body references
```

```markdown
---
name: morning-brief
description: Compose a 3-bullet morning brief for today's date.
allowed-tools:
  - bash
  - text_editor
---

# Morning Brief

Use `scripts/today.sh` to get today's date, then list exactly three bullets
and reply with `BRIEF COMPLETE`.
```

When the agent calls `load_skill` with `"morning-brief"`, the response
carries a `DIRECTORY:` header (`skills/morning-brief/`) so the agent
composes paths to bundled scripts correctly across multi-skill runs.

#### Run shape

```
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
agent run · schedule=daily · task=morning_brief · model=claude-haiku-4-5 · skills=[morning-brief]
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

I'll load the morning-brief skill and follow its instructions.
→ skill: morning-brief
← exit=0 0ms

— turn 2 —
→ bash: skills/morning-brief/scripts/today.sh
2026-05-10
← exit=0 20ms

— turn 3 —
- On 2026-05-10, ...
- ...
BRIEF COMPLETE

[5742 in / 247 out tokens · $0.006977 · 3705ms · stop=end_turn]
```

The `agent_run` slog event carries `skills_available` (the catalog the agent
saw) and `skills_loaded` (the subset whose bodies it actually fetched), so
unattended runs are auditable: *did the 3am job actually use what it had?*

#### MCP servers

[Model Context Protocol](https://modelcontextprotocol.io) servers are launched
as subprocesses for the lifetime of an agent run. Cronicle speaks JSON-RPC
over their stdin/stdout via the [official Go MCP SDK](https://github.com/modelcontextprotocol/go-sdk),
lists the tools each server advertises, and registers them with the agent
namespaced as `<server-name>__<tool-name>` so multiple servers compose
without name collisions.

```hcl
agent {
  prompt = "Triage open issues; close stale ones older than 90 days."

  mcp "github" {
    command = ["npx", "-y", "@modelcontextprotocol/server-github"]
    env     = ["GITHUB_TOKEN"]   // forward only what the server needs
  }

  mcp "fs" {
    command = ["npx", "-y", "@modelcontextprotocol/server-filesystem", "/data"]
  }
}
```

Lifecycle and isolation:

- **Per-run subprocess** — each server starts when the agent run begins and shuts down when it ends. The SDK's `CommandTransport` closes stdin then SIGTERMs after a 5s grace period.
- **Cancellation cascade** — servers run under the same context as the agent loop, so `wallclock` cancellation tears them down.
- **Fail loudly** — if any server fails to start or fails to list its tools, already-running peers are closed and the run aborts before any API call. There is no partial-MCP state where the agent sees half a tool surface.
- **Env opt-in** — only env-var names listed in `mcp.env` are forwarded from cronicle's environment, plus `PATH` so the binary resolves. The task's HCL `env` is also passed through. Wholesale env forwarding would leak secrets into untrusted server processes.

In the pretty stream, MCP tool calls render with `<server>.<tool>`:

```
→ fs.list_directory: {"path":"/data"}
← exit=0 3ms

→ github.create_issue: {"title":"...","body":"..."}
← exit=0 412ms
```

The `agent_run` slog event carries `mcp_servers=[fs,github]` so an audit
can answer *which servers the 3am job had available* — independent of
whether the agent actually invoked any of their tools.

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




