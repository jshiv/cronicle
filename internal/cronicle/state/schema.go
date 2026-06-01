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
// events:  slim chronology ledger — one row per structural slog event
//          (entry_type-bearing) so the API can answer "what happened in
//          this run, in what order?" without parsing JSONL. NO payload
//          column: the rich bytes live in cronicle.jsonl (and Loki, when
//          shipped). This table is metadata only; pretty/text/json bytes
//          come from Loki for history, from LiveSink for live.
//
// schema_versions: numbered migrations. v1 is everything below; v2+ are
//          appended via additional `schemaSQL_v2` blocks executed if the
//          recorded version is < target.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS schema_versions (
    version    INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL DEFAULT (CURRENT_TIMESTAMP)
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
    ts         TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_events_run ON events(run_id, id);
CREATE INDEX IF NOT EXISTS idx_events_ts  ON events(ts);
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

// schemaSQL_v4 slims the events table. Previously `payload` stored the
// full JSON line — duplicate of cronicle.jsonl and the Loki ingest, and
// the source of the unbounded-growth concern. After v4, events is a
// chronology ledger only: id, run_id, task, entry_type, ts. The actual
// log bytes live in cronicle.jsonl → Loki for history, and in LiveSink
// for live.
//
// Aggressive rebuild rather than ALTER TABLE DROP COLUMN: keeps the
// SQL trivial and we don't carry forward old payloads anyway.
const schemaSQL_v4 = `
DROP TABLE IF EXISTS events;

CREATE TABLE events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id     TEXT NOT NULL,
    task       TEXT NOT NULL DEFAULT '',
    entry_type TEXT NOT NULL,
    ts         TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_events_run ON events(run_id, id);
CREATE INDEX IF NOT EXISTS idx_events_ts  ON events(ts);
`

// schemaSQL_v5 adds the schedule_state table — the control-plane row
// that lives alongside the HCL definition. The HCL says "what to run";
// schedule_state says "is it currently allowed to run?"
//
// This is the first piece of true runtime control state: the cron loop
// consults schedule_state.paused on every tick to decide whether to
// enqueue the run, and operators flip it via the listener (POST
// /v1/schedules/{name}/pause). Hot-reloading the HCL to set cron=""
// is too rudimentary (it conflates definition and control) and round-
// trips through the config source; this row is authoritative state
// inside the runner.
//
// Rows are sparse: only paused/drained schedules need rows. The cron
// loop's pause check is LEFT JOIN, so unknown schedules default to
// "active". Drained semantics (Phase 4) are reserved here so we don't
// re-migrate later.
const schemaSQL_v5 = `
CREATE TABLE IF NOT EXISTS schedule_state (
    name       TEXT PRIMARY KEY,
    paused     INTEGER NOT NULL DEFAULT 0,   -- 0|1
    paused_at  TEXT NOT NULL DEFAULT '',
    paused_by  TEXT NOT NULL DEFAULT '',
    reason     TEXT NOT NULL DEFAULT '',
    drained    INTEGER NOT NULL DEFAULT 0,   -- 0|1, reserved for Phase 4
    updated_at TEXT NOT NULL DEFAULT (CURRENT_TIMESTAMP)
);
`

// schemaSQL_v6 adds task_state — per-task control rows that flag a task
// as "skipped" so the DAG walker treats it as a no-op success without
// executing the body. Definition-independent (operators can skip without
// editing HCL), persists across runs, and survives runner restart.
//
// Composite key (schedule, task): the same task name may exist in
// multiple schedules and shouldn't share skip state. Sparse — only
// skipped tasks have rows.
//
// Dependency semantics: a skipped task's dependents still run; their
// depends_on edge is satisfied "vacuously". An operator who wants to
// halt the chain should pause the schedule, not skip the head task.
// Cascade-skip (mark dependents skipped too) is reserved for a future
// verb so the default remains predictable.
const schemaSQL_v6 = `
CREATE TABLE IF NOT EXISTS task_state (
    schedule    TEXT NOT NULL,
    task        TEXT NOT NULL,
    skipped     INTEGER NOT NULL DEFAULT 0,
    skipped_at  TEXT NOT NULL DEFAULT '',
    skipped_by  TEXT NOT NULL DEFAULT '',
    reason      TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL DEFAULT (CURRENT_TIMESTAMP),
    PRIMARY KEY (schedule, task)
);
`

// schemaSQL_v7 adds run_state — per-run-instance pause flag. Distinct
// from schedule_state (which pauses *future* ticks) and task_state
// (which skips tasks at definition time). run_state is the "freeze the
// DAG walker mid-execution" verb: in-flight tasks finish, but the
// walker blocks before launching the next layer until the run is
// unpaused.
//
// Composite key not needed: run_id is globally unique. Sparse rows.
// The runs projection table is left untouched so the read model for
// "what's the status of run X" stays event-derived; control state
// lives here.
const schemaSQL_v7 = `
CREATE TABLE IF NOT EXISTS run_state (
    run_id      TEXT PRIMARY KEY,
    paused      INTEGER NOT NULL DEFAULT 0,
    paused_at   TEXT NOT NULL DEFAULT '',
    paused_by   TEXT NOT NULL DEFAULT '',
    reason      TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL DEFAULT (CURRENT_TIMESTAMP)
);
`

// schemaSQL_v8 adds runner_state — a singleton row holding runner-wide
// control flags. Today it carries `drained`: a global "stop accepting
// new work" verb that gates the cron tick, the listener triggers, and
// the DAG walker's task launches. In-flight tasks finish normally;
// once drained is cleared, work resumes from where each gate stopped.
//
// CHECK (id = 1) enforces the singleton: there's at most one row, and
// upsert always targets it. Future runner-wide flags (max_concurrency,
// scheduler_offset, etc.) layer in here without a fresh migration.
const schemaSQL_v8 = `
CREATE TABLE IF NOT EXISTS runner_state (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    drained     INTEGER NOT NULL DEFAULT 0,
    drained_at  TEXT NOT NULL DEFAULT '',
    drained_by  TEXT NOT NULL DEFAULT '',
    reason      TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL DEFAULT (CURRENT_TIMESTAMP)
);
INSERT INTO runner_state(id) VALUES (1) ON CONFLICT (id) DO NOTHING;
`

// schemaSQL_v9 adds the secrets table — the in-tree key/value store
// the SecretStore role on Backend reads + writes. Storage matches the
// rest of the runtime-control state plane (one common pathway) rather
// than sitting alongside in a sibling subsystem.
//
// `version` is a monotonic per-row counter, bumped on every upsert.
// SecretsEtag() returns max(version) over the table; HTTP serves it as
// the ETag so workers' If-None-Match short-circuits unchanged
// responses. Deletes also bump version (via a marker row? no — see
// below): the SQLite implementation uses a separate `secrets_version`
// singleton that's incremented on every mutation, so deletes are
// visible to etag-based observers without needing tombstones.
//
// `value` is plaintext at the row level. File mode on state.db is
// 0600, mirroring how cronicle handles transcripts. AEAD-at-rest
// (XChaCha20-Poly1305 with a DEK from env, mirroring cronicle-infra's
// pipeline) is a follow-up — adding it later is an in-place column
// swap, the SecretStore API surface doesn't change.
//
// Audit columns (updated_at, updated_by) are minimal on purpose; the
// rich audit trail flows through slog → events table → Loki, same as
// the rest of the runtime-control surface.
const schemaSQL_v9 = `
CREATE TABLE IF NOT EXISTS secrets (
    name        TEXT PRIMARY KEY,
    value       TEXT NOT NULL,
    version     INTEGER NOT NULL DEFAULT 1,
    updated_at  TEXT NOT NULL DEFAULT (CURRENT_TIMESTAMP),
    updated_by  TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS secrets_meta (
    id              INTEGER PRIMARY KEY CHECK (id = 1),
    etag_counter    INTEGER NOT NULL DEFAULT 0
);
INSERT INTO secrets_meta(id) VALUES (1) ON CONFLICT (id) DO NOTHING;
`

// schemaSQL_v10 adds at-rest envelope encryption to the secrets table.
// Two new columns hold the ciphertext and per-row nonce; the existing
// `value` column is kept (and written as the empty string for new rows)
// for one release as a rollback path — a prior cronicle binary that
// hasn't learned about ct/nonce will read empty plaintexts rather than
// crash. A follow-up migration will DROP COLUMN once rollout settles.
//
// The seal/open round-trip is performed in Go by the Store when an
// AEAD has been bound via WithAEAD; without it the table works exactly
// as before (plaintext column only). This keeps local-dev runs free of
// any DEK ceremony.
const schemaSQL_v10 = `
ALTER TABLE secrets ADD COLUMN value_ct    BLOB;
ALTER TABLE secrets ADD COLUMN value_nonce BLOB;
`

const targetSchemaVersion = 10
