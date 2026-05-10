# Design: state plane via log projection

Status: draft / sketch
Branch: `design/state-plane`
Author: working notes

## TL;DR

Logs are already the event stream — `task_start`, `shell_run`, `agent_run`,
`schedule_start`, `schedule_complete`, with exit codes, durations, costs.
The state plane is a **projection of those same events**, maintained in
the producer process and queried via the listener API. Workers POST
their events directly to the producer (in-process for single-node, HTTP
for distributed). The JSONL log on disk continues to be written and
shipped, unchanged, for log aggregators — but the projection no longer
depends on tailing it back.

The producer also owns the queue. Workers long-poll for jobs, ack on
completion, receive cancel signals over a control channel. Persistence
is a single embedded SQLite file (pure-Go driver, no cgo). Redis/NSQ
support drops — nobody is running it in practice and keeping two queue
shapes alive isn't worth the test matrix.

## Goals

1. Power live UI / control-plane affordances: list in-flight runs, show
   per-task progress, stop a run, retry a failed run.
2. **Drop Redis/NSQ.** One binary, one optional disk file, one port.
   Distributed mode = `cronicle run` on the producer + N workers
   pointing at it.
3. Preserve the existing Unix-philosophy layering: logs are the event
   record; everything else is derived.
4. Single code path — projection is always built. Storage backend
   varies by command (SQLite on disk for `run`, in-memory for `exec`).

## Non-goals

- Multi-producer HA / leader election. Single producer, optional warm
  standby. Cross-producer cron-tick dedup is the harder problem
  (option C in prior discussion) and deferred.
- Replacing the JSONL log. It stays. Loki/Vector stay. This is purely
  about a live-view cache + the internal queue.
- Temporal-style durable workflow state, signals, or human-in-the-loop
  steps. Out of scope.
- Backwards compatibility with `--queue redis|nsq`. Removed.

## What we keep (the Unix-philosophy answer)

The original design intent — pipe queue + logs as state — was right for
the cron-of-shell-tasks use case it was built for. Re-running a failed
shell job by re-tailing a log is fine because shell jobs are usually
idempotent and free.

What broke that model isn't the listener. It's that **agent tasks have
cost and side effects**. "Stop this $0.40-and-climbing run" needs a
live state view. "Retry only the failed leaf in a 5-task DAG" needs to
know the leaf's status without grepping the file.

The framing that preserves the philosophy:

> Logs are the event stream. State is a projection of the event stream.
> The projection is rebuildable, not authoritative. If you delete the
> state DB, you lose live status for in-flight runs but nothing more —
> you re-derive history from logs.

That's the same shape as Kafka + materialized views, or git's reflog
vs. branch refs: an append-only log of facts, plus a small index that
makes recent state cheap to query. The log is the truth.

## What we add

### 1. `run_id` threaded through every event

Today events carry `schedule` and `task` but no run identifier, so two
fires of the same schedule look identical in the projection. A ULID
assigned at trigger time (cron tick or HTTP trigger), included in
every subsequent event for that run, fixes this. It's a small change
to `Schedule` (`RunID string`) and the slog handler in
`internal/cronicle/exec.go`.

### 2. The projection

An in-memory map maintained by the producer:

```go
type RunState struct {
    RunID     string
    Schedule  string
    Status    Status // queued | running | succeeded | failed | canceled
    StartedAt time.Time
    EndedAt   time.Time // zero until terminal
    Tasks     []TaskState
    Source    string    // "cron" | "http" | "exec"
    WorkerID  string    // who claimed it (empty in single-node)
    Cost      float64   // sum of agent costs across the run
}

type TaskState struct {
    Name      string
    Status    Status
    Attempt   int
    StartedAt time.Time
    EndedAt   time.Time
    ExitCode  int
    Error     string
    Cost      float64
}
```

Built by folding existing events:

| event              | effect                                          |
|--------------------|-------------------------------------------------|
| `schedule_start`   | open run, set Status=running, populate Tasks    |
| `task_start`       | mark task running                               |
| `shell_run` (success=true)  | mark task succeeded, record exit/duration |
| `shell_run` (success=false) | mark task failed                          |
| `agent_run`        | mark task succeeded/failed, accumulate Cost     |
| `schedule_complete`| close run; Status terminal                      |
| `trigger` (already exists) | optional pre-state for "queued"          |

Persisted continuously to SQLite — every event is `INSERT INTO
events` plus an `UPDATE runs / tasks` in the same transaction.
Bounded retention: a periodic janitor deletes events + runs older
than the configured window (default 90 days). For deep history
beyond the window, query the JSONL log on disk.

### 3. How events get to the projection

**Explicit publish, not log tailing.** Workers emit events through the
existing slog handler chain. We add a new handler that:

- single-node: in-process call into `projection.Apply(event)` —
  fastest path, no serialization.
- distributed: `POST /v1/events` to the producer with the event as a
  JSONL line. Same shape as `cronicle.jsonl`. Body can be batched
  (newline-delimited multi-event) so one HTTP roundtrip covers a
  burst.

The local JSONL file + stdout writers stay on. Log aggregators
(Loki, Vector, Splunk) keep working untouched. The projection never
queries the log file or a third-party aggregator — that path was
always going to be brittle (file rotation, log-shipping latency,
aggregator query SLAs leaking into our control plane).

```
worker:
  slog.Info(... entry_type=task_start ...)
       │
       ├──► stdout (operator)
       ├──► .cronicle/log/cronicle.jsonl (rotation, optional Loki ship)
       └──► HTTP handler ──► POST /v1/events ──► producer

producer:
  HTTP /v1/events ──► projection.Apply(event)
                  └─► same JSONL/stdout writers (audit trail kept on producer too)
```

Worker-side buffering: events queue locally with bounded retention
(say 5,000 events / 60s) and retry with backoff if the producer is
unreachable. Logs continue writing locally regardless, so a partition
never loses the audit trail.

### 4. Producer as queue (Redis/NSQ replacement)

Today's queue abstraction (`vice.Transport`) is removed. The new shape:

```
GET  /v1/jobs?worker=W1&block=30s  long-poll; returns one schedule JSON or 204
POST /v1/jobs/{run_id}/ack         worker confirms completion (success/failure)
POST /v1/jobs/{run_id}/heartbeat   visibility-timeout renewal mid-task
GET  /v1/workers/{id}/control      SSE; producer pushes "cancel run_id=X"
```

The queue is a table in the embedded SQLite file (see §5). On worker
pull, a row moves to `claimed` with `claim_expires = now + visibility`.
A reaper goroutine moves expired claims back to `pending` so a dead
worker's job is re-dispatched. Same semantics as SQS or Redis
BLPOP+keepalive, but in-process.

#### Scale check

A typical cronicle deployment fires far less than people assume:

- 100 schedules × 24 fires/day = 2,400 schedule fires/day
- ×5 tasks/schedule = 12,000 task fires/day
- ×3 events/task (start, complete, agent_run) ≈ 36,000 events/day
- Average ~25 events/min, peak maybe 100/min

SQLite in WAL mode does 1,000+ writes/sec on a laptop SSD. We're 60×
under the floor. Even 10× this load is fine on a single producer.
The number that actually matters is **concurrent in-flight runs**,
not aggregate event throughput, and that's bounded by worker count.

If a deployment ever outgrows a single producer, the answer is
sharding by schedule (run two `cronicle run` processes against
disjoint HCL configs) — not a more sophisticated queue. That's still
simpler than running Redis Sentinel.

### 5. Embedded store choice

The state plane needs durable storage for two things: the queue
(claimed/pending jobs, visibility timeouts) and the projection
(`/v1/runs` history, periodic snapshots). These are different shapes
— the queue is FIFO with claim semantics, the projection is filtered
list reads — but they want to live in the same on-disk file for
operational simplicity. One file to back up, one to wipe.

#### Candidates

| store                        | shape | language | pros | cons |
|------------------------------|-------|----------|------|------|
| **bbolt** (etcd-io fork)     | KV B+tree, mmap | pure-Go | tiny API, battle-tested in etcd, single-file | single writer (fine for us); no SQL — we'd hand-roll secondary indexes for `/v1/runs?status=failed&schedule=X` |
| **BadgerDB** (dgraph)        | KV LSM | pure-Go | concurrent writers, large-value friendly | LSM compaction can stall; no SQL; heavier than we need |
| **modernc.org/sqlite**       | SQL | pure-Go (transpiled from C) | SQL queries, ubiquitous tooling, WAL mode, indexes for free, debug shell | larger binary, slightly slower than mattn/go-sqlite3 (cgo) but irrelevant at our load |
| **mattn/go-sqlite3**         | SQL | cgo | fastest SQLite | requires cgo — breaks the goreleaser cross-compile that we already paid for in v0.4.0 |
| **Pebble** (cockroachdb)     | KV LSM | pure-Go | very fast | massive footprint, designed for cockroach scale |

#### Recommendation: `modernc.org/sqlite`

- **Right query shape.** The projection API needs filtered reads
  (`/v1/runs?status=failed&schedule=X&since=24h&limit=50`). With a
  KV store we re-implement that as composite keys + tombstone-aware
  iterators. With SQL it's `WHERE status = ? AND schedule = ? AND
  started_at > ?` plus indexes. Less code, less bugs.
- **Pure-Go matters here.** The v0.4.0 release notes specifically
  called out the cross-compile work to make Windows/Darwin/Linux
  builds work without a cgo toolchain. Bringing in cgo just for
  SQLite would undo that.
- **Performance is irrelevant at our load.** modernc's SQLite
  benchmarks at ~50% of mattn's cgo version on writes. Mattn does
  ~50k inserts/sec batched; modernc does ~25k. We need <100/min.
  Three orders of magnitude of headroom either way.
- **Operationally familiar.** `sqlite3 .cronicle/state.db '.tables'`
  works on every dev box. Compare that to debugging a corrupt bbolt
  page.

#### Why not bbolt

bbolt is a real option and the answer would not be wrong. It is
smaller (no SQL parser, no virtual tables) and faster on simple
KV workloads. If `/v1/runs` filtering ends up being only
"recent N runs, no other filters," bbolt is preferable. The reason
to spend the SQL tax up front is that the API surface in §"API
surface" includes filter combinations that grow over time
(`status`, `schedule`, `since`, `worker_id`, `source`), and writing
a half-baked SQL engine on top of KV is the worst outcome.

#### What about pure in-memory?

For ephemeral one-shots there's a clean answer: SQLite supports
`:memory:` as a DSN. Same code path, no disk, dies with the process.
Default for `cronicle exec`. See §6.

### 6. Always-on, with backend chosen by command

The projection runs unconditionally. There is one code path: every
emitted event passes through the projection's `Apply()`. What
varies is the storage backend:

| command                           | backend           | rationale |
|-----------------------------------|-------------------|-----------|
| `cronicle run`                    | SQLite on disk    | the daemon needs queue durability and live-state across restarts |
| `cronicle run --state=:memory:`   | in-memory SQLite  | escape hatch for read-only-FS deployments (containers, lambdas) |
| `cronicle exec`                   | in-memory SQLite  | foreground one-shot; user is watching; nothing to query later |
| `cronicle worker`                 | (none — workers don't host a projection) | the projection lives only on the producer |

Default disk path is `<croniclePath>/.cronicle/state.db` — same
directory we already write logs, runs, and scratch to. No new
filesystem permission surface.

The "single code path" benefit is real: `Projection`, the
`Apply()` fold rules, and the `/v1/runs` query layer all use the
same store interface. Disk vs. memory is one line at construction.
No conditional branches in hot paths.

The "needs disk write access" downside is paid only by `cronicle
run` and only at the existing `.cronicle/` directory. It's the
same constraint that already applies to `--log-to-file` and
`${scratch}`.

### 7. Stop and retry

**Stop**: `POST /v1/runs/{run_id}/cancel` →
1. Producer marks projection state `canceled`.
2. Producer pushes `{type: "cancel", run_id: X}` to the worker
   holding it via the SSE control channel.
3. Worker calls `ctx.Cancel()`. Shell tasks die because they already
   run under `exec.CommandContext`. Agent tasks need a small change:
   check `ctx.Done()` between turns (cheap — every turn is already a
   syscall to Anthropic).
4. Tasks not yet started in the same DAG are skipped.

Honest limits: a tool call that's already in flight finishes before
the agent loop notices cancellation. Best-effort, not preemption.

**Retry**: `POST /v1/runs/{run_id}/retry` →
1. Producer re-enqueues the schedule JSON it still has from the
   original trigger. New `run_id`, otherwise identical payload.

v1 retries the entire run. Per-task retry ("rerun only `compose`
since `crawl_*` succeeded") is doable but requires the producer to
synthesize a sub-schedule the way `triggerTask` already does in the
listener — so it's not new mechanism, just new API surface.

### 8. Recovery

| failure mode | what's lost | how it heals |
|--------------|-------------|--------------|
| producer crash, restart | in-flight projection rows in WAL not yet checkpointed | SQLite WAL replay on open recovers committed rows; queued jobs survive in the same DB file |
| worker crash, mid-task | task event stream stops mid-run | visibility timeout on claimed job → another worker picks it up; projection marks the orphaned attempt failed via heartbeat timeout |
| network partition (worker ↔ producer) | event POSTs fail | worker queues events locally, retries with backoff; producer's projection sees the gap and reconciles when worker rejoins |
| state DB corrupted | live status + queue contents | delete the file and restart; logs on disk remain authoritative for history. If queue had pending jobs, missed cron ticks fire on the next heartbeat. |

## Architecture

```
              ┌─────────────────────────────────────────────┐
              │              cronicle run (producer)         │
              │                                              │
   HCL ─────► │  cron tick ─┐                                │
              │             ▼                                │
              │           [queue]──────► GET /v1/jobs ──┐    │
              │             ▲                            │    │
   HTTP ────► │  listener ──┘                            │    │
   trigger    │                                          │    │
              │  projection ◄────── POST /v1/events ◄────┼────┐
              │     ▲                                    │    │
              │     │   /v1/runs    /v1/runs/{id}/cancel │    │
              │     └────────── API ───────────────────  │    │
              │                                          │    │
              │  SQLite .cronicle/state.db               │    │
              │     - queue (pending / claimed / done)   │    │
              │     - projection (runs, tasks, events)   │    │
              │  cronicle.jsonl (append-only event log)  │    │
              └──────────────────────────────────────────┼────┘
                                                         │
                                                         │
                                            ┌────────────┴───────────┐
                                            │      cronicle worker    │
                                            │   exec / agent / git    │
                                            │   slog → POST /v1/events│
                                            │   ctx.Done() ← SSE      │
                                            └─────────────────────────┘
```

## API surface (proposed)

Existing (PR #83):

```
GET  /healthz
GET  /v1/schedules
POST /v1/schedules/{name}/trigger
POST /v1/schedules/{name}/tasks/{task}/trigger
```

New for state plane:

```
GET  /v1/runs                          list recent runs (filterable by status, schedule)
GET  /v1/runs/{run_id}                 single run, including per-task state
POST /v1/runs/{run_id}/cancel          stop in-flight run
POST /v1/runs/{run_id}/retry           re-enqueue original schedule JSON
GET  /v1/runs/{run_id}/events          stream of events for a run (SSE; for live UI)
POST /v1/events                        worker → producer event ingest
GET  /v1/workers                       list active workers
```

Distributed-mode worker channel:

```
GET  /v1/jobs?worker={id}&block={dur}  long-poll job claim
POST /v1/jobs/{run_id}/ack             worker reports completion
POST /v1/jobs/{run_id}/heartbeat       worker keepalive (visibility renewal)
GET  /v1/workers/{id}/control          SSE; producer → worker (cancel signals)
```

All authed with the same bearer token as the trigger API. Worker auth
uses the same token; multi-tenant separation is out of scope for v1.

## What this is NOT

- **Not a replacement for logs.** Delete the projection DB and you lose
  live status for currently-running runs. History is intact in JSONL.
- **Not Temporal / Argo.** No durable workflow state, no signals, no
  manual approve steps.
- **Not multi-producer HA.** A single producer is still the cron-tick
  authority; if you need that level of resilience, run an external
  broker and a hot standby that reads the same JSONL — but be aware
  cronicle does not currently de-dupe cron ticks across producers,
  and that's the work that would be option C.

## Open questions

1. **Projection retention default** — 90 days seems right. Configurable
   via `retention { events = "90d" }` in cronicle.hcl. Janitor runs
   on the heartbeat tick.
2. **Event ingest format** — JSONL bodies (one event per line, matches
   the file format). Composes with batching and with debug aids like
   `tail -f cronicle.jsonl | curl --data-binary @- /v1/events`.
3. **Worker identity** — Operator-assigned via `--worker-id` flag,
   falling back to `<hostname>-<pid>`. Stable IDs make logs and the
   `GET /v1/workers` view useful for debugging.
4. **Schema migrations** — SQLite gives us this for free with a
   `schema_version` PRAGMA + numbered migration files. Worth doing
   from day one even though we have one table to start.
5. **Per-task retry** — punt to v2 unless we hear demand. The mechanism
   exists (sub-schedule construction via `triggerTask`), but the UX
   question — "retry compose, but should it re-run with the original
   `${date}` or today's?" — needs thought.
6. **Removing `--queue redis|nsq`** — break compatibility cleanly in
   the next minor (v0.5). No deprecation period given non-existent
   user base for those modes; release notes call it out.

## Phasing

**Phase 1** (state plane in place, no API for control yet):
- Add `run_id` to `Schedule` and thread through events.
- Add the in-process projection (single-node) — slog handler tee.
- Add SQLite store with queue + projection schema.
- Add `GET /v1/runs`, `/v1/runs/{id}`, `GET /v1/workers`.
- `cronicle exec` uses `:memory:` SQLite; `cronicle run` uses
  `.cronicle/state.db`.

**Phase 2** (distributed via producer-served queue):
- `POST /v1/events` ingest endpoint.
- `GET /v1/jobs` long-poll with visibility timeouts; ack + heartbeat.
- `cronicle worker` rewrites: HTTP client to producer instead of
  vice transport.
- **Remove** `--queue redis` / `--queue nsq` and the vice dependency.

**Phase 3** (active runtime control):
- `POST /v1/runs/{id}/cancel`, `/retry`.
- Worker SSE control channel.
- Agent loop checks `ctx.Done()` between turns.

Each phase ships independently. After Phase 1 the UI / cronicle-web has
something real to query. After Phase 2 the deployment story collapses
to "one binary, one optional disk file." After Phase 3 there's a stop
button.

## Why this preserves the original philosophy

The pipe-queue + logs-as-state design is right when the things you
schedule are cheap and idempotent. We didn't break it by adding a
listener; we exposed that it doesn't compose with the operations we
now care about (live status, cost-aware stop, selective retry).

The fix isn't to abandon "logs as state" — it's to recognize that the
log is an *event stream*, and event streams are queryable through
projections. The projection is small, derived, and disposable. The
log is large, authoritative, and pure. That's not a departure from
the philosophy. It's the philosophy applied at the right layer.
