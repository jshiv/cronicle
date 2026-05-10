# Daily Report

A canonical multi-agent workflow: 4 crawlers fan out to read team state from different sources, a composer fans in to merge them into a unified daily report.

## Shape

```
                       ┌──────────────────┐
                       │ crawl_slack      │ ─┐
                       └──────────────────┘  │
                       ┌──────────────────┐  │
                       │ crawl_discord    │ ─┤
                       └──────────────────┘  │   ┌─────────────┐
                       ┌──────────────────┐  ├──▶│  compose    │
                       │ crawl_email      │ ─┤   └─────────────┘
                       └──────────────────┘  │
                       ┌──────────────────┐  │
                       │ crawl_code       │ ─┘
                       └──────────────────┘
```

Each crawler writes a markdown file to a schedule-scoped scratch dir; the composer reads them and produces `REPORT.md` (and optionally posts to Slack via MCP).

## How tasks share data

The `${scratch}` template variable resolves to a per-schedule, per-run shared dir at `<croniclePath>/.cronicle/scratch/<schedule>/<run-id>/`. All tasks in one schedule run see the same dir. cronicle creates it before walking the DAG, so even fan-out tasks running in parallel can write into it without coordination beyond filename.

The slog `schedule_start` event records the scratch path for audit:

```
schedule started ... scratch=/.../scratch/daily_report/20260510T160000Z
```

## Run it

This demo's crawlers produce mock data (so it runs without real Slack/Discord/Gmail credentials) but `crawl_code` exercises the built-in git tool against the real cronicle repo:

```bash
cronicle exec --schedule daily_report --path deploy/daily-report/cronicle.hcl
```

After it finishes, check the scratch dir:

```bash
ls $(pwd)/.cronicle/scratch/daily_report/*/
# slack.md  discord.md  email.md  code.md  REPORT.md
```

`REPORT.md` is the merged output.

## Productionizing

Replace each crawler's mock prompt with a real MCP server invocation:

```hcl
task "crawl_slack" {
  agent {
    prompt = "Use the slack MCP to query channels #engineering, #incidents over the last 24h. Summarize as 3-5 bullets. Save to ${scratch}/slack.md."
    tools  = ["text_editor"]

    mcp "slack" {
      command = ["npx", "-y", "@modelcontextprotocol/server-slack"]
      env     = ["SLACK_BOT_TOKEN", "SLACK_TEAM_ID"]
    }
  }
}
```

For posting back to Slack, add the same `mcp "slack"` block on the composer task and instruct the prompt to call the post-message tool with the rendered `REPORT.md` content.

## What this demonstrates

- **Cron-driven multi-agent DAG** — `cron = "0 9 * * *"` + `depends = [...]`
- **Cross-task context** — `${scratch}` shared dir
- **Per-task tool universes** — each crawler gets only what it needs
- **Per-task cost ceilings** — `budget_usd` per agent caps damage from a runaway turn
- **Mixed primitives** — `repo` clone (code crawler) + MCP servers (data crawlers) + skill (composer) all in one schedule
