// Package state implements the cronicle state plane: an SQLite-backed
// projection of slog events into queryable run/task tables.
//
// Logs remain authoritative for what happened (cronicle.jsonl on disk).
// The projection is a derived, retention-windowed view that powers live
// status APIs (GET /v1/runs, /v1/runs/{id}). Delete the state DB and the
// log on disk still has the truth — the projection rebuilds from event
// ingest as new events arrive.
package state

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo required
)

// Status values as recorded in runs.status / tasks.status.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCanceled  = "canceled"
)

// Source values as recorded in runs.source.
const (
	SourceCron = "cron"
	SourceHTTP = "http"
	SourceExec = "exec"
	SourceOnce = "once"
)

// Store is the projection store. Concurrent-safe — internal mutex
// serializes Apply() so out-of-order events (e.g. a shell_run racing
// schedule_complete on a fast task) don't interleave their UPDATEs.
//
// Phase 2b adds queue methods (Enqueue/Claim/Ack/Heartbeat/Reap) that
// share the same mutex. wait/waitOnce gate the lazy initialization of
// the long-poll wakeup primitive so single-node mode (no queue methods
// ever called) doesn't pay for it.
//
// Phase 3 adds controlOnce/controlReg2 for the SSE worker control
// channel — workers subscribe and receive cancel signals from the
// producer. Lazy-init keeps single-node mode free of the cost.
type Store struct {
	db          *sql.DB
	mu          sync.Mutex // serializes write transactions
	waitOnce    sync.Once
	wait        *jobWaiters
	controlOnce sync.Once
	controlReg2 *controlRegistry
}

// Open returns a Store backed by the given DSN. Use ":memory:" for an
// ephemeral in-memory store (cronicle exec, tests). For on-disk usage,
// pass a filesystem path; the parent directory must already exist.
//
// WAL mode is enabled and busy_timeout set to 5s so concurrent writers
// (event ingest + queue claim, once Phase 2 lands) wait rather than
// fail with SQLITE_BUSY.
func Open(dsn string) (*Store, error) {
	if dsn == "" {
		return nil, errors.New("state.Open: empty DSN")
	}
	// modernc.org/sqlite registers as "sqlite". The pragmas pass through
	// the connection string so they apply to every connection in the pool.
	conn := dsn
	if dsn != ":memory:" {
		conn = dsn + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)"
	}
	db, err := sql.Open("sqlite", conn)
	if err != nil {
		return nil, fmt.Errorf("state.Open: %w", err)
	}
	// Pool size of 1 keeps writes serialized at the connection level on
	// top of the in-process mutex; SQLite WAL serializes anyway, but this
	// avoids transient SQLITE_BUSY on the in-memory variant where
	// busy_timeout pragmas don't help.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("state.Open: ping: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// OpenFile is a convenience for `cronicle run`: opens a state.db inside
// croniclePath/.cronicle/. Caller is responsible for ensuring the
// directory exists (EnableFileLog already mkdirs .cronicle/log).
func OpenFile(croniclePath string) (*Store, error) {
	return Open(filepath.Join(croniclePath, ".cronicle", "state.db"))
}

// Close releases the underlying connection.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// migrate runs the schema SQL idempotently and records the version.
// Each numbered version's DDL is applied if the recorded version is < target.
// Idempotent on re-runs because every CREATE uses IF NOT EXISTS.
func (s *Store) migrate() error {
	// v1: runs/tasks/events/schema_versions
	if _, err := s.db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("state.migrate v1: %w", err)
	}
	var current int
	row := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_versions`)
	if err := row.Scan(&current); err != nil {
		return fmt.Errorf("state.migrate: read version: %w", err)
	}
	// v2: jobs (queue table)
	if current < 2 {
		if _, err := s.db.Exec(schemaSQL_v2); err != nil {
			return fmt.Errorf("state.migrate v2: %w", err)
		}
	}
	// v3: workers (registry)
	if current < 3 {
		if _, err := s.db.Exec(schemaSQL_v3); err != nil {
			return fmt.Errorf("state.migrate v3: %w", err)
		}
	}
	// v4: slim events ledger (drop payload column on upgrade)
	if current < 4 {
		if _, err := s.db.Exec(schemaSQL_v4); err != nil {
			return fmt.Errorf("state.migrate v4: %w", err)
		}
	}
	if current >= targetSchemaVersion {
		return nil
	}
	for v := current + 1; v <= targetSchemaVersion; v++ {
		if _, err := s.db.Exec(`INSERT INTO schema_versions(version) VALUES (?)`, v); err != nil {
			return fmt.Errorf("state.migrate: record version %d: %w", v, err)
		}
	}
	return nil
}

// Apply folds an event into the projection. Idempotent on event re-delivery
// at the row level: schedule_start INSERT-OR-IGNORE, task UPDATEs are
// monotonic (running → succeeded|failed|canceled never reverses).
//
// Errors are returned but most callers (the slog tee) log-and-discard —
// a write failure on a derived store should never crash the run that's
// actually doing work.
func (s *Store) Apply(e Event) error {
	if e.RunID == "" || e.EntryType == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("state.Apply: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := s.recordEvent(tx, e); err != nil {
		return err
	}
	if err := s.foldEvent(tx, e); err != nil {
		return err
	}
	return tx.Commit()
}

// recordEvent appends a chronology row to the events ledger. Metadata
// only — no payload. The rich bytes for an event live in cronicle.jsonl
// (and Loki, when shipped); this row just answers "what entry_type fired
// at which timestamp for this run/task?" so /v1/runs/{id}/events can
// return a coarse timeline without parsing JSONL.
func (s *Store) recordEvent(tx *sql.Tx, e Event) error {
	_, err := tx.Exec(
		`INSERT INTO events(run_id, task, entry_type, ts) VALUES (?,?,?,?)`,
		e.RunID, e.Task, e.EntryType, e.Time.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("state.recordEvent: %w", err)
	}
	return nil
}

// foldEvent dispatches to the per-entry-type handler that updates runs/tasks.
func (s *Store) foldEvent(tx *sql.Tx, e Event) error {
	switch e.EntryType {
	case "schedule_start":
		return s.applyScheduleStart(tx, e)
	case "task_start":
		return s.applyTaskStart(tx, e)
	case "shell_run", "shell_run_streamed":
		return s.applyTaskTerminal(tx, e, "shell")
	case "agent_run", "agent_run_streamed":
		return s.applyTaskTerminal(tx, e, "agent")
	case "schedule_complete":
		return s.applyScheduleComplete(tx, e)
	case "trigger":
		// Pre-state: a trigger arrived but the run hasn't started executing
		// yet. Insert a placeholder row so /v1/runs?status=queued works.
		// schedule_start (which fires immediately after for in-process runs)
		// will UPDATE this row to running.
		return s.applyTrigger(tx, e)
	}
	return nil // unknown entry_type — ignore (forward-compat)
}

func (s *Store) applyTrigger(tx *sql.Tx, e Event) error {
	source := e.Source
	if source == "" {
		source = SourceHTTP // trigger events come from the listener
	}
	_, err := tx.Exec(`
		INSERT OR IGNORE INTO runs(run_id, schedule, status, source, started_at)
		VALUES (?, ?, ?, ?, NULL)`,
		e.RunID, e.Schedule, StatusQueued, source,
	)
	return err
}

func (s *Store) applyScheduleStart(tx *sql.Tx, e Event) error {
	ts := e.Time.UTC().Format(time.RFC3339Nano)
	source := e.Source
	if source == "" {
		source = SourceCron // pre-Phase-2 fallback for events without a source
	}
	// INSERT OR REPLACE would clobber a queued row's source; use upsert with
	// COALESCE-style merging by running INSERT then UPDATE if a row existed.
	_, err := tx.Exec(`
		INSERT INTO runs(run_id, schedule, status, source, started_at, task_count, worker_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET
		    status = excluded.status,
		    started_at = excluded.started_at,
		    task_count = excluded.task_count,
		    worker_id = CASE WHEN excluded.worker_id != '' THEN excluded.worker_id ELSE runs.worker_id END
		`,
		e.RunID, e.Schedule, StatusRunning, source, ts, len(e.Tasks), e.WorkerID,
	)
	if err != nil {
		return fmt.Errorf("applyScheduleStart: upsert run: %w", err)
	}
	for _, name := range e.Tasks {
		_, err := tx.Exec(`
			INSERT OR IGNORE INTO tasks(run_id, name, status)
			VALUES (?, ?, ?)`,
			e.RunID, name, StatusQueued,
		)
		if err != nil {
			return fmt.Errorf("applyScheduleStart: insert task %q: %w", name, err)
		}
	}
	return nil
}

func (s *Store) applyTaskStart(tx *sql.Tx, e Event) error {
	ts := e.Time.UTC().Format(time.RFC3339Nano)
	attempt := max(e.Attempt, 1)
	// Insert if missing (in case task_start arrives before schedule_start
	// for whatever reason), otherwise update to running.
	_, err := tx.Exec(`
		INSERT INTO tasks(run_id, name, status, attempt, started_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(run_id, name) DO UPDATE SET
		    status = excluded.status,
		    attempt = excluded.attempt,
		    started_at = COALESCE(tasks.started_at, excluded.started_at)
		`,
		e.RunID, e.Task, StatusRunning, attempt, ts,
	)
	return err
}

func (s *Store) applyTaskTerminal(tx *sql.Tx, e Event, kind string) error {
	ts := e.Time.UTC().Format(time.RFC3339Nano)
	status := StatusSucceeded
	if e.Success != nil && !*e.Success {
		status = StatusFailed
	}
	var exitCode *int
	if e.Exit != nil {
		exitCode = e.Exit
	}
	var dur *int64
	if e.DurationMs != nil {
		dur = e.DurationMs
	}
	var cost float64
	if e.CostUSD != nil {
		cost = *e.CostUSD
	}

	// Sticky cancel: if a run was explicitly canceled (by /v1/runs/{id}/cancel),
	// a subsequent shell_run/agent_run carrying success=false from the
	// SIGTERM'd process shouldn't promote the status back to failed —
	// the operator's intent is what landed first. Preserve status when
	// the existing row is already 'canceled'.
	_, err := tx.Exec(`
		INSERT INTO tasks(run_id, name, status, started_at, ended_at, duration_ms, exit_code, cost_usd, error, kind)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id, name) DO UPDATE SET
		    status = CASE WHEN tasks.status = 'canceled' THEN tasks.status ELSE excluded.status END,
		    ended_at = excluded.ended_at,
		    duration_ms = COALESCE(excluded.duration_ms, tasks.duration_ms),
		    exit_code = COALESCE(excluded.exit_code, tasks.exit_code),
		    cost_usd = excluded.cost_usd,
		    error = CASE WHEN tasks.status = 'canceled' THEN tasks.error ELSE excluded.error END,
		    kind = excluded.kind
		`,
		e.RunID, e.Task, status, ts, ts, dur, exitCode, cost, e.Error, kind,
	)
	if err != nil {
		return fmt.Errorf("applyTaskTerminal: %w", err)
	}
	if cost > 0 {
		// Roll the agent cost up to the run total.
		_, err := tx.Exec(`UPDATE runs SET cost_usd = cost_usd + ? WHERE run_id = ?`, cost, e.RunID)
		if err != nil {
			return fmt.Errorf("applyTaskTerminal: cost rollup: %w", err)
		}
	}
	return nil
}

func (s *Store) applyScheduleComplete(tx *sql.Tx, e Event) error {
	ts := e.Time.UTC().Format(time.RFC3339Nano)
	status := StatusSucceeded
	if e.Success != nil && !*e.Success {
		status = StatusFailed
	}
	var dur *int64
	if e.DurationMs != nil {
		dur = e.DurationMs
	}
	taskCount := e.TaskCount
	// Sticky cancel: preserve the canceled status if cancel won the race
	// with the post-SIGTERM schedule_complete event.
	_, err := tx.Exec(`
		UPDATE runs SET
		    status = CASE WHEN status = 'canceled' THEN status ELSE ? END,
		    ended_at = ?,
		    duration_ms = COALESCE(?, duration_ms),
		    task_count = CASE WHEN ? > 0 THEN ? ELSE task_count END,
		    error = CASE WHEN status = 'canceled' THEN error ELSE ? END
		WHERE run_id = ?`,
		status, ts, dur, taskCount, taskCount, e.Error, e.RunID,
	)
	return err
}
