Cronicle
---
Production-grade scheduling for AI agents. Cron triggers, git versioning, HCL config, slog audit trails, per-run cost ceilings — plus native [Anthropic Agent Skills](https://docs.claude.com/en/docs/agents-and-tools/agent-skills) and [Model Context Protocol](https://modelcontextprotocol.io) server support.

Shell tasks still work the way they always did. Agent tasks share the same scheduler, the same git/HCL config, the same audit trail — composed from one declarative runtime.

---

[![PkgGoDev](https://pkg.go.dev/badge/github.com/jshiv/cronicle)](https://pkg.go.dev/github.com/jshiv/cronicle)

## Install

One-liner (Linux, macOS):

```bash
curl -fsSL https://raw.githubusercontent.com/jshiv/cronicle/master/install.sh | sh
```

Detects OS/arch, downloads the matching release artifact, installs to `/usr/local/bin` (or `$HOME/.local/bin` if `/usr/local/bin` isn't writable). Pin a version with `CRONICLE_VERSION=v0.4.0` or override the path with `CRONICLE_INSTALL_DIR=$HOME/.local/bin`.

Manual install: download the matching tarball from the [releases page](https://github.com/jshiv/cronicle/releases/latest) and place the `cronicle` binary on your `PATH`.

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
* [Distribute cronicle tasks with a Redis broker (docker-compose)](deploy/redis/README.md)
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

    // Tools available to the agent. Omit to default to local-only:
    //   bash         — run shell commands in the task workspace
    //   text_editor  — view/create/edit files (workspace-confined)
    //   git          — read+write git operations via embedded go-git
    //                  (no host git CLI required)
    // Opt-in (server-side, billed per call on Anthropic):
    //   web_search   — server-side web search
    //   web_fetch    — server-side URL fetch
    tools = ["bash", "text_editor", "git", "web_search"]

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

#### Built-in `git` tool

Cronicle ships with [go-git](https://github.com/go-git/go-git) embedded — the same library that powers the `repo` block — and exposes it to agents as a `git` tool. Agents can read and modify a repo without the host having `git` installed, preserving cronicle's single-binary-on-a-bare-machine property.

Subcommands:

| | |
|---|---|
| `status` | List tracked changes (porcelain-style summary) |
| `log` | Recent commits in `<sha> <subject>` form (default 10, max 200) |
| `diff` | Unified diff. `from`/`to` are refs (branch, hash, `HEAD~N`); default is HEAD vs working tree |
| `branch` | Create + switch to a new branch from current HEAD |
| `commit` | Stage all worktree changes and commit |
| `push` | Push current branch using the same auth as the task's `repo` block |

Example:

```hcl
agent {
  prompt = "Summarize the last 10 commits as a 5-bullet changelog at ./CHANGELOG.md."
  tools  = ["git", "text_editor"]
}
```

In the pretty stream, git calls render as `→ git: <subcommand> <key arg>`:

```
→ git: log
← exit=0 4ms
→ git: diff abc1234~1..abc1234
← exit=0 6ms
→ editor: create ./CHANGELOG.md
← exit=0 0ms
```

`push` reuses the auth method captured at clone time; an agent that ran on a task with no `repo` block won't have credentials for a private remote.

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

## Remote triggers (HTTP)

`cronicle run` can expose a small REST API for firing schedules and tasks
on demand — useful for control-plane proxies, alert webhooks, or "rerun
this now" buttons in a UI. Triggered runs use the same queue path as
cron-fired runs, so they produce identical logs and DAG semantics.

```bash
cronicle run \
  --path cronicle.hcl \
  --listen :8765 \
  --listen-token "$CRONICLE_LISTEN_TOKEN"
```

The listener refuses to bind without a token (an open trigger endpoint
on an unattended cron service is a foot-cannon). Pass it via the flag or
set `CRONICLE_LISTEN_TOKEN` in the environment.

| Method | Path                                                   | Purpose                              |
|--------|--------------------------------------------------------|--------------------------------------|
| GET    | `/healthz`                                             | Liveness (no auth)                   |
| GET    | `/v1/schedules`                                        | List configured schedules + tasks    |
| POST   | `/v1/schedules/{name}/trigger`                         | Fire the whole schedule (full DAG)   |
| POST   | `/v1/schedules/{name}/tasks/{task}/trigger`            | Fire one task (depends stripped)     |
| GET    | `/v1/runs`                                             | List recent runs (filterable)        |
| GET    | `/v1/runs/{run_id}`                                    | Single run + per-task detail         |
| POST   | `/v1/events`                                           | JSONL ingest (batched events)        |
| GET    | `/v1/jobs?worker=&block=`                              | Long-poll job claim                  |
| POST   | `/v1/jobs/{run_id}/ack`                                | Worker reports completion            |
| POST   | `/v1/jobs/{run_id}/heartbeat`                          | Visibility-timeout renewal           |

Auth is bearer-token (`Authorization: Bearer <token>`). Rotate by
restarting the process.

```bash
curl -X POST \
  -H "Authorization: Bearer $CRONICLE_LISTEN_TOKEN" \
  http://localhost:8765/v1/schedules/daily-report/trigger
# -> 202 Accepted {"queued":"daily-report","schedule":"daily-report"}
```

In distributed mode (`--queue redis|nsq`) the listener pushes to the
broker, so any worker consuming the queue picks the trigger up — same
as a cron tick.

---

## Run state (HTTP)

Every fire of a schedule — cron tick, HTTP trigger, or `cronicle exec`
— gets a `run_id` and is folded into a SQLite-backed projection at
`<cronicle-path>/.cronicle/state.db`. The projection is a derived view
of the slog event stream (`task_start`, `shell_run`, `agent_run`,
`schedule_complete`); the JSONL log on disk remains authoritative for
what happened, and the state DB is rebuildable from incoming events.

Query via the HTTP listener (auth same as triggers):

```bash
# all recent runs, newest first
curl -s -H "Authorization: Bearer $TOKEN" http://localhost:8765/v1/runs

# filter
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8765/v1/runs?status=failed&schedule=daily&limit=20"

# one run with per-task detail
curl -s -H "Authorization: Bearer $TOKEN" \
  http://localhost:8765/v1/runs/$RUN_ID
```

Response shape (list):

```json
[
  {
    "run_id": "20260510T182254Z-29c5ba99",
    "schedule": "daily-report",
    "status": "succeeded",
    "source": "http",
    "started_at": "2026-05-10T18:22:54.212857Z",
    "ended_at":   "2026-05-10T18:22:54.216826Z",
    "duration_ms": 3,
    "cost_usd": 0.001050,
    "task_count": 5
  }
]
```

`status` is one of `queued | running | succeeded | failed | canceled`.
`source` is `cron | http | exec | once`. Filters: `status=`,
`schedule=`, `since=<RFC3339>`, `limit=<n>` (default 50, max 500).

`cronicle exec` uses an in-memory projection (the run is foreground, you
are watching it; nothing later queries the DB). `cronicle run` opens
`.cronicle/state.db` in WAL mode and persists across restarts.

### Posting events directly (POST /v1/events)

External producers — distributed workers (Phase 2 onwards), debug
shippers, integration tests — can write events into the projection
directly. Body is JSONL (one event per line, same shape as
`cronicle.jsonl` on disk):

```bash
# Replay the local log file into a remote producer:
tail -F .cronicle/log/cronicle.jsonl | \
  curl -s -X POST \
    -H "Authorization: Bearer $TOKEN" \
    --data-binary @- \
    http://producer.internal:8765/v1/events

# Or post a one-off batch:
curl -X POST \
  -H "Authorization: Bearer $TOKEN" \
  --data-binary @batch.jsonl \
  http://localhost:8765/v1/events
# -> {"accepted":4,"dropped":0}
```

Lines that don't parse, or that lack `entry_type` / `run_id`, are counted
as `dropped` but the rest still apply (the projection is happy to ignore
unknown fields, so the JSONL log format is the only contract). Body
limit is 16 MiB per request; oversized bodies return 413. The endpoint
is idempotent at the row level — re-POSTing the same events updates
the same projection rows monotonically.

---

## Distributed mode without a broker (`--queue self`)

Run `cronicle run --queue self` and the producer becomes its own queue.
Cron-tick fires + HTTP triggers enqueue into the SQLite jobs table at
`<cronicle-path>/.cronicle/state.db`. Workers — local goroutines or
remote `cronicle worker` processes — claim jobs over HTTP long-poll,
execute, ship events back, and ack. No Redis, no NSQ, no Sentinel.

```bash
# Producer: state plane + queue + listener, in-process worker disabled
cronicle run \
  --path cronicle.hcl \
  --listen :8765 --listen-token "$TOKEN" \
  --queue self \
  --worker=false

# Remote worker: long-polls /v1/jobs, executes, posts events back
cronicle worker \
  --path /local/repo \
  --producer http://producer:8765 \
  --producer-token "$TOKEN" \
  --worker-id node-1
```

How it works:

- **Atomic claim**: `BEGIN IMMEDIATE; UPDATE jobs SET status='claimed', claimed_by=? WHERE id=? AND status='pending'`. SQLite WAL serializes writers, so two concurrent workers cannot both acquire the same row. The losing worker's long-poll sees no row and reconnects.
- **Visibility timeout**: claimed jobs expire after 5 minutes. A janitor goroutine sweeps every 10 seconds, moving expired claims back to `pending`. A worker dies → its job is re-dispatched. Long agent runs `POST /v1/jobs/{id}/heartbeat` to extend the deadline.
- **Event shipping**: workers tee their slog event stream to the producer via `POST /v1/events` (JSONL, batched every 500ms or 64 events). Producer's projection reflects what the worker actually did. Events also write to the worker's local `cronicle.jsonl` for on-host audit.
- **Two persistent connections per worker**: the rolling `GET /v1/jobs` long-poll and the slog→events shipper. Plus short-lived `POST /v1/jobs/{id}/ack` and `/heartbeat`. No SSE control channel yet (that's Phase 3, for cancel signals).

For comparison: legacy `--queue redis` and `--queue nsq` still work but are scheduled for removal.

---

## Command Templates
The cronicle command string accepts the following template argumets
```
	 ${date}: 		  "2006-01-02"
	 ${datetime}: 	"2006-01-02T15:04:05Z07:00"
	 ${timestamp}: 	"2006-01-02 15:04:05Z07:00"
	 ${path}:       task.Path
```




