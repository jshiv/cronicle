# Cronicle on Loki + Grafana

Stand up a local log dashboard for cronicle: Vector tails the structured-JSON log file, ships to Loki, Grafana shows a pre-provisioned dashboard.

## Why this stack

- **Vector** parses each line of `cronicle.jsonl` and forwards to Loki. We tail the file (not stdout) so cronicle's pretty stdout for humans coexists with structured logs to disk; Vector also checkpoints its position so a restart doesn't lose lines.
- **Loki** is the log store. Stable fields (`entry_type`, `schedule`, `task`, `success`) become labels; everything else (`cost_usd`, `model`, `mcp_servers`, `skills_loaded`, `transcript`, …) lives in the log line and is queryable via LogQL JSON parsing.
- **Grafana** comes pre-provisioned with the Loki datasource and a "Cronicle Runs" dashboard. No click-through after `docker compose up`.

## Run cronicle with file logging

```bash
# From the repo root, init a demo config (creates ./demo/cronicle.hcl).
./cronicle init --path demo

# Run with --log-to-file so cronicle writes .cronicle/log/cronicle.jsonl
# and per-run agent transcripts under .cronicle/runs/.
./cronicle run --path demo/cronicle.hcl --log-to-file
```

Cronicle's `--log-to-file` is independent of stdout — pretty stdout for humans + tail-able JSON file at the same time is the intended composition.

## Bring up the dashboard

```bash
# CRONICLE_PATH points at the cronicle-managed dir (the one with
# .cronicle/log inside). Defaults to ../.. which is the repo root.
CRONICLE_PATH=$(pwd) docker compose -f deploy/local/docker-compose.yaml up -d
```

Then open <http://localhost:3000> — anonymous access is enabled, you land directly on the Cronicle Runs dashboard.

## What the dashboard shows

| Panel | LogQL |
|---|---|
| Recent runs | `{app="cronicle", entry_type=~"shell_run.*|agent_run.*"}` |
| Runs per minute (by schedule) | `sum by (schedule) (count_over_time({app="cronicle", entry_type=~"shell_run.*|agent_run.*"}[1m]))` |
| Failures | `sum(count_over_time({app="cronicle", success="false"}[$__range]))` |
| Agent cost (USD, sum) | `sum by (schedule) (sum_over_time({app="cronicle", entry_type=~"agent_run.*"} \| json \| unwrap cost_usd [5m]))` |
| Agent duration p95 (ms) | `quantile_over_time(0.95, ... \| unwrap duration_ms [5m])` |

The dashboard is the starting point. From the Explore tab you can pivot on any field cronicle emits — token counts, MCP server names, skills loaded, etc. Set up alerts on failure rate or cost ceiling thresholds from there.

## Tear down

```bash
docker compose -f deploy/local/docker-compose.yaml down
```

Volumes are not persisted — Loki's data is gone after `down`. If you want history across restarts, add a named volume on the `loki` service.
