# State plane architecture (excerpt)

The producer process owns three responsibilities:
1. **Scheduling** — `robfig/cron` ticks fire `ProduceSchedule` to push
   schedule JSON onto a queue (in-memory chan or SQLite jobs table).
2. **State** — slog handler chain tees every event into the projection,
   so the runs/tasks/events tables always reflect what slog wrote.
3. **Control** — the HTTP listener exposes triggers, run queries,
   cancel/retry/resume, the worker registry, and the SSE channel.

Workers are HTTP clients only. They long-poll `/v1/jobs`, run the
DAG, ship events back via `POST /v1/events`, ack via `POST
/v1/jobs/{id}/ack`. The SSE channel `/v1/workers/{id}/control`
gives them low-latency cancel preemption.

The atomic claim primitive is `BEGIN IMMEDIATE; UPDATE jobs SET
status='claimed' WHERE id=? AND status='pending'`. SQLite WAL
serializes concurrent writers, so two workers racing on one row
can't both win — the `WHERE` clause filters the loser.
