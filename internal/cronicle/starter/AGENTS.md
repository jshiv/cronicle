# Cronicle guide

This directory is a [cronicle](https://github.com/jshiv/cronicle) project.
`cronicle.hcl` defines schedules; each schedule is a DAG of tasks that
runs on a cron trigger. This file is for anyone, human or AI, adding or
changing those schedules. `cronicle guide` prints it again.

This guide covers the common paths. For anything more advanced, run
`cronicle <command> --help` (for example `cronicle run --help` or
`cronicle secret set --help`): each command lists every flag with its
default, plus examples.

## How to write schedules

Cronicle's output is built to make a run transparent: it draws the DAG,
then prints every task's exact command above that task's output, with a
copy-pasteable rerun line when a task fails. Write schedules that take
advantage of that.

1. **One task, one step.** A task should do one thing you can name in a
   word or two: `fetch`, `validate`, `load`, `notify`. If you'd describe
   it with "and", split it.
2. **Write the command inline.** Put the shell command or short script
   directly in `command`, using `sh -c`, `python3 -c`, or a heredoc, so
   the code that runs is the code a reader sees in `cronicle.hcl` and in
   the run output. A task that only says `python run.py` hides everything
   it does.
3. **Build complexity with `depends`, not inside one task.** Chain small
   tasks into a DAG. Independent steps run in parallel, a failure stops
   only what depends on it, and the output shows exactly which step broke.
4. **Fan out with `for_each`, not a loop.** When the same step runs for
   every item in a list (each PR, each region, each file), use `for_each`
   instead of a loop inside one task. Each item becomes its own task with
   its own output, status, and rerun line, so one bad item is visible and
   doesn't hide the rest. See [`for_each`](#for_each-one-task-per-item).
5. **Hand data between tasks through `${scratch}`.** Each run of a
   schedule gets its own shared directory. One task writes a file there,
   and the next one reads it.
6. **Split schedules by trigger.** Work that runs on a different cron, or
   is unrelated, belongs in its own `schedule` block.
7. **Reach for an external script only when you need one.** Long programs,
   code with its own tests, or code that lives in another repo can stay
   in files (use a `repo` block for a remote repo). Keep the DAG in
   `cronicle.hcl` and keep each script to one step.

Avoid: one schedule with one task that runs `python main.py`, where
`main.py` fetches, transforms, loads, and reports. Prefer:

```hcl
schedule "daily_sales" {
  cron = "0 6 * * *"

  task "fetch" {
    command = ["sh", "-c", "curl -fsS https://api.example.com/sales?date=${date} > ${scratch}/sales.json"]
  }

  task "validate" {
    depends = ["fetch"]
    command = ["python3", "-c", <<-EOT
      import json
      rows = json.load(open("${scratch}/sales.json"))
      assert rows, "no sales rows for ${date}"
      print(len(rows), "rows")
    EOT
    ]
  }

  task "load" {
    depends = ["validate"]
    command = ["sh", "-c", "psql \"$DATABASE_URL\" -c \"\\copy sales from '${scratch}/sales.json'\""]
    env     = ["DATABASE_URL=$secret.DATABASE_URL"]
  }

  agent "summary" {
    depends = ["validate"]
    model   = "claude-haiku-4-5"
    tools   = ["bash"]
    prompt  = "Read ${scratch}/sales.json and write a 3-bullet summary of ${date}'s sales."
  }

  task "notify" {
    depends = ["load", "summary"]
    command = ["sh", "-c", "echo \"sales for ${date} loaded\""]
  }
}
```

## Commands

This section shows the common flags. `cronicle --help` lists the
commands, and `cronicle <command> --help` (also for subcommands such as
`cronicle secret set --help`) shows every flag, its default, and
examples. Check it before guessing at a flag.

### Set up

```sh
cronicle init                         # starter cronicle.hcl, AGENTS.md, CLAUDE.md, .cronicle/
cronicle init --path ./ops            # in another directory
cronicle init --path ./ops \
  --clone git@github.com:org/ops.git --key ~/.ssh/deploy_key   # start from an existing repo
cronicle guide                        # print this guide
cronicle version                      # version and build info (also --version)
```

### Run once: `exec`

`exec` runs tasks now, in the foreground, and exits non-zero if any task
fails, so it works in scripts and CI. Use it to try a schedule while
writing it.

```sh
cronicle exec                                    # every schedule in ./cronicle.hcl
cronicle exec --path ops/cronicle.hcl            # another file
cronicle exec --schedule daily_sales             # one schedule's whole DAG
cronicle exec --schedule daily_sales --task load # one task alone: its depends don't run,
                                                 # and it gets a fresh, empty ${scratch}
cronicle exec --time 2026-01-01T06:00:00Z        # as if triggered at that time (${date} etc.)
cronicle exec --time 2026-01-01T06:00:00Z \
              --end  2026-01-07T06:00:00Z        # backfill: one run per day in the range
```

### Run on a schedule: `run`

`run` starts the scheduler and keeps going until stopped. It reloads
`cronicle.hcl` when the file changes, so edits take effect without a
restart. Ctrl-C lets in-flight tasks finish first.

```sh
cronicle run                                     # ./cronicle.hcl
cronicle run --path ops/cronicle.hcl --log-to-file
cronicle run --cron "@every 5m" --command "/bin/echo hi"   # one-off, no cronicle.hcl needed
cronicle run --config-source s3://bucket/cronicle.hcl --workdir /var/lib/cronicle
cronicle run --state-source postgres://user@db/cronicle    # durable run history
```

`--config-source` loads the config from `file://`, `http(s)://`, `s3://`,
or `postgres://` instead of `--path`, and `--workdir` sets where
`.cronicle/` lives in that case.

### HTTP API and distributed workers

`--listen` adds an HTTP API (bearer token required) and switches to a
durable job queue that remote workers can pull from:

```sh
cronicle run --listen :8765 --listen-token "$TOKEN"                 # API + local workers
cronicle run --listen :8765 --listen-token "$TOKEN" --worker=false  # dispatcher only
cronicle run --listen :8765 --listen-token "$TOKEN" --worker-count 8
cronicle worker --producer http://producer:8765 --producer-token "$TOKEN" \
                --path /srv/cronicle --worker-id node-1             # on another host
```

A worker long-polls the producer for jobs, runs them with the same
semantics as a local run, and ships its events back. If a worker dies,
its job is handed to another one after the visibility timeout (5m).

Main endpoints (all take `Authorization: Bearer $TOKEN`):

| request | does |
|---|---|
| `GET /healthz` | liveness, no auth |
| `GET /v1/schedules` | list schedules and tasks |
| `POST /v1/schedules/{name}/trigger` | run a schedule's whole DAG now |
| `POST /v1/schedules/{name}/tasks/{task}/trigger` | run one task now (depends stripped) |
| `GET /v1/runs`, `GET /v1/runs/{run_id}` | run history and per-task detail |
| `POST /v1/runs/{run_id}/cancel` | stop a pending or running run |
| `POST /v1/runs/{run_id}/retry` | run a finished run again from scratch |
| `POST /v1/runs/{run_id}/resume` | rerun only the tasks that didn't succeed |
| `GET /v1/runs/{run_id}/events` | live output of one run (SSE) |
| `GET /v1/schedules/{name}/events` | live output of every run of a schedule (SSE) |
| `GET /v1/events/stream` | live output of everything (SSE) |
| `GET /v1/workers` | connected workers and what they're running |

`--live-format` (`pretty`, `json`, or `text`) sets the bytes on the SSE
streams; `pretty` is the same ANSI output a terminal shows.

### Secrets

Secrets live in `.cronicle/state.db`, or on a remote producer with
`--remote URL --token T`. Reference them as `$secret.NAME` in `env` and
`repo` blocks; they're resolved when the task runs, never written into
`cronicle.hcl`.

```sh
printf %s "$DATABASE_URL" | cronicle secret set DATABASE_URL   # value from stdin
cronicle secret list                                           # names only
cronicle secret get DATABASE_URL
cronicle secret rm DATABASE_URL
cronicle secret set API_KEY --remote http://producer:8765 --token "$TOKEN"
```

`run`, `exec`, and `worker` can also pull secrets from elsewhere with
`--secret-source` (`env://`, `file:///abs/path.json`, `http(s)://…`, or
`state://`).

A missing secret fails the task with its name and the `secret set`
command to fix it. It is never passed on as the literal text.

#### Example: a token for private repos

A `repo` block for a private HTTPS repo reads its token from a secret:

```hcl
repo {
  url      = "https://github.com/org/app.git"
  password = "$secret.GITHUB_TOKEN"
}
```

Set it once per project. On your own machine, the GitHub CLI's token is
the quickest:

```sh
gh auth token --hostname github.com | cronicle secret set GITHUB_TOKEN --workdir .
```

That works in a fresh clone too: `secret set` creates `.cronicle/` when
the directory has a `cronicle.hcl`. Or skip storing it, and hand the
token over for a single run:

```sh
GITHUB_TOKEN=$(gh auth token) cronicle exec --secret-source env://
```

`gh auth token` is your personal token, with every scope you granted
`gh`. On a server or a shared producer, store a
[fine-grained token](https://github.com/settings/personal-access-tokens)
instead, limited to the repos the schedule clones (read access to
Contents; add Pull requests to comment on PRs). Remote workers get
secrets from their producer, so set it there, not on each worker.

### Output

All commands take `--log-format`: `auto` (the default: `pretty` in a
terminal, `text` when piped), `pretty`, `text` (`key=value` lines), or
`json` (one object per line). `--color auto|always|never` controls ANSI
color in `pretty`. `--log-to-file` on `run`, `exec`, and `worker` also
writes JSON logs to `.cronicle/log/cronicle.jsonl` and a transcript per
run to `.cronicle/runs/`.

## `cronicle.hcl` reference

```hcl
timezone       = "America/New_York"   // optional: default timezone for crons
heartbeat      = "@every 5m"          // optional: how often to log "alive" (default 5m)
config_refresh = "@every 1s"          // optional: how often to check the config for changes
repos          = ["https://github.com/org/team-schedules.git"]   // optional: see Repos

mcp "github" {                        // optional: an MCP server agents share by name
  url     = "https://api.githubcopilot.com/mcp/"   // (also allowed inside a schedule)
  headers = { Authorization = "Bearer $secret.GITHUB_TOKEN" }
}

schedule "name" {
  cron       = "0 6 * * *"       // cron, "@every 10m", "@hourly", "@once", or "" (exec/API only)
  timezone   = "UTC"             // optional
  start_date = "2026-01-01"      // optional: runs outside this window are skipped
  end_date   = "2026-12-31"

  task "step" {
    command             = ["sh", "-c", "echo hi"]   // argv; no shell unless you call one
    depends             = ["other_step"]            // run after these succeed
    continue_on_failure = false                     // true: run even if a dependency failed
    env                 = ["KEY=value", "TOKEN=$secret.TOKEN"]
    retry {                                         // try again on failure
      count   = 2                                   // extra attempts
      seconds = 30                                  // wait between (also minutes, hours)
    }
  }

  task "per_item" {
    for_each     = { for x in task.step.output : x.id => x }  // one task per item: see for_each
    max_parallel = 4                                // at most 4 items at once
    command      = ["./handle.sh", each.key, each.value.url]
  }

  agent "review" {                                  // an LLM agent task; same DAG as tasks
    depends    = ["step"]
    provider   = "anthropic"                        // default; "openai" for OpenAI models
    model      = "claude-haiku-4-5"
    prompt     = "..."
    system     = "..."                              // optional
    tools      = ["bash", "text_editor", "git", "web_search", "web_fetch"]  // web_fetch is Anthropic only
    skills     = ["skills/triage/SKILL.md"]         // Agent Skills, loaded on demand
    max_turns  = 10
    max_tokens = 4000
    budget_usd = 0.50                               // stop the run past this cost (estimated for unpriced models)
    wallclock  = "5m"                               // stop the run after this long
    env        = ["ANTHROPIC_API_KEY=$secret.ANTHROPIC_API_KEY"]

    mcp "fs" {                                      // local MCP server, started for this run
      command = ["npx", "-y", "@modelcontextprotocol/server-filesystem", "${path}"]
      env     = ["SOME_VAR"]                        // env var names to pass through
    }
    mcp "linear" {                                  // remote MCP server
      url     = "https://mcp.linear.app/mcp"        // transport = "sse" for older servers
      headers = { Authorization = "Bearer $secret.LINEAR_TOKEN" }
      tools   = ["list_issues", "create_comment"]   // only these of its tools (default: all)
    }
    mcp_servers = ["github"]                        // shared mcp blocks, by name (below)
  }
}
```

Every task, shell or agent, runs in the task's path: the directory
holding `cronicle.hcl`, or the task's own repo checkout (see Repos). A
shell command runs with that as its working directory. An agent's
`bash`, `text_editor`, and `git` tools work there (`git` is built in, so
the host needs no git CLI: `status`, `log`, `diff`, `show`, `branch`,
`commit`, `push`) (`text_editor` can
also reach `${scratch}`), and its system prompt tells it that absolute
path and which commit is checked out. Agent tasks need `ANTHROPIC_API_KEY` (or `OPENAI_API_KEY` with
`provider = "openai"`) in their environment or `env`. With no `tools`,
an agent gets `bash`, `text_editor`, and `git`; `web_search` and
`web_fetch` are opt-in because the provider bills each call.

### Placeholders

Filled in when a task runs, in `command`, `prompt`, and `system`:

| placeholder         | value                                              |
|---------------------|----------------------------------------------------|
| `${date}`           | `YYYY-MM-DD` of the trigger time (UTC)             |
| `${datetime}`       | RFC3339 trigger time                               |
| `${timestamp}`      | Unix seconds of the trigger time                   |
| `${last_run}`       | RFC3339 start of the schedule's last successful run (empty on the first) |
| `${last_run_epoch}` | the same, in Unix seconds                          |
| `${path}`           | the task's working directory                       |
| `${scratch}`        | directory shared by all tasks in this run          |
| `${env.NAME}`       | env var `NAME` when the config is loaded (empty if unset) |
| `${each.key}`, `${each.value}`, `${each.value.field}` | the item, in a `for_each` task (also `$EACH_KEY` / `$EACH_VALUE` in its env) |

`${...}` is reserved for these placeholders, in heredocs too: any other
name is a config error. Write shell variables as `$NAME`, or escape the
braced form as `$${NAME}`.

When a task fails (after its retries), every task that depends on it is
skipped; tasks that don't depend on it keep running.

### `for_each`: one task per item

`for_each` runs a task or agent once per item of a collection, with
Terraform's semantics. The collection usually comes from an upstream
task's stdout, parsed as JSON, so the work can be discovered at run
time. A common shape: list the open PRs, then review each one with its
own agent in its own checkout of the PR:

```hcl
schedule "pr_review" {
  cron = "0 * * * *"

  // Prints the open PRs: [{"number": 12, "title": "…", "headRefOid": "a1b2…", "baseRefName": "main"}, …]
  task "list_prs" {
    command = ["gh", "pr", "list", "--repo", "org/app", "--json", "number,title,headRefOid,baseRefName"]
    env     = ["GH_TOKEN=$secret.GITHUB_TOKEN"]
  }

  // One agent per PR (review[12], review[15], …), each in its own
  // checkout of that PR's head commit.
  agent "review" {
    for_each     = { for pr in task.list_prs.output : pr.number => pr }
    max_parallel = 3

    repo {
      url      = "https://github.com/org/app.git"
      password = "$secret.GITHUB_TOKEN"
      commit   = each.value.headRefOid
    }

    model      = "claude-sonnet-4-6"
    tools      = ["git", "text_editor"]
    max_turns  = 20
    budget_usd = 0.50
    prompt     = <<-EOT
      Review pull request #${each.key}, "${each.value.title}". The working
      tree is the PR's head commit. Diff it against origin/${each.value.baseRefName}
      with the git tool, read the changed files, and use git show for a
      file's version on the base branch. Write the review to
      ${scratch}/review-${each.key}.md: bugs first, then risks, then nits.
    EOT
  }

  // Same collection, so post[12] waits only on review[12].
  task "post" {
    for_each = { for pr in task.list_prs.output : pr.number => pr }
    depends  = ["review"]
    command  = ["gh", "pr", "comment", each.key, "--repo", "org/app", "--body-file", "${scratch}/review-${each.key}.md"]
    env      = ["GH_TOKEN=$secret.GITHUB_TOKEN"]
  }

  // A plain task waits for every item, failed or not.
  task "summary" {
    depends             = ["post"]
    continue_on_failure = true
    command             = ["sh", "-c", "ls ${scratch} | grep -c '^review-' | xargs echo reviews written:"]
  }
}
```

A literal works too: `for_each = toset(["us", "eu"])` gives `task[eu]` and
`task[us]`.

**The collection.** `task.<name>.output` is that task's stdout as JSON.
Only one task's output can feed a collection, and that task must be a
plain task, not another `for_each`. As in Terraform, the result must be
one of:
- **a map or object:** one item per key.
- **a set of strings:** one item per element, with `each.key` equal to
  `each.value`.

Both iterate in sorted order. A list is not a set, so a JSON array is
rejected until you convert it:
- `toset(...)` for a list of strings.
- a for expression for a list of objects, like
  `{ for pr in task.list_prs.output : pr.number => pr }`.

**Functions.** The `for_each` expression (and only it) can call
Terraform's pure built-in functions: `merge`, `keys`, `values`,
`lookup`, `range`, `zipmap`, `concat`, `flatten`, `distinct`, the
`set*` functions, `toset`/`tolist`/`tomap`/`tostring`/`tonumber`,
`format`, `join`, `split`, `replace` (regex when wrapped in `/…/`),
`upper`, `lower`, `jsondecode`, `try`, `can`, and the rest of the
string and number set. For example:
- `merge({ main = { sha = "HEAD" } }, { for pr in … : pr.number => pr })`
- `toset([for i in range(3) : tostring(i)])`

**Inside the task.**
- `each.key` and `each.value` (with fields: `each.value.sha`) fill into
  `command`, `env`, an agent's `prompt` and `system`, and a `repo`
  block's `url`, `branch`, and `commit`. A string fills in as itself;
  anything else fills in as JSON. Values are inserted last, as plain
  text, and never evaluated.
- Each item with a `repo` gets its own working tree,
  `.cronicle/repos/<repo>/<schedule>/<task>/<key>/`, so items running at
  once never share a checkout. An agent's system prompt names that
  folder and the commit on it.
- In `sh -c` scripts, read the item from `"$EACH_KEY"` / `"$EACH_VALUE"`
  rather than writing `${each.key}` into the script: text spliced into a
  script runs as shell code.

**Dependencies.**
- A `for_each` task waits on its source automatically.
- Two `for_each` tasks over the same source that depend on each other
  pair up by key.
- Any other task that depends on a `for_each` task waits for all of its
  items. Without `continue_on_failure`, it's skipped if any item failed.
- `depends` always names whole tasks (`"review"`), never items
  (`"review[12]"`). Task names can't contain `[` or `]`.

**Limits, failures, retries.**
- `max_parallel = N` caps how many items run at once.
- An expansion holds at most 1,000 items.
- An empty collection is a successful no-op.
- Bad output (not JSON, the wrong shape) fails the `for_each` task with
  the reason. An item missing a field the task reads fails just that
  item.
- The items are recorded with the run, so `POST /v1/runs/{id}/retry-failed`
  and retrying one item (`/v1/runs/{id}/tasks/review[15]/retry`) rerun
  the same items without asking the source again.
- `cronicle exec --task review` can't run a collection read from another
  task, since that task won't run. Run the whole schedule instead.

### MCP servers: more tools for an agent

An agent's built-in tools are `bash`, `text_editor`, `git`, `web_search`,
and `web_fetch`. An `mcp` block adds a [Model Context Protocol](https://modelcontextprotocol.io)
server's tools: an issue tracker, a database, a vendor API. Reach for one
when the agent needs to act on a service. For a fixed call you could
write yourself, a plain task with `curl` or a CLI is simpler, and you can
see exactly what it runs.

```hcl
// Defined once, used by name. Also allowed inside a schedule, where it
// wins over a top-level block of the same name.
mcp "github" {
  url     = "https://api.githubcopilot.com/mcp/"       // a remote server
  headers = { Authorization = "Bearer $secret.GITHUB_TOKEN" }
  tools   = ["get_pull_request", "add_issue_comment"]  // only these
}

schedule "triage" {
  cron = "0 9 * * *"

  agent "label" {
    model       = "claude-haiku-4-5"
    prompt      = "Label today's new issues in org/app."
    mcp_servers = ["github"]

    mcp "fs" {                                         // a local server, inline
      command = ["npx", "-y", "@modelcontextprotocol/server-filesystem", "${scratch}"]
    }
  }
}
```

- **Local vs. remote.** `command` launches a local server for the run.
  Use `env` to name the variables it may see; nothing else is passed.
  `url` connects to a remote one over Streamable HTTP
  (`transport = "sse"` for older servers), with `headers` for auth.
  Header values take `$secret.NAME`, and are never logged or shown.
- **Limit `tools`.** Without a `tools` list, the agent gets every tool
  the server offers, including ones that write or delete. For an
  unattended run, list just the tools the job needs. A name the server
  doesn't offer fails the run, and the error lists every name it does
  offer: an easy way to look them up.
- **Tool names.** The agent sees each tool as `<server>__<tool>`
  (`github__add_issue_comment`). The run output shows calls as
  `→ github.add_issue_comment: {…}`, and the block header has an `mcp`
  row per server.
- **Lifetime.** Servers start or connect when the agent run begins and
  stop when it ends. If one fails to start, the run fails before any
  model call, so the agent never sees half of its tools.
- **In `for_each`.** `each.*`, `${path}`, and `${scratch}` work in
  `command`, `url`, and header values, so each item can point a server at
  its own checkout.

### Repos

A `repo` block runs tasks against a git repository. Cronicle uses an
embedded git, so the host doesn't need `git` installed.

```hcl
repo {
  url    = "git@github.com:org/etl.git"   // or https://…, or a local path
  key    = "~/.ssh/deploy_key"            // SSH key, for private SSH remotes
  branch = "main"                         // or commit = "<sha>", not both
  // HTTPS auth: username = "x" (default), password = "$secret.GITHUB_TOKEN"
}
```

What it does depends on where it goes:

- **In a `task`:** the task runs inside its own checkout of the repo, at
  `.cronicle/repos/<repo>/<schedule>/<task>/`. That is its working
  directory and `${path}`, so a command can call the repo's files by
  relative path. The repo is cloned on the first run. On every run after
  that, cronicle fetches and force-checks-out `branch` or `commit` (the
  remote's default branch when neither is set), so each run uses the
  latest code and **local edits in that checkout are discarded**. A
  `for_each` item gets its own checkout,
  `.cronicle/repos/<repo>/<schedule>/<task>/<key>/`, and its `branch`
  or `commit` can name the item (`commit = each.value.headRefOid`).
- **In a `schedule`:** the default for every task in it. Each task still
  gets its own checkout. A task's own `repo` block overrides it, and
  fields the task leaves empty (`url`, `key`, `branch`, `commit`) are
  taken from the schedule's.
- **At the top level:** the directory holding `cronicle.hcl` is itself a
  checkout of the repo. `run` keeps it in sync with the remote, so
  schedule changes are deployed by pushing to the repo, and tasks without
  their own `repo` run there at the latest commit. Local edits there are
  overwritten too: change the schedules through the repo.

Tasks with no `repo` anywhere run in the directory holding
`cronicle.hcl`.

Credentials: `key` for SSH, or `password` for HTTPS, usually a token.
Use `$secret.NAME` (resolved when the task runs) or `${env.NAME}`
(resolved when the config loads). Don't write a token into the file.
See [Example: a token for private repos](#example-a-token-for-private-repos).

`repos = [...]` at the top level is different: it lists repos that keep
their own `cronicle.hcl`. Cronicle clones each one and adds its
schedules to this one, so a team can own its schedules in its own repo.

A repo is the right home for code that has outgrown an inline command:
long programs, code with tests, or code shared across tasks. Keep the
steps and their order in `cronicle.hcl`, though, with one task per
script call, so the DAG still shows what runs.

## Reading a run

```
◆ daily_sales  06:00:00 · 5 tasks · run 20260101T060000Z-1a2b3c4d
  fetch
  └─ validate
     ├─ load
     └─ summary
        └─ notify  ← load, summary
```

The DAG comes first: each task under the one it waits on, with `←`
listing every dependency of a task that waits on more than one. A
`for_each` task is drawn as `name[*]`. Each task then prints as a frame:
`╭─` with the command, `│` lines of output, and `╰─` with the exit code
and duration. A task with a `repo` shows its checkout under a `├─ git ─`
divider before its output: clone or existing checkout, fetch, and the
commit it ended up on. A failed shell task ends with a `↻ rerun` line holding the
exact command on one line, ready to paste into a shell. For a
`for_each` item, the line also sets `EACH_KEY` and `EACH_VALUE`. The
last line, `● name succeeded` or `● name failed`, is the result for the
whole run.

`for_each` adds a few lines:

```
⋔ review  3 items from list_prs: 12, 15, 20
╰─ review[15]  failed · 8120 in · 940 out · $0.0385 · 41.2s · max_turns
⊘ post[15] skipped  upstream review[15] failed
⋔ review  done · 3 items · 1 failed
```

Everything cronicle keeps is under `.cronicle/` (gitignored): `state.db`
(run history and secrets), `scratch/` (per-run `${scratch}` dirs),
`repos/` (repo checkouts), and with `--log-to-file`, `log/` and `runs/`.

More: https://github.com/jshiv/cronicle#readme
