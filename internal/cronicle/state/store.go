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
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	// StatusSkipped is recorded for tasks the DAG walker bypassed
	// because their task_state.skipped flag was set. The run that
	// contains skipped tasks can still succeed: skipped is a
	// terminal task state but not a failure mode.
	StatusSkipped = "skipped"
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
	dialect     dialect    // sqlite (default) | postgres
	mu          sync.Mutex // serializes write transactions
	waitOnce    sync.Once
	wait        *jobWaiters
	controlOnce sync.Once
	controlReg2 *controlRegistry

	// aead is the at-rest cipher for the secrets table. nil keeps the
	// historical plaintext-column behavior; setting it via WithAEAD
	// flips PutSecret/GetSecret/ListSecrets to seal/open through
	// value_ct/value_nonce instead. The transition is forward-only —
	// once any row is encrypted, the store must keep its AEAD bound
	// or those rows become unreadable.
	aead *AEAD
}

// WithAEAD binds the at-rest cipher and returns the receiver so
// callers can chain immediately after Open:
//
//	s, _ := state.Open(dsn)
//	s.WithAEAD(aead)
//	_, _ = s.BackfillSecrets()
//
// Idempotent — a second call replaces the prior key, which is the
// intended path for a controlled DEK rotation (operator does a
// re-encrypt pass via PutSecret of every name after swapping).
func (s *Store) WithAEAD(a *AEAD) *Store {
	s.aead = a
	return s
}

// BackfillSecrets re-seals any plaintext rows under the bound AEAD,
// in a single transaction. Returns the number of rows it touched.
//
// Run this once at api startup after WithAEAD. Idempotent: rows that
// already have a non-empty value_ct are skipped, so a redeploy that
// re-invokes the helper is free. With no AEAD bound the call is a
// no-op (returns 0, nil) — operators who haven't opted in keep the
// historical plaintext column behavior with no surprise.
func (s *Store) BackfillSecrets() (int, error) {
	if s == nil || s.aead == nil {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("state.BackfillSecrets: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.Query(`
		SELECT name, value FROM secrets
		WHERE value_ct IS NULL AND value != ''`)
	if err != nil {
		return 0, fmt.Errorf("state.BackfillSecrets: scan: %w", err)
	}
	type pair struct{ name, value string }
	var todo []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.name, &p.value); err != nil {
			rows.Close()
			return 0, fmt.Errorf("state.BackfillSecrets: row: %w", err)
		}
		todo = append(todo, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("state.BackfillSecrets: rows: %w", err)
	}
	n := 0
	for _, p := range todo {
		ct, nonce, err := s.aead.Seal([]byte(p.value), p.name)
		if err != nil {
			return n, fmt.Errorf("state.BackfillSecrets: seal %s: %w", p.name, err)
		}
		if _, err := tx.Exec(`
			UPDATE secrets SET value_ct = ?, value_nonce = ?, value = ''
			WHERE name = ?`, ct, nonce, p.name); err != nil {
			return n, fmt.Errorf("state.BackfillSecrets: update %s: %w", p.name, err)
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		return n, fmt.Errorf("state.BackfillSecrets: commit: %w", err)
	}
	return n, nil
}

// Open returns a Store backed by the given DSN. Use ":memory:" for an
// ephemeral in-memory store (cronicle exec, tests). For on-disk usage,
// pass a filesystem path; the parent directory must already exist.
//
// TODO(postgres): accept postgres:// DSNs to unify config source, secret
// store, and state backend onto one database. Requires:
//   - DSN scheme dispatch (file path → sqlite, postgres:// → pgx)
//   - Schema dialect: AUTOINCREMENT → SERIAL, INSERT OR IGNORE →
//     ON CONFLICT DO NOTHING (affects schema.go + store.go + queue.go +
//     control.go — not just DDL but also runtime queries)
//   - pgx/v5/stdlib driver registration
//   - Integration test against a real Postgres instance
//
// WAL mode is enabled and busy_timeout set to 5s so concurrent writers
// (event ingest + queue claim, once Phase 2 lands) wait rather than
// fail with SQLITE_BUSY.
func Open(dsn string) (*Store, error) {
	if dsn == "" {
		return nil, errors.New("state.Open: empty DSN")
	}
	if isPostgresDSN(dsn) {
		return openPostgres(dsn)
	}
	conn := dsn
	if dsn == ":memory:" {
		// WAL is meaningless for in-memory and not all sqlite builds
		// accept it on :memory:; busy_timeout matters once MaxOpenConns
		// is raised above 1. foreign_keys(on) MUST be applied here too
		// — the tasks table has FOREIGN KEY (run_id) REFERENCES runs
		// with ON DELETE CASCADE, and skipping the pragma in tests +
		// workers (which use :memory: for the local projection) lets
		// orphan rows and missing cascades pass silently in tests
		// while production (file DSN) enforces them.
		conn = ":memory:?_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)"
	} else {
		conn = dsn + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)"
	}
	db, err := sql.Open("sqlite", conn)
	if err != nil {
		return nil, fmt.Errorf("state.Open: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("state.Open: ping: %w", err)
	}
	s := &Store{db: db, dialect: dialectSQLite}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// envIntDefault reads a positive integer from env, falling back to def when
// unset, empty, or unparseable / non-positive.
func envIntDefault(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// openPostgres backs the Store with a shared, durable Postgres instead of a
// local SQLite file. Same schema and queries (via the placeholder-rebind
// driver + DDL dialect transform). This is what a per-deployment producer
// uses in the managed topology so run history survives pod restarts and
// `${last_run}` reads the authoritative record.
func openPostgres(dsn string) (*Store, error) {
	registerPGDriver()
	db, err := sql.Open(pgxRebindDriverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("state.Open(postgres): %w", err)
	}
	// A producer's Store serializes essentially all DB access behind its
	// mutex, so it needs very few connections — but there are MANY producers
	// (one per deployment) sharing one managed Postgres with a small
	// connection cap (e.g. db-s-1vcpu-1gb ≈ 22 usable). Keep the per-producer
	// pool tiny so N producers + the api stay under budget. Overridable via
	// CRONICLE_STATE_MAX_OPEN_CONNS for tuning without an image rebuild.
	maxOpen := envIntDefault("CRONICLE_STATE_MAX_OPEN_CONNS", 2)
	db.SetMaxOpenConns(maxOpen)
	idle := maxOpen - 1
	if idle < 1 {
		idle = 1
	}
	db.SetMaxIdleConns(idle)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("state.Open(postgres): ping: %w", err)
	}
	// When the DSN scopes the producer to a per-deployment schema
	// (search_path=dep_<id>), self-create it so the control plane needs no
	// DB access. Best-effort: a restricted role that can't CREATE SCHEMA
	// must have it pre-provisioned (migrate then fails clearly if missing).
	if schema := schemaFromDSN(dsn); schema != "" {
		_, _ = db.Exec(`CREATE SCHEMA IF NOT EXISTS ` + quoteIdent(schema))
	}
	s := &Store{db: db, dialect: dialectPostgres}
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

// execDDL applies one schema block: dialect-transform for Postgres, then
// run each statement individually (pgx's extended protocol rejects
// multi-statement Exec; splitting is harmless for SQLite too).
func (s *Store) execDDL(block string) error {
	if s.dialect == dialectPostgres {
		block = dialectizePG(block)
	}
	for _, stmt := range splitStatements(block) {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("execDDL: %q: %w", strings.TrimSpace(stmt)[:min(48, len(strings.TrimSpace(stmt)))], err)
		}
	}
	return nil
}

// migrate runs the schema SQL idempotently and records the version.
// Each numbered version's DDL is applied if the recorded version is < target.
// Idempotent on re-runs because every CREATE uses IF NOT EXISTS.
func (s *Store) migrate() error {
	// v1: runs/tasks/events/schema_versions
	if err := s.execDDL(schemaSQL); err != nil {
		return fmt.Errorf("state.migrate v1: %w", err)
	}
	var current int
	row := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_versions`)
	if err := row.Scan(&current); err != nil {
		return fmt.Errorf("state.migrate: read version: %w", err)
	}
	// Per-version migration: run DDL, immediately record the version
	// row. Previously DDLs ran in a batch and versions were inserted
	// in a single loop at the end — if the process crashed between
	// running v10's ALTER TABLE and writing the version row, the next
	// startup re-ran the ALTER, which fails with "duplicate column
	// name" because SQLite's ALTER TABLE ADD COLUMN has no
	// IF NOT EXISTS form. Per-version recording shrinks the window
	// dramatically (no other DDL between ALTER and version write) and
	// recordVersion(10) below is also called from a v10-specific
	// idempotent path that checks column existence before ALTERing.
	steps := []struct {
		v   int
		ddl string
		run func() error // optional override for non-idempotent steps
	}{
		{2, schemaSQL_v2, nil},
		{3, schemaSQL_v3, nil},
		{4, schemaSQL_v4, nil},
		{5, schemaSQL_v5, nil},
		{6, schemaSQL_v6, nil},
		{7, schemaSQL_v7, nil},
		{8, schemaSQL_v8, nil},
		{9, schemaSQL_v9, nil},
		{10, schemaSQL_v10, s.migrateV10Idempotent},
	}
	for _, step := range steps {
		if current >= step.v {
			continue
		}
		if step.run != nil {
			if err := step.run(); err != nil {
				return fmt.Errorf("state.migrate v%d: %w", step.v, err)
			}
		} else if err := s.execDDL(step.ddl); err != nil {
			return fmt.Errorf("state.migrate v%d: %w", step.v, err)
		}
		if _, err := s.db.Exec(`INSERT INTO schema_versions(version) VALUES (?)`, step.v); err != nil {
			return fmt.Errorf("state.migrate: record version %d: %w", step.v, err)
		}
	}
	return nil
}

// migrateV10Idempotent runs the v10 secrets-encryption columns ALTER
// only when the columns aren't already present. The probe shape is
// dialect-specific because SQLite uses PRAGMA table_info while
// Postgres uses information_schema.columns — running PRAGMA against
// Postgres errors with "syntax error at or near 'PRAGMA'" and crashes
// the producer on its first startup against PG. This was the original
// M7 fix's blind spot.
//
// For Postgres specifically, we could just rely on `ADD COLUMN
// IF NOT EXISTS` (which PG supports), but the same probe-then-DDL
// pattern works cleanly across both dialects and keeps the
// re-run-safety invariant identical.
func (s *Store) migrateV10Idempotent() error {
	if s.dialect == dialectPostgres {
		return s.migrateV10IdempotentPG()
	}
	return s.migrateV10IdempotentSQLite()
}

func (s *Store) migrateV10IdempotentSQLite() error {
	rows, err := s.db.Query(`PRAGMA table_info(secrets)`)
	if err != nil {
		return fmt.Errorf("probe secrets schema: %w", err)
	}
	defer rows.Close()
	hasCT, hasNonce := false, false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return fmt.Errorf("scan secrets schema: %w", err)
		}
		switch name {
		case "value_ct":
			hasCT = true
		case "value_nonce":
			hasNonce = true
		}
	}
	if hasCT && hasNonce {
		return nil
	}
	return s.execDDL(schemaSQL_v10)
}

// migrateV10IdempotentPG is the Postgres counterpart. PG supports
// ALTER TABLE ADD COLUMN IF NOT EXISTS natively, but using it here
// would couple migration semantics to the DDL string. Probing via
// information_schema keeps the structural shape identical to the
// SQLite path — both branches "check, then DDL when missing."
func (s *Store) migrateV10IdempotentPG() error {
	rows, err := s.db.Query(`
		SELECT column_name
		FROM information_schema.columns
		WHERE table_name = 'secrets'
		  AND column_name IN ('value_ct', 'value_nonce')`)
	if err != nil {
		return fmt.Errorf("probe secrets schema: %w", err)
	}
	defer rows.Close()
	hasCT, hasNonce := false, false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scan secrets schema: %w", err)
		}
		switch name {
		case "value_ct":
			hasCT = true
		case "value_nonce":
			hasNonce = true
		}
	}
	if hasCT && hasNonce {
		return nil
	}
	return s.execDDL(schemaSQL_v10)
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
	case "task_skipped":
		// DAG walker bypassed this task because task_state.skipped=1.
		// Terminal state, doesn't roll up cost or exit_code, sets
		// kind='' since no body ran.
		return s.applyTaskSkipped(tx, e)
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

// applyTaskSkipped marks a task row terminal with status='skipped'.
// Idempotent: re-emitting the same skip event won't move the row. The
// task may not have a row yet (skip is detected before task_start fires),
// so we INSERT-or-UPDATE with both started_at and ended_at set to the
// same timestamp — duration_ms=0 conveys "this never executed."
func (s *Store) applyTaskSkipped(tx *sql.Tx, e Event) error {
	ts := e.Time.UTC().Format(time.RFC3339Nano)
	_, err := tx.Exec(`
		INSERT INTO tasks(run_id, name, status, started_at, ended_at, duration_ms, kind, error)
		VALUES (?, ?, ?, ?, ?, 0, '', ?)
		ON CONFLICT(run_id, name) DO UPDATE SET
		    status = excluded.status,
		    started_at = COALESCE(tasks.started_at, excluded.started_at),
		    ended_at = excluded.ended_at,
		    duration_ms = 0,
		    error = excluded.error
		`,
		e.RunID, e.Task, StatusSkipped, ts, ts, e.Error,
	)
	if err != nil {
		return fmt.Errorf("applyTaskSkipped: %w", err)
	}
	return nil
}

func (s *Store) applyTrigger(tx *sql.Tx, e Event) error {
	source := e.Source
	if source == "" {
		source = SourceHTTP // trigger events come from the listener
	}
	_, err := tx.Exec(`
		INSERT INTO runs(run_id, schedule, status, source, started_at)
		VALUES (?, ?, ?, ?, NULL)
		ON CONFLICT (run_id) DO NOTHING`,
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
			INSERT INTO tasks(run_id, name, status)
			VALUES (?, ?, ?)
			ON CONFLICT (run_id, name) DO NOTHING`,
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
