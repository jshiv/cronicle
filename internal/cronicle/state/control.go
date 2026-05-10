package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrNotCancelable is returned when Cancel is called on a run that's
// already in a terminal state (succeeded / failed / canceled).
var ErrNotCancelable = errors.New("state: run not cancelable (already terminal)")

// CancelResult describes what state.Cancel actually changed.
type CancelResult struct {
	RunID        string `json:"run_id"`
	WorkerID     string `json:"worker_id,omitempty"`     // who held the claim, if any
	WasClaimed   bool   `json:"was_claimed"`             // job was in flight
	WasPending   bool   `json:"was_pending"`             // job was queued but not yet claimed
	WasUnknown   bool   `json:"was_unknown,omitempty"`   // run only exists in projection (e.g., never enqueued via --queue self)
	Status       string `json:"status"`                  // post-cancel status
}

// Cancel marks a run as canceled in both the queue (jobs row) and the
// projection (runs/tasks rows). Returns the worker_id holding the
// claim (if any) so the caller can route an SSE control message to
// them; the worker is responsible for actually preempting its in-flight
// execute via ctx.Cancel().
//
// Transitions allowed:
//
//	pending  → canceled (queue row + run/tasks rows)
//	claimed  → canceled (queue row + run/tasks rows; SSE signal to worker)
//	(no jobs row, only projection runs row in 'running'): mark canceled
//	  in projection only — useful for in-process / default-queue mode
//
// Terminal states are rejected with ErrNotCancelable.
func (s *Store) Cancel(runID string) (CancelResult, error) {
	if runID == "" {
		return CancelResult{}, errors.New("Cancel: empty run_id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return CancelResult{}, fmt.Errorf("Cancel: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res := CancelResult{RunID: runID, Status: StatusCanceled}

	// Queue row inspection
	var (
		jobStatus    sql.NullString
		jobClaimedBy sql.NullString
	)
	err = tx.QueryRow(`SELECT status, claimed_by FROM jobs WHERE run_id = ?`, runID).
		Scan(&jobStatus, &jobClaimedBy)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// No queue row — either pre-Phase-2b setup, or the job was
		// dispatched in the legacy chan-based mode. We can still mark
		// the projection row canceled.
		res.WasUnknown = true
	case err != nil:
		return CancelResult{}, fmt.Errorf("Cancel: read job: %w", err)
	case jobStatus.String == JobPending:
		res.WasPending = true
		if _, err := tx.Exec(`
			UPDATE jobs SET status=?, completed_at=?, last_error='canceled'
			WHERE run_id = ? AND status = ?`,
			JobCanceled, now, runID, JobPending,
		); err != nil {
			return CancelResult{}, fmt.Errorf("Cancel: pending update: %w", err)
		}
	case jobStatus.String == JobClaimed:
		res.WasClaimed = true
		res.WorkerID = jobClaimedBy.String
		if _, err := tx.Exec(`
			UPDATE jobs SET status=?, completed_at=?, last_error='canceled', claim_expires_at=NULL
			WHERE run_id = ? AND status = ?`,
			JobCanceled, now, runID, JobClaimed,
		); err != nil {
			return CancelResult{}, fmt.Errorf("Cancel: claimed update: %w", err)
		}
	default:
		// done / failed / canceled — terminal. The Heartbeat handler
		// will already be returning 409 to any worker still holding it.
		return CancelResult{}, ErrNotCancelable
	}

	// Projection update — runs row + any non-terminal task rows.
	if _, err := tx.Exec(`
		UPDATE runs SET status = ?, ended_at = ?, error = COALESCE(NULLIF(error,''),'canceled')
		WHERE run_id = ? AND status NOT IN (?, ?, ?)`,
		StatusCanceled, now, runID, StatusSucceeded, StatusFailed, StatusCanceled,
	); err != nil {
		return CancelResult{}, fmt.Errorf("Cancel: runs update: %w", err)
	}
	if _, err := tx.Exec(`
		UPDATE tasks SET status = ?, ended_at = ?
		WHERE run_id = ? AND status NOT IN (?, ?, ?)`,
		StatusCanceled, now, runID, StatusSucceeded, StatusFailed, StatusCanceled,
	); err != nil {
		return CancelResult{}, fmt.Errorf("Cancel: tasks update: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return CancelResult{}, fmt.Errorf("Cancel: commit: %w", err)
	}
	return res, nil
}

// RetryResult is the outcome of state.Retry: the new run_id assigned
// to the re-enqueued job, and a copy of the schedule name for the
// caller's convenience.
type RetryResult struct {
	OriginalRunID string `json:"original_run_id"`
	NewRunID      string `json:"new_run_id"`
	Schedule      string `json:"schedule"`
}

// Retry re-enqueues the schedule that was originally fired as runID.
// Loads the payload from the jobs row (which still holds it after
// completion), assigns a fresh run_id (caller-provided so the producer
// controls ID format), patches it into the payload, and Enqueues.
//
// Only terminal runs can be retried — refusing to re-fire a job that's
// still in flight prevents duplicate execution. This is operator-
// initiated, not automatic; failed runs do NOT re-fire on their own.
func (s *Store) Retry(runID, newRunID string) (RetryResult, error) {
	if runID == "" || newRunID == "" {
		return RetryResult{}, errors.New("Retry: empty run_id or new_run_id")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var (
		schedule string
		payload  string
		status   string
	)
	err := s.db.QueryRow(`SELECT schedule, payload, status FROM jobs WHERE run_id = ?`, runID).
		Scan(&schedule, &payload, &status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RetryResult{}, fmt.Errorf("Retry: run %q not in queue (cannot retry runs that never went through --queue self)", runID)
		}
		return RetryResult{}, fmt.Errorf("Retry: read job: %w", err)
	}
	if status == JobPending || status == JobClaimed {
		return RetryResult{}, fmt.Errorf("Retry: run %q is still in flight (status=%s)", runID, status)
	}

	// Patch the payload so it carries the new run_id. The schedule
	// JSON has top-level RunID; we json-decode + re-encode rather than
	// regex-replacing, to keep the format authoritative.
	var raw map[string]any
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return RetryResult{}, fmt.Errorf("Retry: bad stored payload: %w", err)
	}
	raw["RunID"] = newRunID
	newPayload, err := json.Marshal(raw)
	if err != nil {
		return RetryResult{}, fmt.Errorf("Retry: marshal new payload: %w", err)
	}

	// Manual enqueue (we already hold s.mu). The Enqueue method
	// re-acquires s.mu, so re-implement the INSERT here.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`
		INSERT OR IGNORE INTO jobs(run_id, schedule, payload, status, enqueued_at)
		VALUES (?, ?, ?, ?, ?)`,
		newRunID, schedule, string(newPayload), JobPending, now,
	); err != nil {
		return RetryResult{}, fmt.Errorf("Retry: enqueue: %w", err)
	}
	// Wake any blocked long-poll waiters.
	w := s.waiters()
	w.mu.Lock()
	w.cond.Broadcast()
	w.mu.Unlock()

	return RetryResult{
		OriginalRunID: runID,
		NewRunID:      newRunID,
		Schedule:      schedule,
	}, nil
}

// ---- Control-channel registry ----------------------------------------------
//
// Workers subscribe to GET /v1/workers/{id}/control as an SSE stream.
// The store keeps an in-memory map[worker_id]chan ControlMsg. Cancel
// looks up the worker holding the claim and pushes a message; if no
// active subscription exists the cancel still succeeds in the database
// (heartbeat-based detection picks it up next cycle).

// ControlMsg is the wire shape of the SSE control stream. Type is
// "cancel" or "ping" today; future verbs (pause, drain) layer in here.
type ControlMsg struct {
	Type  string `json:"type"`
	RunID string `json:"run_id,omitempty"`
}

// controlRegistry is process-local — reset on producer restart, which
// matters because the SSE conns are torn down on restart anyway. The
// chan capacity gives a worker a small grace window to be slow without
// blocking Cancel; we drop oldest if a worker is stuck.
type controlRegistry struct {
	mu   sync.Mutex
	subs map[string]chan ControlMsg
}

func (s *Store) controlReg() *controlRegistry {
	s.controlOnce.Do(func() {
		s.controlReg2 = &controlRegistry{subs: make(map[string]chan ControlMsg)}
	})
	return s.controlReg2
}

// Subscribe registers a worker for control messages and returns the
// receive channel + an unsubscribe func the caller MUST defer to clean
// up. If the worker already has a subscription (reconnected without
// the previous one closing), the old channel is closed so the previous
// goroutine exits and the new one takes over.
func (s *Store) Subscribe(workerID string) (<-chan ControlMsg, func()) {
	reg := s.controlReg()
	reg.mu.Lock()
	defer reg.mu.Unlock()

	if existing, ok := reg.subs[workerID]; ok {
		close(existing)
		delete(reg.subs, workerID)
	}
	ch := make(chan ControlMsg, 8)
	reg.subs[workerID] = ch
	return ch, func() {
		reg.mu.Lock()
		defer reg.mu.Unlock()
		if cur, ok := reg.subs[workerID]; ok && cur == ch {
			close(ch)
			delete(reg.subs, workerID)
		}
	}
}

// PushControl sends a message to a specific worker. Drops if the
// worker isn't subscribed (heartbeat-based detection is the fallback).
// Drops if the worker's buffer is full — operator likely retries.
func (s *Store) PushControl(workerID string, msg ControlMsg) bool {
	reg := s.controlReg()
	reg.mu.Lock()
	ch, ok := reg.subs[workerID]
	reg.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- msg:
		return true
	default:
		return false
	}
}
