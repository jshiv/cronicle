# Design: live event stream via slog handler

Status: draft, ready to implement
Branch: `feat/live-event-stream`
Predecessor commit: `9d8c0ab Phase 1: SSE per-run event stream`

## TL;DR

Cronicle already fans every slog record out to multiple handlers
(stdout pretty, JSONL file, `state.Sink` projection). The natural
shape for a live event stream is **one more handler in that chain** —
a `state.LiveSink` that holds a pub/sub registry and forwards each
record to subscribed channels. The existing SSE endpoint
(`GET /v1/runs/{id}/events`, shipped in the predecessor commit on
this branch) keeps its wire contract, but its live source flips from
`state.Store.publish` (post-Apply, projection-filtered) to
`liveSink.Subscribe` (pre-projection, full firehose).

Result: the event stream sees everything stdout sees, including
records the projection drops because they have no `entry_type` —
errors from tools, warnings from the agent runtime, lifecycle prints
that carry a `run_id`. Same wire format. ~80 LOC change in cronicle.
Zero change in cronicle-infra or cronicle-web.

## Goals

1. Live debugging granularity matches what `kubectl logs` would show
   for the same worker — including non-`entry_type` slog records that
   carry a `run_id`.
2. Decouple liveness from the SQLite projection. A transient projection
   write failure stops the SQLite update; it does not stop the live
   stream.
3. Future-proof for per-turn agent events. When `pkg/agent.Run` adds
   `slog.Info("turn complete", "run_id", …, "turn", N, …)`, the SSE
   stream picks them up automatically — no migration to add a new
   `entry_type` to the projection schema.
4. Symmetric with the existing handler chain — anyone who understands
   `prettyHandler` / `Sink` understands `LiveSink`.

## Non-goals

- Replacing the `events` table or the `state.Sink` projection. They
  remain the durable record + the source of truth for replay on
  reconnect. `LiveSink` is in-memory only.
- Per-token streaming from the agent. That requires changes to
  `pkg/agent` (see "Out of scope" below).
- Cross-run firehose subscribers. The current API is per-run
  (`GET /v1/runs/{id}/events`); a server-wide `?run_id=*` mode is a
  future option but not part of this design.

## Current state (predecessor commit `9d8c0ab`)

The branch already ships a working per-run SSE stream:

- `state.Store.SubscribeEvents(runID) (<-chan Event, func())`
  (`internal/cronicle/state/store.go`) — pub/sub registry on the
  Store; `publish()` called from `Apply` post-commit.
- `state.Store.EventsSince(runID, sinceID) → []EventRow`
  (`internal/cronicle/state/query.go`) — SELECT-by-id replay query
  for SSE `Last-Event-ID` resume.
- `runEvents(w, r, store, runID)` handler in
  `internal/cronicle/listen_control.go` mounted under
  `handleRunRoute` as `GET /v1/runs/{id}/events`. Replay → live;
  heartbeat every 15s.
- Wire format: `id: N\nevent: cronicle\ndata: <json>\n\n`. Replay
  payload is the raw `events.payload` column (the slog JSON line);
  live payload is a slim re-marshal of the typed `state.Event`.

The downstream pieces consume this stream:

- `cronicle-infra` reverse proxy is SSE-aware (detects
  `text/event-stream`, switches from `io.Copy` to a flusher loop).
- `cronicle-web` has a `LiveEventLog` component that opens an
  EventSource at `/api/sse/runs/{runId}` and renders events as they
  arrive.

The two limitations this design addresses:

1. **`Sink.Handle` filters out everything without `entry_type`**
   (`internal/cronicle/state/sink.go:50-60`). Anything without that
   attr never reaches `Apply`, so it never reaches `publish`, so it
   never reaches the SSE stream. This includes all warnings, errors,
   and lifecycle prints — exactly the records you most want to see
   when debugging a stuck run.
2. **Coupled to projection write success.** If `Sink.Handle` fails
   to fold an event (counted in `Sink.errs`), `Apply` errored out
   before `publish` ran, so subscribers don't see it either — even
   though stdout did.

## Proposed change

### A new slog handler

```go
// internal/cronicle/state/live_sink.go (new file)

package state

import (
    "context"
    "log/slog"
    "sync"
)

// LiveSink is a slog.Handler that forwards each record to a pub/sub
// registry of subscribers. Sits alongside Sink in the multiHandler
// chain; the two are independent — neither blocks or filters the
// other.
//
// Filter: records without a `run_id` attr are dropped (they're
// process-level lifecycle, not run-level). Subscribers receive the
// raw encoded JSON line (same encoding as the file log) so the wire
// format on /v1/runs/{id}/events stays uniform across replay (events
// table) and live (this handler).
type LiveSink struct {
    subsMu sync.Mutex
    subs   map[*liveSub]struct{}
}

type liveSub struct {
    ch    chan []byte
    runID string // "" matches all
}

func NewLiveSink() *LiveSink { return &LiveSink{} }

func (s *LiveSink) Subscribe(runID string) (<-chan []byte, func()) {
    sub := &liveSub{ch: make(chan []byte, 64), runID: runID}
    s.subsMu.Lock()
    if s.subs == nil {
        s.subs = make(map[*liveSub]struct{})
    }
    s.subs[sub] = struct{}{}
    s.subsMu.Unlock()
    return sub.ch, func() {
        s.subsMu.Lock()
        delete(s.subs, sub)
        s.subsMu.Unlock()
    }
}

func (s *LiveSink) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (s *LiveSink) Handle(_ context.Context, r slog.Record) error {
    runID := extractRunID(r)
    if runID == "" {
        return nil
    }
    line, err := encodeRecord(r) // existing helper in sink.go
    if err != nil {
        return nil
    }
    s.fanout(runID, line)
    return nil
}

func (s *LiveSink) WithAttrs(_ []slog.Attr) slog.Handler { return s }
func (s *LiveSink) WithGroup(_ string) slog.Handler      { return s }

func (s *LiveSink) fanout(runID string, line []byte) {
    s.subsMu.Lock()
    if len(s.subs) == 0 {
        s.subsMu.Unlock()
        return
    }
    targets := make([]*liveSub, 0, len(s.subs))
    for sub := range s.subs {
        if sub.runID == "" || sub.runID == runID {
            targets = append(targets, sub)
        }
    }
    s.subsMu.Unlock()
    for _, t := range targets {
        select {
        case t.ch <- line:
        default:
            // slow consumer; drop. The events table replay covers
            // the gap on reconnect.
        }
    }
}

// extractRunID looks for a top-level run_id attr. We can't use
// slog.Record.Attrs's range-stop semantics from outside the package
// without re-walking the whole attr set — fine, the slice is small.
func extractRunID(r slog.Record) string {
    var out string
    r.Attrs(func(a slog.Attr) bool {
        if a.Key == "run_id" {
            out = a.Value.String()
            return false
        }
        return true
    })
    return out
}
```

`encodeRecord` already exists in `internal/cronicle/state/sink.go:93`
and is the canonical "slog record → JSONL line" encoder. Reuse it
verbatim — keeps live/replay payload shapes identical.

### Wiring into the handler chain

Cronicle's logger setup constructs a `multiHandler` over the active
handlers (see `internal/cronicle/log.go:257`). `LiveSink` is added
alongside the existing handlers wherever the producer initializes its
logger AND has a `state.Store` in scope. Concretely the spot is the
`cronicle run` boot path (search for the existing `state.NewSink(store)`
call).

```go
// pseudocode of the existing setup
liveSink := state.NewLiveSink()
sink := state.NewSink(store)
slog.SetDefault(slog.New(&multiHandler{handlers: []slog.Handler{
    prettyStdout,
    jsonlFile,
    sink,       // existing — projects into SQLite
    liveSink,   // new — fans out to SSE subscribers
}}))

// And expose liveSink to the listener so handleRunRoute can call it.
// Either field on listenServer or pass via constructor.
listenServer{store: store, liveSink: liveSink, …}
```

### Endpoint change

`runEvents` in `listen_control.go` currently does:

```go
live, unsubscribe := store.SubscribeEvents(runID)
```

Becomes:

```go
live, unsubscribe := s.liveSink.Subscribe(runID)
```

Channel element type changes from `state.Event` to `[]byte` (already
JSON). The existing `marshalLiveEvent` helper goes away — `LiveSink`
publishes pre-encoded lines so SSE just writes them through.

`writeSSEEvent(w, lastID, payload)` stays unchanged.

The replay path stays unchanged (`store.EventsSince` → `EventRow.Payload`).

### Wire contract

Unchanged from Phase 1:

```
id: 7
event: cronicle
data: {"time":"…","level":"INFO","msg":"…","entry_type":"task_start","run_id":"…",…}

id: 8
event: cronicle
data: {"time":"…","level":"WARN","msg":"tool execution failed","run_id":"…","tool":"…","err":"…"}
```

The second example above is the new value: a `WARN` record with no
`entry_type` that today is invisible to subscribers but valuable for
debugging. Browser, cronicle-infra proxy, and the `LiveEventLog`
component all keep working as-is — they parse the JSON payload
opaquely and don't depend on `entry_type` being present.

## Filter rules

| Record shape | Today (`Store.publish`) | With `LiveSink` | Why |
|---|---|---|---|
| `task_start`, `agent_run`, `schedule_complete` (has `entry_type`) | streams | streams | unchanged |
| `slog.Error("tool failed", "run_id", X, …)` | dropped (no `entry_type`) | streams | the kind of thing you most want when debugging |
| `slog.Info("config loaded")` (no `run_id`) | dropped | dropped | process-level, not run-level |
| `slog.Info("git clone took 3.2s", "run_id", X)` | dropped | streams | per-run timing useful in the live log |
| Per-turn agent events (future) | requires schema migration | streams as soon as `pkg/agent` adds slog calls with `run_id` | future-proofing |

The single filter rule — **"records with `run_id` attr pass, others
drop"** — is intentionally simple. Subscribers who want narrower
filters (specific entry_type, level, etc.) can do that client-side or
a future query-param (`?level=warn+`, `?entry_type=…`).

## Architecture

Before:

```
slog.Info("…", "run_id", X, "entry_type", "task_start", …)
        │
        ▼
multiHandler ─┬─ prettyStdout
              ├─ jsonlFile
              └─ Sink ──▶ filter has entry_type? ──▶ Apply
                                                        │
                                                        └─▶ publish ──▶ SSE
```

After:

```
slog.Info("…", "run_id", X, …)         # entry_type optional
        │
        ▼
multiHandler ─┬─ prettyStdout
              ├─ jsonlFile
              ├─ Sink ──▶ Apply (unchanged; durable projection)
              └─ LiveSink ──▶ filter has run_id? ──▶ fanout ──▶ SSE
```

`Sink` and `LiveSink` are independent. They share `encodeRecord` for
byte-for-byte payload parity but neither blocks or feeds the other.

## Replay strategy

Unchanged. SSE endpoint:

1. Resolves `sinceID` from `Last-Event-ID` header or `?since=` query.
2. Subscribes to `liveSink` BEFORE the replay query, so anything
   committed during replay is captured.
3. Reads `EventsSince(runID, sinceID)` from the events table and
   writes each `EventRow.Payload` as one SSE message; tracks
   `lastID = max(EventRow.ID)`.
4. Loops on the live channel; assigns each new event an SSE id of
   `++lastID`.
5. De-dupe is left to the client, but in practice the replay/live
   cutover only collides if a row was committed by `Sink.Apply`
   AND emitted by `LiveSink.Handle` for the same record. They could
   both fire if a record has BOTH an `entry_type` AND a `run_id`.

The de-dupe risk above is real and not yet handled. Two options:

**Option A** — replay-only-then-live. Subscribe AFTER replay finishes,
accept the small window where events written between the SELECT and
the Subscribe are missed. Re-fetch with `EventsSince(lastID)` once,
just before declaring "live mode," to backfill that window.

**Option B** — tag records. `LiveSink` adds a process-monotonic event
sequence number to each record before encode; replay query reads it
back from the events table. The SSE endpoint de-dupes by sequence
number. Requires a new column in the events table and a migration.

**Recommendation: Option A.** Simpler; the backfill is one extra
SELECT; no schema change. The window is microseconds.

## Trade-offs

The live stream will carry more bytes than today. Worst case is a
chatty `slog.Info` loop in the agent runtime that fires per-token —
which we don't have today, but if added would flood the SSE stream.
Mitigations:

- The `select { case t.ch <- line: default: drop }` pattern bounds
  the per-subscriber memory at 64 messages.
- A future per-record level filter at the handler (`>= INFO` only,
  for example) can be added cheaply.
- For high-throughput debugging, the JSONL on disk is still the
  authoritative complete record.

## Implementation plan

Estimated 2-4 hours.

1. Create `internal/cronicle/state/live_sink.go` with the code in
   "A new slog handler" above. Tests in `live_sink_test.go`:
   - `Handle` with a record carrying `run_id` reaches a matching
     subscriber.
   - `Handle` with no `run_id` is dropped silently.
   - Slow consumer doesn't block other subscribers.
   - Unsubscribe stops further deliveries; double-unsubscribe is
     safe.
2. Wire `LiveSink` into the handler chain in the producer's logger
   setup. Search for `state.NewSink(` in the codebase to find the
   spot. Keep it gated on the same condition as `state.Sink`
   (no projection, no live stream).
3. Add `liveSink *state.LiveSink` to `listenServer` (or pass via
   constructor — match how `store` is passed).
4. In `listen_control.go` `runEvents` handler, switch the live
   subscription:
   - Before: `live, unsub := store.SubscribeEvents(runID)`
   - After:  `live, unsub := s.liveSink.Subscribe(runID)`
   - Channel type changes from `<-chan Event` to `<-chan []byte`.
   - Drop the `marshalLiveEvent` helper — payloads arrive
     pre-encoded.
   - Implement Option A backfill: after the initial replay loop,
     re-query `EventsSince(runID, lastID)` once and emit any new
     rows before entering the live loop.
5. Delete `state.Store.SubscribeEvents` and `state.Store.publish`
   along with the `subsMu`/`subs` fields on `Store` — `LiveSink`
   replaces them. `state.Event` stays (the projection still uses it).
6. Update `internal/cronicle/state/store_test.go` to drop tests for
   `SubscribeEvents` (move them to `live_sink_test.go` if
   applicable).

## Test plan

- Unit: `live_sink_test.go` per the cases above.
- Integration: existing SSE smoke test — kick a run via
  `/v1/schedules/{name}/trigger`, open SSE on the run, assert at
  least `task_start` + `agent_run` arrive within 5s. Add a new case
  that asserts a `slog.Warn` (or `slog.Error`) without `entry_type`
  but with `run_id` ALSO arrives — this is the regression that proves
  the architecture change works.
- Smoke: `just k8s-up` in `cronicle-infra`, trigger a run, watch
  `/personal/projects/demo/runs/<rid>` in `cronicle-web` — the
  `LiveEventLog` component renders the stream; expanded payloads
  show the new richer event set.

## Open questions

1. Should `LiveSink` carry a level filter at the handler? Today
   stdout shows everything — keeping `LiveSink` symmetric is appealing
   but the cost of "the agent debug-spammed" is higher in SSE than in
   stdout (per-subscriber chan, network bytes). Default to no filter,
   add `?level=` query param later if it bites.
2. `extractRunID` walks the attr slice for every record. For records
   with no `run_id` (which we drop) this is wasted work. Profile this
   if `slog` becomes a hot path; otherwise the slice is small enough
   that it doesn't matter.
3. The `cronicle worker` process also produces slog records during
   task execution. Currently those flow into the worker's stdout
   AND get POSTed to the producer's `/v1/events` ingest endpoint
   (where `Sink.Handle` projects them). Does the worker ALSO need a
   `LiveSink`, or do the producer-side ingest events flow through
   `LiveSink` after they're decoded by the producer? — answer:
   the latter is cleaner. Worker is unchanged. Producer-side
   `handleIngestEvents` should call `liveSink.Handle` (or a
   `LiveSink.Inject(line []byte, runID string)` method) for each
   ingested line, since those records didn't go through the
   producer's slog.

   Alternative: `handleIngestEvents` re-injects the line into slog
   via a thin wrapper — both `Sink` and `LiveSink` see it through the
   normal handler chain. Slightly more elegant but reuses the
   producer's slog for events that originated elsewhere; might cause
   double-logging on stdout. Recommendation: explicit
   `LiveSink.Inject` to avoid the stdout duplication.

## Out of scope

- Per-token streaming from the agent (`anthropic.MessagesStream`
  deltas piped through slog). Doable later: each `text_delta` becomes
  a slog record with `run_id` and a new `entry_type:"text_delta"`.
  No change to `LiveSink`.
- Server-wide firehose (`?run_id=*`). `LiveSink.Subscribe("")` already
  supports it internally; a route would just need a way to
  authenticate the subscriber for that scope.
- Cross-deployment aggregation (the cronicle-infra `/v1/events` style
  fan-in). That's a different layer.

## Why this preserves the original philosophy

The state-plane design doc (`design-state-plane.md`) frames cronicle
as: **logs are the event stream; everything else is a projection.**

Today's SSE — built on `Store.publish` — is a projection of a
projection: events make it through `Sink.Handle`'s `entry_type`
filter, get folded into SQLite, then `publish` fires. Adding a
handler-layer fan-out drops one of those projection layers and puts
SSE consumers on the same level as stdout — closer to the source,
consistent with the philosophy.
