# Distributed mode demo

A working example of cronicle's broker-less distributed mode (introduced
in v0.5.0). Demonstrates:

- Multi-worker job fan-out — three ETL-style schedules firing on the
  same cadence, claimed in parallel by the worker pool
- The state-plane API: `/v1/runs`, `/v1/workers`, `/v1/schedules`
- Cancel / retry / resume on a long-running pipeline
- SSE control channel for low-latency cancel signals

No Redis. No NSQ. No docker-compose for the queue. The producer
serves its own queue out of an embedded SQLite file at
`.cronicle/state.db`; workers consume via HTTP long-poll.

**Scope of this demo**: pure shell tasks, focused on the runtime
control surface (queue, workers, cancel/retry/resume). For the agent
side of cronicle:

- [`deploy/mcp-demo/`](../mcp-demo/README.md) — smallest live demo
  combining `mcp` + `skills` + `${scratch}` in one task.
- [`deploy/daily-report/`](../daily-report/README.md) — multi-agent
  fan-out + composer using `${scratch}` and `skills`; MCP server
  blocks shown as commented templates (slack/gmail need credentials).

## Layout

- `cronicle.hcl` — three `etl_*` schedules on `@every 30s` cadence and
  one `report_pipeline` schedule (cron=`""` — manual trigger only)
  with three sequential tasks for the cancel/resume demo

## Prerequisites

- `cronicle` v0.5.0+ on your `PATH` (or invoke it by absolute path)
- Three terminals (or `tmux`/`screen`)
- A local copy of this directory: `git clone … && cd cronicle/deploy/distributed`

## Walkthrough

### 1. Start the producer

The producer hosts the cron tick + the listener + the SQLite queue.
`--worker=false` keeps it from running an in-process consumer, so all
work goes to remote workers (cleaner for the demo).

```bash
# terminal 1
export CRONICLE_LISTEN_TOKEN=demo-token
cronicle run \
  --path ./cronicle.hcl \
  --listen :8765 \
  --listen-token "$CRONICLE_LISTEN_TOKEN" \
  --worker=false
```

You'll see the cron loop start and the trigger listener bind:

```
INFO config loaded path=./cronicle.hcl schedules=4 tasks=5
INFO Starting Scheduler... cronicle=start
INFO Starting cron... schedule=etl_users  cron="@every 30s"
INFO Starting cron... schedule=etl_orders cron="@every 30s"
INFO Starting cron... schedule=etl_events cron="@every 30s"
INFO Trigger listener up addr=:8765
```

### 2. Start two workers

Each worker registers itself on the producer, opens an SSE control
channel (for cancel signals), and long-polls `/v1/jobs` for work to
claim. Run them in separate terminals so you can watch them fight for
jobs.

```bash
# terminal 2
export CRONICLE_LISTEN_TOKEN=demo-token
cronicle worker \
  --path ./ \
  --producer http://localhost:8765 \
  --producer-token "$CRONICLE_LISTEN_TOKEN" \
  --worker-id worker-A
```

```bash
# terminal 3
cronicle worker \
  --path ./ \
  --producer http://localhost:8765 \
  --producer-token "$CRONICLE_LISTEN_TOKEN" \
  --worker-id worker-B
```

Each worker logs its subscription to the control channel:

```
INFO HTTP worker started worker_id=worker-A producer=http://localhost:8765
INFO control channel: subscribed worker_id=worker-A
```

### 3. Watch the registry

```bash
TOKEN=demo-token
curl -s -H "Authorization: Bearer $TOKEN" \
  http://localhost:8765/v1/workers | jq .
```

```json
[
  { "worker_id": "worker-A", "host": "...", "status": "idle", "last_seen": "...", "runs_total": 0 },
  { "worker_id": "worker-B", "host": "...", "status": "idle", "last_seen": "...", "runs_total": 0 }
]
```

### 4. Watch the ETLs fan out

The three `etl_*` schedules fire every 30s. With two workers up, two
of them claim in parallel; the third waits in the queue and gets
picked up by whichever worker frees up first.

```bash
# tail recent runs every few seconds
watch -n 2 'curl -s -H "Authorization: Bearer '"$TOKEN"'" \
  "http://localhost:8765/v1/runs?limit=10" \
  | jq -r ".[] | \"\(.run_id)  \(.schedule)  \(.status)\""'
```

You'll see new runs appear with `status: succeeded` shortly after each
30-second tick. Run the workers query again — `runs_total` should be
roughly evenly split between A and B, with `status: idle|active`
oscillating as they claim and release.

### 5. Trigger the long pipeline + cancel mid-run

`report_pipeline` is `extract → transform → load`. `transform` sleeps
30 seconds, which is plenty of room to cancel.

```bash
# fire it
RID=$(curl -s -X POST -H "Authorization: Bearer $TOKEN" \
  http://localhost:8765/v1/schedules/report_pipeline/trigger \
  | jq -r .queued)

# wait a bit so 'extract' completes, then peek at the per-task state
sleep 3
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8765/v1/runs?schedule=report_pipeline&limit=1" \
  | jq '.[0] | {run_id, status, tasks: .tasks | map({name, status})}'
```

You'll see something like:

```json
{
  "run_id": "20260510T...-...",
  "status": "running",
  "tasks": [
    { "name": "extract",   "status": "succeeded" },
    { "name": "transform", "status": "running" },
    { "name": "load",      "status": "queued" }
  ]
}
```

Now cancel:

```bash
RUN_ID=$(curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8765/v1/runs?schedule=report_pipeline&limit=1" \
  | jq -r '.[0].run_id')

curl -s -X POST -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8765/v1/runs/$RUN_ID/cancel" | jq .
```

```json
{
  "run_id": "20260510T...",
  "worker_id": "worker-A",
  "was_claimed": true,
  "status": "canceled"
}
```

The worker holding `transform` receives the cancel SSE message within
milliseconds, kills the sleep via SIGTERM, and logs:

```
INFO control: canceling run run_id=20260510T...
ERROR shell task failed task=transform error="signal: terminated"
```

The projection's per-task state stays sticky — `transform` reports
`canceled`, not `failed`, even though the SIGTERM'd shell emitted
a failure event after.

### 6. Resume

The operator's "I fixed the underlying issue, pick up where we
stopped" path. Re-enqueues a new run with the already-succeeded tasks
filtered out:

```bash
curl -s -X POST -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8765/v1/runs/$RUN_ID/resume" | jq .
```

```json
{
  "original_run_id": "20260510T...A",
  "new_run_id":      "20260510T...B",
  "schedule":        "report_pipeline",
  "skipped_tasks":   ["extract"]
}
```

The new run has only `transform` and `load`. `transform`'s
`Depends` was rewired (the only entry, `extract`, was filtered) so
it becomes a fresh DAG entry point. `load`'s `Depends=["transform"]`
is preserved.

(`/resume` replays the *stored payload*, not the current HCL. If your
"fix" was a config change, hit `POST /v1/schedules/{name}/trigger`
instead — that fires a fresh run from current HCL.)

### 7. Compare with /retry

`/retry` re-enqueues the **whole** DAG from scratch — including
`extract`, which would re-run. Useful when the original output is
suspect; wasteful when only the failing leaf needs rework.

```bash
curl -s -X POST -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8765/v1/runs/$RUN_ID/retry" | jq .
```

### 8. Cancel scope (or: "does cancel stop everything?")

No — cancel is scoped to one `run_id`. Each fire of each schedule gets
its own run_id, its own queue row, its own per-run context on the
worker, its own SSE message routed by run_id. Other schedules in the
same HCL file keep running.

To see this concretely: while the ETLs are firing every 30s, fire
`report_pipeline` and cancel it mid-`transform`. The next ETL tick
arrives normally; workers claim and execute it untouched.

```bash
# fire report_pipeline
curl -s -X POST -H "Authorization: Bearer $TOKEN" \
  http://localhost:8765/v1/schedules/report_pipeline/trigger > /dev/null
sleep 2
RID=$(curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8765/v1/runs?schedule=report_pipeline&limit=1" \
  | jq -r '.[0].run_id')

# cancel just that run_id
curl -s -X POST -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8765/v1/runs/$RID/cancel" > /dev/null

# wait one tick, check that ETLs are still firing
sleep 35
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8765/v1/runs?status=succeeded&limit=5" \
  | jq -r '.[] | "\(.schedule)  \(.status)  \(.run_id)"'
```

You'll see the canceled `report_pipeline` next to fresh `etl_*`
successes — the ETLs were never touched.

The same scoping applies in reverse: a worker can hold multiple
in-flight runs simultaneously (one per claimed job, each with its own
`context.CancelFunc` keyed by `run_id`), and canceling one cancels
exactly one.

### 9. Inspecting state directly

The producer's state lives in a single SQLite file. `sqlite3` works
on it without locking the producer (WAL mode, concurrent readers
are fine):

```bash
DB=.cronicle/state.db

# tables
sqlite3 "$DB" '.tables'
#   events  jobs  runs  schema_versions  tasks  workers

# recent runs by status
sqlite3 -header -column "$DB" "
  SELECT schedule, status, COUNT(*) AS n
  FROM runs
  WHERE started_at > datetime('now', '-1 hour')
  GROUP BY schedule, status
  ORDER BY schedule, status;
"

# what's pending in the queue right now
sqlite3 -header "$DB" "
  SELECT run_id, schedule, status, claimed_by, attempt
  FROM jobs WHERE status IN ('pending', 'claimed')
  ORDER BY enqueued_at;
"

# worker registry
sqlite3 -header "$DB" "
  SELECT worker_id, current_run, runs_total, runs_failed, last_seen
  FROM workers ORDER BY last_seen DESC;
"
```

Useful when you want a quick local query without going through the
HTTP API, or when the listener is intentionally off and you still
want a postmortem.

## Operational notes

- **Worker liveness**: a worker that disappears (process killed,
  network gone) shows up in `/v1/workers` as `status: stale` after
  ~2 minutes. Its in-flight jobs are reaped by the visibility-timeout
  janitor every 10s and become claimable again.
- **Three persistent connections per worker**: long-poll `/v1/jobs`
  for dispatch, SSE `/v1/workers/{id}/control` for cancel signals,
  short-lived `POST /v1/events` for shipping log events back. No
  hidden queues, no broker watchdog.
- **State on disk**: `<--path-dir>/.cronicle/state.db` (SQLite, WAL
  mode). One file holds the queue, the projection, and the worker
  registry. `sqlite3 .cronicle/state.db '.tables'` to inspect.

## Migrating from v0.4.x Redis/NSQ

If you were running `cronicle run --queue redis --addr ...`, the
migration is:

```bash
# old
cronicle run --queue redis --addr 127.0.0.1:6379
cronicle worker --queue redis --addr 127.0.0.1:6379

# new
cronicle run --listen :8765 --listen-token "$TOKEN"
cronicle worker --producer http://localhost:8765 --producer-token "$TOKEN"
```

No external broker; the producer is the queue.
