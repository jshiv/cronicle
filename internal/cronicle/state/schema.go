package state

// schemaSQL is the v1 schema for the state plane.
//
// runs:    one row per scheduled execution. Status mutates as events fold in
//          (queued → running → succeeded|failed|canceled). One row in this
//          table corresponds to one schedule trigger (cron tick, HTTP trigger,
//          or `cronicle exec` invocation).
//
// tasks:   one row per task within a run. Status follows the same lifecycle
//          and rolls up into the parent run's status on completion.
//
// events:  append-only mirror of every slog event with entry_type set. Powers
//          GET /v1/runs/{id}/events (live + recent). Bounded by retention
//          window (see internal/cronicle/state/janitor.go); deeper history
//          lives in the JSONL log on disk.
//
// schema_versions: numbered migrations. v1 is everything below; v2+ are
//          appended via additional `schemaSQL_v2` blocks executed if the
//          recorded version is < target.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS schema_versions (
    version    INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS runs (
    run_id      TEXT PRIMARY KEY,
    schedule    TEXT NOT NULL,
    status      TEXT NOT NULL,         -- queued | running | succeeded | failed | canceled
    source      TEXT NOT NULL,         -- cron | http | exec | once
    worker_id   TEXT NOT NULL DEFAULT '',
    started_at  TEXT,                  -- RFC3339 nanos; null until running
    ended_at    TEXT,                  -- RFC3339 nanos; null until terminal
    duration_ms INTEGER,
    cost_usd    REAL NOT NULL DEFAULT 0,
    task_count  INTEGER NOT NULL DEFAULT 0,
    error       TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_runs_schedule_started ON runs(schedule, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_runs_status_started   ON runs(status, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_runs_started          ON runs(started_at DESC);

CREATE TABLE IF NOT EXISTS tasks (
    run_id      TEXT NOT NULL,
    name        TEXT NOT NULL,
    status      TEXT NOT NULL,         -- queued | running | succeeded | failed | canceled
    attempt     INTEGER NOT NULL DEFAULT 1,
    started_at  TEXT,
    ended_at    TEXT,
    duration_ms INTEGER,
    exit_code   INTEGER,
    cost_usd    REAL NOT NULL DEFAULT 0,
    error       TEXT NOT NULL DEFAULT '',
    kind        TEXT NOT NULL DEFAULT '',  -- shell | agent | (empty until first event)
    PRIMARY KEY (run_id, name),
    FOREIGN KEY (run_id) REFERENCES runs(run_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);

CREATE TABLE IF NOT EXISTS events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id     TEXT NOT NULL,
    task       TEXT NOT NULL DEFAULT '',
    entry_type TEXT NOT NULL,
    ts         TEXT NOT NULL,
    payload    TEXT NOT NULL          -- the raw JSON record
);

CREATE INDEX IF NOT EXISTS idx_events_run        ON events(run_id, id);
CREATE INDEX IF NOT EXISTS idx_events_ts         ON events(ts);
`

// schemaSQL_v2 adds the producer-served queue. Jobs persist as rows
// transitioning pending → claimed → done|failed. The single-writer SQLite
// txn that flips status='pending' → 'claimed' on a worker's long-poll IS
// the claim primitive — no race window between "delivered" and "ack",
// because the row's claimed_by column is the authoritative single owner.
//
// Visibility timeout: claim_expires_at is set on claim; a janitor sweeps
// rows with status='claimed' AND claim_expires_at < now back to
// status='pending' so a dead worker's job is re-dispatched.
//
// payload column carries the serialized Schedule JSON the producer
// pushed at enqueue time. Workers decode it identically to how the
// in-process consumer does today.
const schemaSQL_v2 = `
CREATE TABLE IF NOT EXISTS jobs (
    run_id            TEXT PRIMARY KEY,
    schedule          TEXT NOT NULL,
    payload           TEXT NOT NULL,
    status            TEXT NOT NULL,        -- pending | claimed | done | failed
    enqueued_at       TEXT NOT NULL,
    claimed_at        TEXT,
    claimed_by        TEXT NOT NULL DEFAULT '',
    claim_expires_at  TEXT,
    attempt           INTEGER NOT NULL DEFAULT 0,
    last_error        TEXT NOT NULL DEFAULT '',
    completed_at      TEXT
);

CREATE INDEX IF NOT EXISTS idx_jobs_status_enqueued ON jobs(status, enqueued_at);
CREATE INDEX IF NOT EXISTS idx_jobs_claim_expires   ON jobs(claim_expires_at)
  WHERE status = 'claimed';
`

// schemaSQL_v3 adds a workers registry. Phase 3 introduces stop / retry
// semantics, and the operator UI wants to answer "which workers are
// alive, what are they running?" Workers are upserted on every Claim
// and Heartbeat — last_seen is the heartbeat timestamp, current_run
// is the claimed run_id (or empty when idle).
//
// Status values:
//   active   — claimed a job, last seen within visibility window
//   idle     — no current claim; last_seen is the most recent poll/ack
//   stale    — last_seen older than 2× heartbeat cadence (set by janitor)
//
// For now we record the row but compute "stale" on read in ListWorkers
// rather than running a janitor. Cheap to derive, and avoids yet
// another goroutine.
const schemaSQL_v3 = `
CREATE TABLE IF NOT EXISTS workers (
    worker_id    TEXT PRIMARY KEY,
    host         TEXT NOT NULL DEFAULT '',
    last_seen    TEXT NOT NULL,
    current_run  TEXT NOT NULL DEFAULT '',
    claimed_at   TEXT,
    runs_total   INTEGER NOT NULL DEFAULT 0,
    runs_failed  INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_workers_last_seen ON workers(last_seen DESC);
`

const targetSchemaVersion = 3
