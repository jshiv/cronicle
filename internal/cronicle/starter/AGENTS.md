# Cronicle guide

This directory is a [cronicle](https://github.com/jshiv/cronicle) project.
`cronicle.hcl` defines schedules; each schedule is a DAG of tasks that
runs on a cron trigger. This file is for anyone, human or AI, adding or
changing those schedules. `cronicle guide` prints it again.

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
4. **Hand data between tasks through `${scratch}`.** Each run of a
   schedule gets its own shared directory. One task writes a file there,
   and the next one reads it.
5. **Split schedules by trigger.** Work that runs on a different cron, or
   is unrelated, belongs in its own `schedule` block.
6. **Reach for an external script only when you need one.** Long programs,
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

```sh
cronicle init                                   # create cronicle.hcl, AGENTS.md, .cronicle/
cronicle exec                                   # run every schedule in ./cronicle.hcl once, now
cronicle exec --schedule daily_sales            # run one schedule's whole DAG
cronicle exec --task fetch                      # run one task alone (its depends don't run)
cronicle exec --time 2026-01-01T00:00:00Z \
              --end  2026-01-07T00:00:00Z       # backfill: one run per day, ${date} set per day
cronicle run                                    # start the scheduler (runs until stopped)
cronicle run --listen :8765 --listen-token T    # plus the HTTP API for triggers and live runs
cronicle secret set DATABASE_URL                # store a secret (value read from stdin)
cronicle guide                                  # print this guide
```

`exec` and `run` take `--path` (default `./cronicle.hcl`) and
`--log-format` (`auto`, `pretty`, `text`, or `json`). `exec` exits
non-zero when a task fails, so it works in scripts and CI. Add
`--log-to-file` to also keep JSON logs and per-run transcripts under
`.cronicle/`.

## `cronicle.hcl` reference

```hcl
heartbeat = "@every 5m"          // optional: periodic liveness log line
timezone  = "America/New_York"   // optional: default timezone for crons

schedule "name" {
  cron       = "0 6 * * *"       // cron, "@every 10m", "@hourly", "@once", or "" (exec only)
  timezone   = "UTC"             // optional
  start_date = "2026-01-01"      // optional window outside which runs are skipped
  end_date   = "2026-12-31"

  task "step" {
    command             = ["sh", "-c", "echo hi"]   // argv; no shell unless you call one
    depends             = ["other_step"]            // run after these succeed
    continue_on_failure = false                     // true: run even if a dependency failed
    env                 = ["KEY=value", "TOKEN=$secret.TOKEN"]
    retry {
      count   = 2
      seconds = 30
    }
    repo {                                          // optional: run inside a clone of this repo
      url    = "https://github.com/org/repo.git"
      branch = "main"
    }
  }

  agent "step" {                                    // an LLM agent task; same DAG as tasks
    depends    = ["step"]
    model      = "claude-haiku-4-5"                 // provider = "openai" for OpenAI models
    prompt     = "..."
    system     = "..."                              // optional
    tools      = ["bash", "text_editor", "git", "web_search", "web_fetch"]
    max_turns  = 10
    budget_usd = 0.50                               // stop the run past this cost
    wallclock  = "5m"
  }
}
```

Placeholders filled in at run time, in `command`, `prompt`, and `system`:

| placeholder    | value                                              |
|----------------|----------------------------------------------------|
| `${date}`      | `YYYY-MM-DD` of the trigger time (UTC)             |
| `${datetime}`  | RFC3339 trigger time                               |
| `${timestamp}` | Unix seconds of the trigger time                   |
| `${path}`      | the task's working directory                       |
| `${scratch}`   | directory shared by all tasks in this run          |

`${...}` is reserved for these placeholders, in heredocs too: any other
name is a config error. Write shell variables as `$NAME`, or escape the
braced form as `$${NAME}`.

When a task fails, every task that depends on it is skipped; tasks that
don't depend on it keep running. Agent tasks need `ANTHROPIC_API_KEY`
(or `OPENAI_API_KEY` with `provider = "openai"`) in the environment or
the secret store.

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
listing every dependency of a task that waits on more than one. Each
task then prints as a frame: `╭─` with the command, `│` lines of output,
and `╰─` with the exit code and duration. A failed shell task ends with
a `↻ rerun` line holding the exact command on one line, ready to paste
into a shell. The last line, `● name succeeded` or `● name failed`, is
the result for the whole run.

More: https://github.com/jshiv/cronicle#readme
