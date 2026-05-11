package state

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Run is the API-facing shape of a row in the runs table, joined with
// its tasks. JSON tags match the /v1/runs response.
type Run struct {
	RunID      string    `json:"run_id"`
	Schedule   string    `json:"schedule"`
	Status     string    `json:"status"`
	Source     string    `json:"source"`
	WorkerID   string    `json:"worker_id,omitempty"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	EndedAt    time.Time `json:"ended_at,omitzero"`
	DurationMs int64     `json:"duration_ms,omitempty"`
	CostUSD    float64   `json:"cost_usd,omitempty"`
	TaskCount  int       `json:"task_count"`
	Error      string    `json:"error,omitempty"`
	Tasks      []Task    `json:"tasks,omitempty"`
}

// Task is the API-facing per-task shape inside a Run.
type Task struct {
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Attempt    int       `json:"attempt,omitempty"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	EndedAt    time.Time `json:"ended_at,omitzero"`
	DurationMs int64     `json:"duration_ms,omitempty"`
	ExitCode   *int      `json:"exit_code,omitempty"`
	CostUSD    float64   `json:"cost_usd,omitempty"`
	Error      string    `json:"error,omitempty"`
	Kind       string    `json:"kind,omitempty"`
}

// ListFilter parameterizes ListRuns. Zero values mean "no filter".
type ListFilter struct {
	Status   string    // exact match: queued|running|succeeded|failed|canceled
	Schedule string    // exact match
	Since    time.Time // started_at >= Since (zero means no lower bound)
	Limit    int       // page size; 0 → 50, max 500
}

// ListRuns returns runs newest-first. Tasks are NOT loaded here — callers
// that want per-task detail should call GetRun for the specific run_id.
// This keeps /v1/runs cheap when the operator is just scanning recent
// activity.
func (s *Store) ListRuns(f ListFilter) ([]Run, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	var (
		conds []string
		args  []any
	)
	if f.Status != "" {
		conds = append(conds, "status = ?")
		args = append(args, f.Status)
	}
	if f.Schedule != "" {
		conds = append(conds, "schedule = ?")
		args = append(args, f.Schedule)
	}
	if !f.Since.IsZero() {
		conds = append(conds, "started_at >= ?")
		args = append(args, f.Since.UTC().Format(time.RFC3339Nano))
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	q := fmt.Sprintf(`
		SELECT run_id, schedule, status, source, worker_id,
		       COALESCE(started_at, ''), COALESCE(ended_at, ''),
		       COALESCE(duration_ms, 0), cost_usd, task_count, error
		FROM runs
		%s
		ORDER BY COALESCE(started_at, '') DESC, run_id DESC
		LIMIT ?`, where)
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("ListRuns: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var (
			r          Run
			startedStr string
			endedStr   string
		)
		if err := rows.Scan(&r.RunID, &r.Schedule, &r.Status, &r.Source, &r.WorkerID,
			&startedStr, &endedStr, &r.DurationMs, &r.CostUSD, &r.TaskCount, &r.Error); err != nil {
			return nil, err
		}
		r.StartedAt = parseTime(startedStr)
		r.EndedAt = parseTime(endedStr)
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRun returns one run with its tasks attached, or sql.ErrNoRows if the
// run_id is unknown.
func (s *Store) GetRun(runID string) (Run, error) {
	var (
		r          Run
		startedStr string
		endedStr   string
	)
	row := s.db.QueryRow(`
		SELECT run_id, schedule, status, source, worker_id,
		       COALESCE(started_at, ''), COALESCE(ended_at, ''),
		       COALESCE(duration_ms, 0), cost_usd, task_count, error
		FROM runs WHERE run_id = ?`, runID)
	if err := row.Scan(&r.RunID, &r.Schedule, &r.Status, &r.Source, &r.WorkerID,
		&startedStr, &endedStr, &r.DurationMs, &r.CostUSD, &r.TaskCount, &r.Error); err != nil {
		return Run{}, err
	}
	r.StartedAt = parseTime(startedStr)
	r.EndedAt = parseTime(endedStr)

	rows, err := s.db.Query(`
		SELECT name, status, attempt,
		       COALESCE(started_at, ''), COALESCE(ended_at, ''),
		       COALESCE(duration_ms, 0), exit_code, cost_usd, error, kind
		FROM tasks WHERE run_id = ?
		ORDER BY COALESCE(started_at, '') ASC, name ASC`, runID)
	if err != nil {
		return r, fmt.Errorf("GetRun: tasks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			t        Task
			ts       string
			te       string
			exitCode sql.NullInt64
		)
		if err := rows.Scan(&t.Name, &t.Status, &t.Attempt, &ts, &te, &t.DurationMs,
			&exitCode, &t.CostUSD, &t.Error, &t.Kind); err != nil {
			return r, err
		}
		t.StartedAt = parseTime(ts)
		t.EndedAt = parseTime(te)
		if exitCode.Valid {
			ec := int(exitCode.Int64)
			t.ExitCode = &ec
		}
		r.Tasks = append(r.Tasks, t)
	}
	return r, rows.Err()
}

// Worker is the API-facing shape of a row in the workers table. Status
// is computed at read time from last_seen — workers don't run a janitor
// to flip the column; "stale" means "last_seen older than the staleness
// threshold." Threshold defaults to 2 minutes (worker default heartbeat
// is ~100s; one missed cycle is fine, two is concerning).
type Worker struct {
	WorkerID   string    `json:"worker_id"`
	Host       string    `json:"host,omitempty"`
	Status     string    `json:"status"` // active | idle | stale
	LastSeen   time.Time `json:"last_seen,omitzero"`
	CurrentRun string    `json:"current_run,omitempty"`
	ClaimedAt  time.Time `json:"claimed_at,omitzero"`
	RunsTotal  int       `json:"runs_total"`
	RunsFailed int       `json:"runs_failed"`
}

// ListWorkers returns the worker registry sorted by last_seen DESC.
// Status is derived: a worker with current_run set AND last_seen within
// 2 minutes is "active"; current_run empty is "idle"; last_seen older
// than 2 minutes is "stale" regardless of current_run.
func (s *Store) ListWorkers() ([]Worker, error) {
	rows, err := s.db.Query(`
		SELECT worker_id, host, last_seen, current_run,
		       COALESCE(claimed_at, ''), runs_total, runs_failed
		FROM workers
		ORDER BY last_seen DESC`)
	if err != nil {
		return nil, fmt.Errorf("ListWorkers: %w", err)
	}
	defer rows.Close()

	staleAfter := 2 * time.Minute
	now := time.Now()
	var out []Worker
	for rows.Next() {
		var (
			w           Worker
			lastSeenStr string
			claimedStr  string
		)
		if err := rows.Scan(&w.WorkerID, &w.Host, &lastSeenStr, &w.CurrentRun,
			&claimedStr, &w.RunsTotal, &w.RunsFailed); err != nil {
			return nil, err
		}
		w.LastSeen = parseTime(lastSeenStr)
		w.ClaimedAt = parseTime(claimedStr)
		switch {
		case !w.LastSeen.IsZero() && now.Sub(w.LastSeen) > staleAfter:
			w.Status = "stale"
		case w.CurrentRun != "":
			w.Status = "active"
		default:
			w.Status = "idle"
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// UpsertWorker is called by the SSE control-channel handler when a
// worker connects. Records the host (the SSE conn knows the worker_id
// and we let the worker self-declare host via the registration body).
// Without this, workers that long-poll only (don't subscribe to control)
// still register via Claim, but workers running in idle mode never
// would.
func (s *Store) UpsertWorker(workerID, host string) error {
	if workerID == "" {
		return errors.New("UpsertWorker: empty worker_id")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`
		INSERT INTO workers(worker_id, host, last_seen)
		VALUES (?, ?, ?)
		ON CONFLICT(worker_id) DO UPDATE SET
		    host = CASE WHEN excluded.host != '' THEN excluded.host ELSE workers.host END,
		    last_seen = excluded.last_seen`,
		workerID, host, now,
	)
	return err
}

// CountRuns is a small helper used in tests / smoke checks.
func (s *Store) CountRuns() (int, error) {
	var n int
	row := s.db.QueryRow(`SELECT COUNT(*) FROM runs`)
	err := row.Scan(&n)
	return n, err
}

// EventRow is the wire shape of one row from the events table — returned
// by EventsSince for SSE replay. Payload is the raw JSON line the event
// was decoded from, so consumers see exactly what slog wrote.
//
// Seq + Lifetime are the SSE de-dup key (schema v4). Both nullable: rows
// inserted before v4 (or by programmatic Apply callers that bypass the
// slog chain) have Seq=0 and Lifetime="". Clients dedup on (lifetime,
// seq); zero values are unambiguous "not tagged" markers.
type EventRow struct {
	ID        int64     `json:"id"`
	RunID     string    `json:"run_id"`
	Task      string    `json:"task,omitempty"`
	EntryType string    `json:"entry_type"`
	Ts        time.Time `json:"ts,omitzero"`
	Payload   string    `json:"payload"` // raw JSON line, opaque to caller
	Seq       int64     `json:"seq,omitempty"`
	Lifetime  string    `json:"lifetime,omitempty"`
}

// EventsSince returns every event for runID with id > sinceID, ordered
// oldest-first. Pass sinceID=0 for the full history. Used by SSE to
// replay history on first connect and to catch up after reconnect.
//
// NOTE: sinceID is the row's autoincrement id, not the seq attr. Prefer
// EventsResume for clients with Last-Event-ID — that respects the
// (lifetime, seq) de-dup key and stays correct across producer restarts.
func (s *Store) EventsSince(runID string, sinceID int64) ([]EventRow, error) {
	rows, err := s.db.Query(`
		SELECT id, run_id, COALESCE(task,''), entry_type, ts, payload,
		       COALESCE(seq, 0), COALESCE(lifetime, '')
		FROM events
		WHERE run_id = ? AND id > ?
		ORDER BY id ASC`, runID, sinceID)
	if err != nil {
		return nil, fmt.Errorf("EventsSince: %w", err)
	}
	defer rows.Close()
	out := make([]EventRow, 0, 16)
	for rows.Next() {
		var (
			r  EventRow
			ts string
		)
		if err := rows.Scan(&r.ID, &r.RunID, &r.Task, &r.EntryType, &ts, &r.Payload, &r.Seq, &r.Lifetime); err != nil {
			return nil, err
		}
		r.Ts = parseTime(ts)
		out = append(out, r)
	}
	return out, rows.Err()
}

// EventsResume is the Last-Event-ID-aware history replay. clientLifetime
// + clientSeq come from the SSE id format `<lifetime>-<seq>`.
//
// Returns oldest-first:
//
//   - When lifetime matches a row's: skip rows with seq <= clientSeq
//     (the client already has them) and include the rest.
//   - When lifetime does not match: include the row (client hasn't seen
//     this process's events). Covers two cases:
//     a) Producer restarted mid-run; client connects with the old
//     lifetime; we replay all events from the new lifetime plus
//     the tail of the old one.
//     b) Client passes a junk/stale Last-Event-ID (e.g. the run got
//     relocated to a different producer); replay everything for
//     the run rather than silently dropping events.
//
// Pass clientLifetime="" + clientSeq=0 for a fresh history dump (the
// initial-connect path).
func (s *Store) EventsResume(runID, clientLifetime string, clientSeq int64) ([]EventRow, error) {
	// SQL filter encodes the (lifetime ≠ client) OR (lifetime = client AND seq > clientSeq)
	// rule. NULL lifetime sorts as ≠ client when client provided one — exactly what we
	// want for pre-v4 rows that predate de-dup support.
	q := `
		SELECT id, run_id, COALESCE(task,''), entry_type, ts, payload,
		       COALESCE(seq, 0), COALESCE(lifetime, '')
		FROM events
		WHERE run_id = ?
		  AND (
		    COALESCE(lifetime, '') != ?
		    OR COALESCE(seq, 0) > ?
		  )
		ORDER BY id ASC`
	rows, err := s.db.Query(q, runID, clientLifetime, clientSeq)
	if err != nil {
		return nil, fmt.Errorf("EventsResume: %w", err)
	}
	defer rows.Close()
	out := make([]EventRow, 0, 16)
	for rows.Next() {
		var (
			r  EventRow
			ts string
		)
		if err := rows.Scan(&r.ID, &r.RunID, &r.Task, &r.EntryType, &ts, &r.Payload, &r.Seq, &r.Lifetime); err != nil {
			return nil, err
		}
		r.Ts = parseTime(ts)
		out = append(out, r)
	}
	return out, rows.Err()
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
