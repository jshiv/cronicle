# cronicle v0.5.0 — release highlights

- Broker-less distributed mode: SQLite-backed jobs queue, workers
  consume via HTTP long-poll. Redis/NSQ support removed.
- State plane: every fire of a schedule gets a `run_id` and is
  projected into `.cronicle/state.db` for `/v1/runs` queries.
- Cancel / retry / resume verbs on the listener API. Resume skips
  already-succeeded tasks and re-runs the rest.
- Worker registry at `/v1/workers` with derived status
  (active / idle / stale).
- SSE control channel for low-latency cancel signal routing.
- `pkg/exec` consolidated around `os/exec.CommandContext`; shell
  tasks now honor per-run cancellation.
