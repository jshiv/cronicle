package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

// JobStatus values mirror the jobs.status column.
const (
	JobPending  = "pending"
	JobClaimed  = "claimed"
	JobDone     = "done"
	JobFailedQ  = "failed" // suffixed to avoid collision with task StatusFailed
	JobCanceled = "canceled"
)

// Job is the wire shape returned to a worker on long-poll. The Payload
// is the serialized Schedule JSON the producer pushed; the worker
// json.Unmarshals it into a Schedule and runs ExecuteTasks identically
// to how the in-process ConsumeSchedule path always has.
type Job struct {
	RunID         string    `json:"run_id"`
	Schedule      string    `json:"schedule"`
	Payload       []byte    `json:"payload"` // base64-encoded JSON
	EnqueuedAt    time.Time `json:"enqueued_at"`
	Attempt       int       `json:"attempt"`
	ClaimedBy     string    `json:"claimed_by,omitempty"`
	ClaimExpires  time.Time `json:"claim_expires_at,omitzero"`
}

// ErrNoJobs is returned by Claim when no job is available to claim.
// Callers (long-poll handler) should sleep on the wakeup chan and try
// again rather than treating it as an error condition.
var ErrNoJobs = errors.New("state: no jobs available")

// jobWaiters synchronizes long-poll waiters with enqueue/visibility
// reapers. Producers signal on enqueue; reapers signal when they move
// expired claims back to pending.
//
// Implementation: a "broadcast channel" — a chan struct{} that
// signal() closes and replaces. Waiters read the current channel
// snapshot, then block on it; closing the chan wakes every waiter
// holding a reference simultaneously. The next signal allocates a
// fresh channel.
//
// Why not sync.Cond: Cond.Wait has no timeout / ctx awareness, so the
// previous code wrapped it in a spawned goroutine + select { done /
// timer.C / ctx.Done }. That had a goroutine-leak race: if signal()
// fired before the spawned goroutine had acquired the mutex and
// entered Wait, the broadcast was lost and the goroutine blocked
// indefinitely (until the next unrelated signal). Channel-close
// broadcast doesn't have this race — there's no "wait inside lock"
// step that can be missed.
type jobWaiters struct {
	mu sync.Mutex
	ch chan struct{}
}

func (s *Store) waiters() *jobWaiters {
	s.waitOnce.Do(func() {
		s.wait = &jobWaiters{ch: make(chan struct{})}
	})
	return s.wait
}

// signal wakes every waiter currently blocked in WaitForJob, then
// reallocates the channel for the next round. Holding the mutex
// while we swap the chan ensures concurrent signal()s don't
// double-close or lose wakeups.
func (s *Store) signalWaiters() {
	w := s.waiters()
	w.mu.Lock()
	close(w.ch)
	w.ch = make(chan struct{})
	w.mu.Unlock()
}

// currentChan returns the channel each WaitForJob caller blocks on.
// Captured under lock so a concurrent signal() can't swap the chan
// out from under us mid-read — we either get the current chan (and
// its close on next signal wakes us) or the next chan (and its close
// on the next-next signal wakes us). Either way no wakeup is lost.
func (w *jobWaiters) currentChan() chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ch
}

// Enqueue persists a job for a future Claim. Idempotent on duplicate
// run_id — a re-enqueue with the same id is a no-op (workers are the
// authoritative source on whether a job needs to run again).
//
// schedule is duplicated outside the payload so the index can answer
// "any pending jobs?" without parsing JSON. It's also useful for
// future per-schedule queue stats.
func (s *Store) Enqueue(runID, schedule string, payload []byte) error {
	if runID == "" {
		return errors.New("Enqueue: empty run_id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`
		INSERT INTO jobs(run_id, schedule, payload, status, enqueued_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (run_id) DO NOTHING`,
		runID, schedule, string(payload), JobPending, now,
	)
	if err != nil {
		return fmt.Errorf("Enqueue: %w", err)
	}
	// Wake every waiter; they'll race for the SQL claim and at most one
	// wins. Broadcast-style wakeup is correct here — extra spurious
	// wakeups for losing workers are cheaper than tracking per-worker
	// state, and at ≤100 workers the wasted Claim calls are negligible.
	s.signalWaiters()
	return nil
}

// Claim attempts to atomically move one pending row to claimed=workerID
// and return it. Returns ErrNoJobs if none are pending. The transaction
// uses BEGIN IMMEDIATE so the write lock is acquired up front, avoiding
// SQLITE_BUSY upgrades mid-statement.
//
// visibility is the duration after which the claim expires if the worker
// neither acks nor heartbeats. The reaper periodically moves expired
// claims back to pending.
func (s *Store) Claim(workerID string, visibility time.Duration) (Job, error) {
	if workerID == "" {
		return Job{}, errors.New("Claim: empty worker_id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	expires := now.Add(visibility)

	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return Job{}, fmt.Errorf("Claim: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Pick the oldest pending row. If two workers race here, BEGIN
	// IMMEDIATE has already serialized them — only one is in this txn
	// at a time. The UPDATE below is conditional on status='pending'
	// so even if the schema-level lock failed (it doesn't, but),
	// double-claim is impossible.
	row := tx.QueryRow(`
		SELECT run_id, schedule, payload, enqueued_at, attempt
		FROM jobs
		WHERE status = ?
		ORDER BY enqueued_at ASC
		LIMIT 1`, JobPending)
	var (
		j          Job
		enqueuedAt string
	)
	if err := row.Scan(&j.RunID, &j.Schedule, &j.Payload, &enqueuedAt, &j.Attempt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, ErrNoJobs
		}
		return Job{}, fmt.Errorf("Claim: select: %w", err)
	}
	j.EnqueuedAt, _ = time.Parse(time.RFC3339Nano, enqueuedAt)

	res, err := tx.Exec(`
		UPDATE jobs SET
		    status = ?,
		    claimed_by = ?,
		    claimed_at = ?,
		    claim_expires_at = ?,
		    attempt = attempt + 1
		WHERE run_id = ? AND status = ?`,
		JobClaimed, workerID, now.Format(time.RFC3339Nano), expires.Format(time.RFC3339Nano),
		j.RunID, JobPending,
	)
	if err != nil {
		return Job{}, fmt.Errorf("Claim: update: %w", err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		// Someone else claimed it between our SELECT and UPDATE — extremely
		// rare with BEGIN IMMEDIATE but theoretically possible. Treat as
		// "no job for us" and let the caller retry.
		return Job{}, ErrNoJobs
	}

	// Worker registry upsert: stamp last_seen + current_run + runs_total.
	// Same transaction as the claim so the registry is always consistent
	// with the queue.
	if _, err := tx.Exec(`
		INSERT INTO workers(worker_id, last_seen, current_run, claimed_at, runs_total)
		VALUES (?, ?, ?, ?, 1)
		ON CONFLICT(worker_id) DO UPDATE SET
		    last_seen = excluded.last_seen,
		    current_run = excluded.current_run,
		    claimed_at = excluded.claimed_at,
		    runs_total = workers.runs_total + 1`,
		workerID, now.Format(time.RFC3339Nano), j.RunID, now.Format(time.RFC3339Nano),
	); err != nil {
		return Job{}, fmt.Errorf("Claim: worker upsert: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Job{}, fmt.Errorf("Claim: commit: %w", err)
	}
	j.ClaimedBy = workerID
	j.ClaimExpires = expires
	j.Attempt++ // reflect the post-UPDATE value to the caller
	return j, nil
}

// Ack marks a job done (success=true) or failed (success=false, errMsg
// recorded). Called by workers after the run completes (or by the
// in-process consumer in --queue self mode). Idempotent.
func (s *Store) Ack(runID, workerID string, success bool, errMsg string) error {
	if runID == "" {
		return errors.New("Ack: empty run_id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	status := JobDone
	if !success {
		status = JobFailedQ
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// Only ack rows we own. If claim expired and was reassigned, the
	// reaper has already cleared claimed_by; this UPDATE then matches
	// nothing and we return without error — the new owner's ack will
	// land instead.
	res, err := s.db.Exec(`
		UPDATE jobs SET
		    status = ?,
		    completed_at = ?,
		    last_error = ?,
		    claim_expires_at = NULL
		WHERE run_id = ? AND claimed_by = ? AND status = ?`,
		status, now, errMsg, runID, workerID, JobClaimed,
	)
	if err != nil {
		return fmt.Errorf("Ack: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		// Worker registry: clear current_run, bump failed counter on
		// failure ack.
		failedInc := 0
		if !success {
			failedInc = 1
		}
		_, _ = s.db.Exec(`
			UPDATE workers SET
			    last_seen = ?,
			    current_run = '',
			    runs_failed = runs_failed + ?
			WHERE worker_id = ?`,
			now, failedInc, workerID,
		)
	}
	return nil
}

// Heartbeat extends the visibility timeout on a claimed job. Workers
// running long agent tasks should call this periodically (every
// visibility/3 is a safe cadence) so the reaper doesn't preempt them.
func (s *Store) Heartbeat(runID, workerID string, visibility time.Duration) error {
	if runID == "" || workerID == "" {
		return errors.New("Heartbeat: empty run_id or worker_id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	expires := now.Add(visibility).Format(time.RFC3339Nano)
	res, err := s.db.Exec(`
		UPDATE jobs SET claim_expires_at = ?
		WHERE run_id = ? AND claimed_by = ? AND status = ?`,
		expires, runID, workerID, JobClaimed,
	)
	if err != nil {
		return fmt.Errorf("Heartbeat: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Either the job already completed, or the claim expired and
		// was reaped. Caller should stop heartbeating and check the
		// run's status via /v1/runs/{id} if it still cares.
		return errors.New("Heartbeat: no matching claimed job (lost ownership?)")
	}
	// Refresh worker registry's last_seen — the heartbeat IS the
	// liveness signal we need on the producer side.
	_, _ = s.db.Exec(`UPDATE workers SET last_seen = ? WHERE worker_id = ?`,
		now.Format(time.RFC3339Nano), workerID,
	)
	return nil
}

// ReapExpired moves rows whose claims have lapsed back to pending so
// another worker can claim them. Called by a janitor goroutine on a
// fixed cadence (every 10s is plenty given typical job durations).
//
// Returns the number of rows moved, useful for /healthz to surface
// "workers are dying mid-job" via slow-burning-fire metrics.
func (s *Store) ReapExpired() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := s.db.Exec(`
		UPDATE jobs SET
		    status = ?,
		    claimed_by = '',
		    claimed_at = NULL,
		    claim_expires_at = NULL
		WHERE status = ? AND claim_expires_at IS NOT NULL AND claim_expires_at < ?`,
		JobPending, JobClaimed, now,
	)
	if err != nil {
		return 0, fmt.Errorf("ReapExpired: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		// Wake any blocked long-polls so the freed jobs get claimed
		// promptly rather than waiting for the next worker reconnect.
		s.signalWaiters()
	}
	return int(n), nil
}

// WaitForJob blocks until either a job is available (signaled by
// Enqueue or ReapExpired), the deadline elapses, or ctx is canceled.
// Returns true if a job is plausibly available (caller should retry
// Claim), false if the wait timed out. False positives are fine — the
// caller's next Claim will return ErrNoJobs and the long-poll will
// return 204.
//
// The previous sync.Cond-based implementation wrapped Cond.Wait in a
// spawned goroutine to add timeout / ctx awareness. That had a
// goroutine-leak race: if Broadcast fired before the goroutine had
// acquired the cond mutex, the wakeup was lost and the goroutine
// blocked indefinitely (until an unrelated future signal). Under idle
// load that's a permanent leak per WaitForJob call.
//
// The chan-close broadcast pattern (see jobWaiters / signalWaiters)
// is naturally ctx-aware and has no "wait inside lock" step that can
// be missed. No goroutine spawn either.
func (s *Store) WaitForJob(ctx context.Context, timeout time.Duration) bool {
	ch := s.waiters().currentChan()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// CountJobsByStatus is a small helper for tests, /healthz, and
// dashboards. Not exposed via API yet — projection runs already give a
// rich enough view; if dashboards ever ask, we'll add /v1/queue.
func (s *Store) CountJobsByStatus(status string) (int, error) {
	row := s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status = ?`, status)
	var n int
	err := row.Scan(&n)
	return n, err
}
