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

Detects OS/arch, downloads the matching release artifact, installs to `/usr/local/bin` (or `$HOME/.local/bin` if `/usr/local/bin` isn't writable). Pin a version with `CRONICLE_VERSION=v0.5.0` or override the path with `CRONICLE_INSTALL_DIR=$HOME/.local/bin`.

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

* [Distributed mode walkthrough — multi-worker fan-out + cancel/retry/resume](deploy/distributed/README.md)
* [MCP + skills + scratch in one task — minimal live demo](deploy/mcp-demo/README.md)
* [Daily-report agent fan-out + composer demo](deploy/daily-report/README.md)
* [Centralize cronicle logs on a local loki/graphana log aggregator](deploy/local/README.md)
* [SSE live-stream demo — per-run, per-schedule, firehose filters](deploy/sse-demo/README.md)


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

### Template variables

Cronicle substitutes the following placeholders in `task.command`,
`agent.prompt`, and `agent.system` strings at execution time:

| variable      | resolves to                                        |
|---------------|----------------------------------------------------|
| `${date}`     | `YYYY-MM-DD` (UTC) of the schedule trigger time    |
| `${datetime}` | RFC3339 timestamp of the schedule trigger time     |
| `${timestamp}`| Unix epoch (seconds) of the schedule trigger time  |
| `${path}`     | the task's working directory (after any `repo` clone) |
| `${scratch}`  | a schedule-scoped shared directory (see below)     |

Unresolved placeholders (e.g. `${scratch}` used when no schedule context
is available) are left as the literal string — loud enough to debug.

#### `${scratch}` — pass artifacts between tasks in one run

Every fire of a schedule gets its own scratch directory at
`<croniclePath>/.cronicle/scratch/<schedule>/<run-timestamp>/`. All
tasks within that run see the same path. Tasks earlier in the DAG can
write files there; downstream tasks read them. The directory persists
across the run for transcript / audit access; the janitor can prune
old runs.

```hcl
schedule "report" {
  cron = "@every 1h"

  task "fetch" {
    // Writes intermediate output to scratch
    command = ["sh", "-c", "curl -s api.example.com/data > ${scratch}/raw.json"]
  }

  agent "summarize" {
    depends = ["fetch"]
    prompt  = <<-EOT
      The fetched data is at ${scratch}/raw.json.
      Read it with the bash tool, then write a 3-bullet summary to
      ${scratch}/summary.md.
    EOT
    model     = "claude-haiku-4-5"
    tools     = ["bash", "text_editor"]
    max_turns = 8
  }

  task "publish" {
    depends = ["summarize"]
    command = ["sh", "-c", "cp ${scratch}/summary.md /var/www/today.md"]
  }
}
```

For a fuller worked example, see [deploy/mcp-demo](deploy/mcp-demo/README.md)
and [deploy/daily-report](deploy/daily-report/README.md).

### `agent` (first-class)
Run a Claude agent task. `agent` is a sibling of `task` at the schedule level
— same DAG, same scheduler, same telemetry — with agent-specific fields
(`prompt`, `model`, `tools`, `skills`, `mcp`, …) at the top of the block
instead of buried in a nested `agent { }`. Shell tasks still use `task`;
agent tasks use `agent`. Pick whichever describes the work.

`${date}`, `${datetime}`, `${timestamp}`, `${path}`, and `${scratch}` are
substituted into `prompt` and `system` at execution time (see
[Template variables](#template-variables)). The agent runs as a multi-turn loop:
it can think, call tools, observe results, and continue until it stops calling
tools, hits `max_turns`, or the `wallclock` deadline fires. With
`--log-to-file`, each run writes a JSONL transcript (request, response per
turn, tool results, accounting) to `.cronicle/runs/`. Requires
`ANTHROPIC_API_KEY` in the environment.

```hcl
schedule "morning" {
  cron = "0 8 * * *"

  agent "brief" {
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

#### Legacy nested form

The original shape — `task "name" { agent { ... } }` — still parses for
backward compatibility. Both forms produce the same internal `Task` with
`Task.Agent` populated; downstream code (DAG, exec, projection, SSE) is
unaware which spelling was used. New configs should prefer the first-class
`agent "name" { }` shape; existing configs need no migration.

```hcl
// Legacy form, still works.
schedule "morning" {
  cron = "0 8 * * *"
  task "brief" {
    agent {
      prompt = "Compose today's morning brief."
      model  = "claude-opus-4-7"
    }
  }
}
```

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

The `worker` command runs a remote consumer that long-polls a producer started with `--listen + --listen-token`.
```bash
cronicle worker --producer http://producer:8765 --producer-token "$CRONICLE_LISTEN_TOKEN"
```
See "Distributed mode without a broker" below for the full setup.

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
| GET    | `/v1/runs/{run_id}/events`                             | SSE: live frames for one run         |
| GET    | `/v1/schedules/{name}/events`                          | SSE: live frames for every run of a schedule (incl. next run) |
| GET    | `/v1/events/stream`                                    | SSE: firehose — every run on every schedule |
| POST   | `/v1/events`                                           | JSONL ingest (batched events)        |
| GET    | `/v1/jobs?worker=&block=`                              | Long-poll job claim                  |
| POST   | `/v1/jobs/{run_id}/ack`                                | Worker reports completion            |
| POST   | `/v1/jobs/{run_id}/heartbeat`                          | Visibility-timeout renewal           |
| GET    | `/v1/workers`                                          | Registry: who's connected, what they ran |
| GET    | `/v1/workers/{id}/control`                             | SSE; producer pushes cancel signals  |
| POST   | `/v1/runs/{run_id}/cancel`                             | Stop a pending or in-flight run      |
| POST   | `/v1/runs/{run_id}/retry`                              | Re-enqueue a terminal run from scratch |
| POST   | `/v1/runs/{run_id}/resume`                             | Re-enqueue only the tasks that didn't succeed |

Auth is bearer-token (`Authorization: Bearer <token>`). Rotate by
restarting the process.

```bash
curl -X POST \
  -H "Authorization: Bearer $CRONICLE_LISTEN_TOKEN" \
  http://localhost:8765/v1/schedules/daily-report/trigger
# -> 202 Accepted {"queued":"daily-report","schedule":"daily-report"}
```

In distributed mode (when the listener is up), the producer writes the
schedule to the SQLite jobs table, and any worker long-polling
`/v1/jobs` picks it up — same as a cron tick.

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

### Live event stream

Server-Sent Events stream of task output as it happens — token-by-token
for agents, line-by-line for shell. The wire bytes are whatever
`--live-format` produces (default `pretty`: ANSI-colored multi-line
text identical to what a TTY would show). Frontends with a terminal-
emulator widget (e.g. xterm.js) render the stream as a faithful mirror
of the producer's console.

Three subscription scopes, each its own endpoint:

```bash
# Drill into one specific run (only works after the run exists)
curl -N -H "Authorization: Bearer $TOKEN" \
  http://localhost:8765/v1/runs/$RUN_ID/events

# Watch every run of a schedule — including the NEXT run before its
# run_id exists. Open this BEFORE triggering; the SSE handler routes
# by schedule name, so the next-run's frames arrive without polling.
curl -N -H "Authorization: Bearer $TOKEN" \
  http://localhost:8765/v1/schedules/daily/events

# Firehose — every run on every schedule. Dashboard view.
curl -N -H "Authorization: Bearer $TOKEN" \
  http://localhost:8765/v1/events/stream
```

A typical frame:

```
event: cronicle
data: hello from tick

event: cronicle
data: step 2
```

Multi-line records (a pretty-rendered shell_run header, or an agent text
delta containing `\n`) become one event with multiple `data:` lines per
the SSE spec — browser `EventSource` joins them with `\n` automatically.

#### Filter scope — when to use which endpoint

| You want | Endpoint | Notes |
|---|---|---|
| Watch one specific in-flight run (operator clicked it from a list) | `GET /v1/runs/{run_id}/events` | Drill-in. Requires `run_id` to already exist — won't help for the next run. |
| Watch every run of a schedule, including the next one before it fires | `GET /v1/schedules/{name}/events` | Subscribe BEFORE triggering. The handler routes by schedule name, so a run that doesn't have a `run_id` yet still delivers frames the moment it starts emitting. |
| Operator dashboard — every run on every schedule | `GET /v1/events/stream` | Firehose. Concurrent runs interleave by event-time. |

All three share the same SSE plumbing — same wire format, same `--live-format` controlling the encoding, same 15s `: ping` heartbeat. Frontend code differs only in the URL.

#### Worked example — multi-schedule walkthrough

Set up a `cronicle.hcl` with three schedules:

```hcl
schedule "heartbeat" {
  cron = "@every 10s"
  task "tick" {
    command = ["sh", "-c", "echo 'tick'; sleep 1; echo 'second line'"]
  }
}

schedule "long-build" {
  cron = ""  // manual trigger
  task "compile" {
    command = ["sh", "-c", "for s in fetching compiling linking testing done; do echo \"[step] $s\"; sleep 1; done"]
  }
}

schedule "story" {
  cron = ""  // manual trigger
  task "tell" {
    agent {
      prompt    = "Tell a two-sentence story about an octopus debugging Go."
      model     = "claude-haiku-4-5"
      max_turns = 1
    }
  }
}
```

```bash
cronicle run --path cronicle.hcl --listen :7766 --listen-token smoke
```

**Per-run** — trigger `long-build`, capture its `run_id`, subscribe to just that one:

```bash
curl -X POST -H "Authorization: Bearer smoke" \
     http://localhost:7766/v1/schedules/long-build/trigger
RUN_ID=$(curl -s -H "Authorization: Bearer smoke" \
     "http://localhost:7766/v1/runs?status=running&limit=1" | jq -r '.[0].run_id')
curl -N -H "Authorization: Bearer smoke" \
     "http://localhost:7766/v1/runs/$RUN_ID/events"
```

Frames you'll see: `shell_run_start` (header rule + command), six `stdout_chunk` lines (`[step] fetching` …), footer `[exit=0 · 6062ms · transcript=…]`, `schedule_complete` ✓.

**Per-schedule** — open SSE first, then trigger twice. Both runs flow through one connection:

```bash
( curl -N -H "Authorization: Bearer smoke" \
       http://localhost:7766/v1/schedules/long-build/events ) &
sleep 0.2
curl -X POST -H "Authorization: Bearer smoke" \
     http://localhost:7766/v1/schedules/long-build/trigger
sleep 7
curl -X POST -H "Authorization: Bearer smoke" \
     http://localhost:7766/v1/schedules/long-build/trigger
```

Two full run sequences come down the same stream — `schedule_start` / `shell_run_start` / chunks / footer / `schedule_complete`, then the second run's identical sequence. No polling for the second run's `run_id`.

**Firehose** — interleave heartbeat (auto-firing every 10s) with a manual `story` trigger:

```bash
( curl -N -H "Authorization: Bearer smoke" \
       http://localhost:7766/v1/events/stream ) &
sleep 0.2
curl -X POST -H "Authorization: Bearer smoke" \
     http://localhost:7766/v1/schedules/story/trigger
```

The stream interleaves `heartbeat`'s shell output with `story`'s agent `text_delta` tokens by event-time — you'll see frames from both runs alternating as they fire. That's the operator-dashboard view.

#### Wire format toggle (`--live-format`)

`--live-format` controls the bytes inside `data:` lines. Set at startup; the same format applies to every SSE consumer regardless of endpoint.

| flag                   | wire bytes                                                       | best for |
|------------------------|------------------------------------------------------------------|---|
| `--live-format=pretty` | ANSI multi-line (default)                                        | xterm.js / TTY clients |
| `--live-format=json`   | one JSON object per record (same shape as `cronicle.jsonl`)       | programmatic consumers, custom UIs |
| `--live-format=text`   | `time=... level=INFO msg=... key=val`                            | plain log viewers, debugging |

Architecture: cronicle keeps three planes for run telemetry, each tuned
to a different consumer:

| Plane         | Source                                       | Use case                                |
|---------------|----------------------------------------------|-----------------------------------------|
| **State**     | SQLite `runs` / `tasks` tables               | Red/green/blue status, filterable list  |
| **History**   | `cronicle.jsonl` → Loki + on-disk transcripts | Search & debug past runs                |
| **Live (this)** | LiveSink in-memory pub/sub                 | "What's happening right now?" — token-by-token, line-by-line |

The live stream is **live-only**. Disconnect = miss. If your tab drops
mid-task and reconnects, you start from "now"; switch to the Loki pane
in your UI for the missed window. This keeps the server stateless and
the wire format trivial.

Cross-plane linking: every record in the JSONL log (and therefore in
Loki) carries `seq` (per-process monotonic int64) and `lifetime`
(8-char hex nonce per producer process) attrs injected by the top-of-
chain `state.Tagger`. A frontend can deep-link from a live frame to
a Loki query scoped to the same `lifetime`+`seq` neighborhood.

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

## Distributed mode

There's no `--queue` flag. Queue mode is derived from whether the
HTTP listener is up:

- **`cronicle run`** alone → in-memory channel queue, single-process
  loop. Cron + trigger push through a Go channel that the in-process
  consumer drains. The state-plane projection (`runs`, `tasks`,
  `events` tables) is still on disk at `.cronicle/state.db`, but the
  jobs table is unused. Right shape for foreground demos.
- **`cronicle run --listen :PORT --listen-token TOKEN`** → SQLite
  jobs table queue + listener API. Cron + triggers enqueue durably.
  Local goroutines OR remote `cronicle worker` processes claim over
  HTTP long-poll, execute, ship events back, ack. Cancel / retry /
  resume work because payloads persist.

The intuition: exposing HTTP signals "I want remote control + remote
workers." Both need the durable queue; one flag answers both.

```bash
# Producer: listener on, in-process worker disabled — pure dispatcher
cronicle run \
  --path cronicle.hcl \
  --listen :8765 --listen-token "$TOKEN" \
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
- **Three persistent connections per worker**: the rolling `GET /v1/jobs` long-poll, the slog→events shipper, and the `GET /v1/workers/{id}/control` SSE channel for cancel signals. Plus short-lived `POST /v1/jobs/{id}/ack` and `/heartbeat`.

### Cancel and retry

```bash
# Stop a pending or in-flight run. If a worker holds the claim, the
# producer pushes "cancel run_id=X" over its SSE control channel; the
# worker's per-run context is canceled and the agent loop exits at the
# next ctx.Done() check between turns.
curl -X POST -H "Authorization: Bearer $TOKEN" \
  http://localhost:8765/v1/runs/$RUN_ID/cancel
# -> {"run_id":"...","worker_id":"ext-1","was_claimed":true,"status":"canceled"}

# Re-enqueue a terminal run (succeeded or failed) FROM SCRATCH.
# All tasks run again. Original run row stays in the projection.
curl -X POST -H "Authorization: Bearer $TOKEN" \
  http://localhost:8765/v1/runs/$RUN_ID/retry
# -> {"original_run_id":"...","new_run_id":"...","schedule":"daily"}

# Re-enqueue a terminal run BUT SKIP TASKS THAT ALREADY SUCCEEDED.
# Common operator workflow: cancel a stuck run, debug/fix the issue,
# resume from where you stopped without re-running the work that
# already completed. depends pointing at skipped tasks are stripped
# so the resumed DAG has correct entry points.
curl -X POST -H "Authorization: Bearer $TOKEN" \
  http://localhost:8765/v1/runs/$RUN_ID/resume
# -> {"original_run_id":"...","new_run_id":"...","schedule":"daily","skipped_tasks":["A","B"]}
```

`/resume` uses the **stored payload** from the original run, not the
current HCL — replay semantics, deterministic. If you need the latest
HCL (e.g. a config-level fix), `POST /v1/schedules/{name}/trigger`
fires a fresh run instead. Returns 400 with a clear message when every
task in the original already succeeded (nothing left to resume).

Cancel preempts shell tasks via `exec.CommandContext` + process-group
kill (SIGTERM, escalating to SIGKILL after 2 seconds). Sub-fans like
`bash -c "sleep 30 | cat"` die together because the child runs in its
own pgid on unix. For agent tasks, the loop checks `ctx.Done()` between
turns — a tool call already in flight finishes before the cancel
takes effect, but no further turns start. Cancel arrives via SSE
push (~ms latency); heartbeat-based 409 detection is the fallback when
the SSE channel is down (within one heartbeat cycle, ~100s).

### Worker registry

`GET /v1/workers` returns the connected workers with derived status:

```json
[
  {
    "worker_id": "ext-1",
    "host": "ec2-east-1",
    "status": "active",
    "last_seen": "2026-05-10T19:06:16Z",
    "current_run": "20260510T190616Z-...",
    "claimed_at": "2026-05-10T19:06:16Z",
    "runs_total": 42,
    "runs_failed": 1
  }
]
```

`status` is one of `active` (current_run set, last_seen recent), `idle`
(no current_run, last_seen recent), `stale` (last_seen older than
2 minutes — likely network partition or worker hung). Workers register
on first claim AND on SSE control-channel connect; a `stale` worker
might have hard-died, in which case the visibility-timeout reaper
recovers their in-flight job within 5 minutes.

As of v0.5, Redis and NSQ broker support has been removed. The vice
transport, the `--queue redis|nsq` flags, and the `deploy/redis` /
`deploy/nsq` docker-compose demos are gone. The `--queue` flag itself
was also dropped — queue mode is now derived from `--listen` presence
(see above). The `queue { ... }` HCL block is parsed for back-compat
but its fields have no effect.

---

## Command Templates
The cronicle command string accepts the following template argumets
```
	 ${date}: 		  "2006-01-02"
	 ${datetime}: 	"2006-01-02T15:04:05Z07:00"
	 ${timestamp}: 	"2006-01-02 15:04:05Z07:00"
	 ${path}:       task.Path
```




