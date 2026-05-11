# SSE live-stream demo

Three schedules, three filter scopes — a fully-worked example of
cronicle's live SSE event stream.

```bash
export ANTHROPIC_API_KEY=...     # only needed for the `story` agent task

cronicle run \
  --path deploy/sse-demo/cronicle.hcl \
  --listen :7766 \
  --listen-token smoke \
  --log-to-file
```

`heartbeat` fires every 10s on its own; `long-build` and `story` are
manual-trigger only (`cron = ""`).

```bash
TOKEN=smoke
BASE=http://localhost:7766
AUTH="Authorization: Bearer $TOKEN"
```

---

## 1. Per-run drill-in — `/v1/runs/{run_id}/events`

Use when the operator clicks a specific running run from a list.
Requires the run_id to already exist.

```bash
# Fire long-build, race to capture its run_id
curl -s -X POST -H "$AUTH" "$BASE/v1/schedules/long-build/trigger" > /dev/null
RUN_ID=$(curl -s -H "$AUTH" "$BASE/v1/runs?status=running&limit=1" | jq -r '.[0].run_id')
echo "watching $RUN_ID"

# Subscribe to just this one run
curl -N -H "$AUTH" "$BASE/v1/runs/$RUN_ID/events"
```

Expect: `shell_run_start` header rule, six `stdout_chunk` lines
(`[step] fetching sources` … `[step] done`), footer
`[exit=0 · 6062ms · transcript=…]`, then `✓ schedule complete`.

---

## 2. Per-schedule — `/v1/schedules/{name}/events`

Subscribe BEFORE the run exists. Every run of the named schedule flows
through the same connection.

```bash
# Open SSE first (background), then trigger twice
curl -sN -H "$AUTH" "$BASE/v1/schedules/long-build/events" &
sleep 0.2
curl -s -X POST -H "$AUTH" "$BASE/v1/schedules/long-build/trigger" > /dev/null
sleep 7   # let run 1 finish
curl -s -X POST -H "$AUTH" "$BASE/v1/schedules/long-build/trigger" > /dev/null
```

Expect: full sequence for run 1 (`schedule_start` → header → 6 chunks →
footer → `schedule_complete`), then the same sequence for run 2 — both
on one stream, no polling needed.

This solves the "open SSE before the run starts" chicken-and-egg
because the filter key is the stable schedule name.

---

## 3. Firehose — `/v1/events/stream`

Every run on every schedule. Frames from concurrent runs interleave by
event-time, which is exactly what an operator dashboard wants.

```bash
curl -sN -H "$AUTH" "$BASE/v1/events/stream" &
sleep 0.2
curl -s -X POST -H "$AUTH" "$BASE/v1/schedules/heartbeat/trigger" > /dev/null
curl -s -X POST -H "$AUTH" "$BASE/v1/schedules/story/trigger" > /dev/null
```

Expect: heartbeat's shell output (`tick from heartbeat`, `second line`)
threaded through `story`'s agent text-delta tokens, plus any
cron-auto-fired heartbeat ticks landing in the middle. All three
schedules' bytes share one wire.

---

## Choosing the encoding (`--live-format`)

| flag                   | wire bytes inside `data:`                     | best for |
|------------------------|----------------------------------------------|---|
| `--live-format=pretty` | ANSI multi-line (default)                    | xterm.js / TTY |
| `--live-format=json`   | one compact JSON object per record           | structured frontends |
| `--live-format=text`   | `time=... level=INFO msg=... key=val`        | plain log viewers |

Set at startup; applies to every SSE endpoint regardless of scope.

---

## Notes

- **Live means live.** SSE frames are not retained server-side. Drop
  the connection and reconnect — you start from "now." For history of
  past runs, query `cronicle.jsonl` (or Loki, if you ship it via the
  pipeline in `deploy/local/`).
- **Auth** is bearer-token, same as the trigger endpoints. The token
  is set with `--listen-token` (or `CRONICLE_LISTEN_TOKEN`).
- **Heartbeat ping** every 15s (`: ping <unix>`) keeps proxies from
  killing idle connections. Browser EventSource clients ignore comment
  lines automatically.
