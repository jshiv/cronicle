# Cronicle on Redis

Distributed mode with Redis as the message broker. The producer (`cronicle run --worker=false`) pushes schedules onto a Redis list; one or more workers (`cronicle worker`) pop them and execute. Schedules — including agent tasks with skills and MCP servers — survive the wire format intact: cronicle JSON-marshals the full `Schedule` struct.

## Start the broker

```bash
docker compose -f deploy/redis/docker-compose.yaml up -d
docker compose -f deploy/redis/docker-compose.yaml ps
```

Stops the broker:

```bash
docker compose -f deploy/redis/docker-compose.yaml down
```

## Start a worker

In one shell, with `ANTHROPIC_API_KEY` exported (the worker is where agent tasks actually run, so the key has to be on the worker, not the producer):

```bash
cronicle worker \
  --path ./deploy/redis \
  --queue redis \
  --addr 127.0.0.1:6379 \
  --log-format pretty \
  --log-to-file
```

The `--log-to-file` flag mirrors structured JSON to `./deploy/redis/.cronicle/log/cronicle.jsonl` and writes per-run agent transcripts to `./deploy/redis/.cronicle/runs/`. This is the auditable view of what the worker actually did.

## Start the producer

In another shell:

```bash
cronicle run \
  --path ./deploy/redis/cronicle.hcl \
  --worker=false \
  --queue redis \
  --addr 127.0.0.1:6379 \
  --log-format pretty
```

`--worker=false` keeps the producer from also consuming the queue. With it `false`, schedules go *only* to the worker.

## What you should see

- Producer log: `Queuing... schedule=ping` (and similarly for `brief`) on each cron tick.
- Worker log: full execution blocks for both schedules. Agent runs include the `mcp_servers=[fs]` slog field; the MCP filesystem server is spawned and torn down per agent run on the worker.
- Worker file log: each agent run writes a JSONL transcript with request/response/tool-result/accounting.

## Scale workers

Multiple workers consume the same Redis list, taking turns:

```bash
# shell A
cronicle worker --path ./worker-a --queue redis --addr 127.0.0.1:6379

# shell B
cronicle worker --path ./worker-b --queue redis --addr 127.0.0.1:6379
```

Each worker needs its own `--path` so cloned repos and `.cronicle/` directories don't collide.

## Notes & gotchas

- **Skill files need to exist on the worker.** `task.Agent.Skills` is a list of paths relative to `task.Path`. The worker resolves them at execution time. If the producer has the skill files but the worker doesn't, `LoadSkillsForTask` errors and aborts the run.
- **MCP server binaries need to be on the worker's `PATH`.** The producer just sends the `command` strings; the worker spawns them. `npx` etc. must be installed where the worker runs.
- **Repo blocks clone on the worker.** If your task references a `repo`, the worker performs the clone; ensure the worker has SSH keys / network to reach the remote.
- **No queue persistence.** This compose config keeps Redis in-memory only. A worker outage losing the in-flight schedule is acceptable for cron — the next tick produces another. Add a Redis volume mount if you want durability.
