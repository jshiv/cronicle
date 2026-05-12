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

// RetryResult is the outcome of state.Retry / state.Resume: the new
// run_id assigned to the re-enqueued job, the schedule name, and (for
// Resume) the list of tasks that were skipped because they had already
// succeeded in the original run.
type RetryResult struct {
	OriginalRunID string   `json:"original_run_id"`
	NewRunID      string   `json:"new_run_id"`
	Schedule      string   `json:"schedule"`
	SkippedTasks  []string `json:"skipped_tasks,omitempty"`
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

// Resume re-enqueues a terminal run with only the tasks that did NOT
// succeed in the original. The common operator workflow this serves:
//
//  1. A scheduled run lights up; some early tasks succeed.
//  2. A later task gets stuck / runs away / emits bad output.
//  3. Operator hits POST /v1/runs/{id}/cancel — the canceled status is
//     sticky in the projection, including per-task succeed/fail/cancel.
//  4. Operator investigates, fixes whatever was wrong.
//  5. Operator hits POST /v1/runs/{id}/resume — the new run skips the
//     tasks that already succeeded and re-runs the rest.
//
// Implementation detail: depends pointing at skipped (succeeded)
// tasks are stripped from the remaining tasks, so a task whose only
// upstream had already succeeded becomes a fresh DAG entry point.
// This matches the cancel/resume semantics most operators expect from
// CI pipeline restarts ("re-run failed jobs only").
//
// Like Retry, the original run row is preserved as audit trail and a
// fresh run_id is minted for the resumed work.
func (s *Store) Resume(runID, newRunID string) (RetryResult, error) {
	if runID == "" || newRunID == "" {
		return RetryResult{}, errors.New("Resume: empty run_id or new_run_id")
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
			return RetryResult{}, fmt.Errorf("Resume: run %q not in queue", runID)
		}
		return RetryResult{}, fmt.Errorf("Resume: read job: %w", err)
	}
	if status == JobPending || status == JobClaimed {
		return RetryResult{}, fmt.Errorf("Resume: run %q is still in flight (status=%s)", runID, status)
	}

	// Walk the original run's per-task status to identify which tasks
	// to skip. We read tasks via a direct query rather than the
	// public GetRun helper to avoid the mutex-reentry that would cause
	// (s.mu is held here; GetRun doesn't acquire it, but we keep
	// reads local for clarity).
	rows, err := s.db.Query(`SELECT name, status FROM tasks WHERE run_id = ?`, runID)
	if err != nil {
		return RetryResult{}, fmt.Errorf("Resume: read tasks: %w", err)
	}
	skipped := make(map[string]bool)
	var skippedList []string
	for rows.Next() {
		var name, taskStatus string
		if err := rows.Scan(&name, &taskStatus); err != nil {
			rows.Close()
			return RetryResult{}, fmt.Errorf("Resume: scan task: %w", err)
		}
		if taskStatus == StatusSucceeded {
			skipped[name] = true
			skippedList = append(skippedList, name)
		}
	}
	rows.Close()

	// Patch the payload: filter Tasks, strip stale depends.
	newPayload, err := filterScheduleTasks([]byte(payload), skipped, newRunID)
	if err != nil {
		return RetryResult{}, err
	}

	// If every task was already succeeded, there's nothing to do —
	// surface a 400-friendly error so the operator notices.
	if jsonHasEmptyTasks(newPayload) {
		return RetryResult{}, fmt.Errorf("Resume: every task in run %q already succeeded; nothing to resume", runID)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`
		INSERT OR IGNORE INTO jobs(run_id, schedule, payload, status, enqueued_at)
		VALUES (?, ?, ?, ?, ?)`,
		newRunID, schedule, string(newPayload), JobPending, now,
	); err != nil {
		return RetryResult{}, fmt.Errorf("Resume: enqueue: %w", err)
	}
	w := s.waiters()
	w.mu.Lock()
	w.cond.Broadcast()
	w.mu.Unlock()

	return RetryResult{
		OriginalRunID: runID,
		NewRunID:      newRunID,
		Schedule:      schedule,
		SkippedTasks:  skippedList,
	}, nil
}

// filterScheduleTasks decodes a serialized Schedule payload, drops the
// tasks whose names appear in skip, also drops references to skipped
// tasks from the remaining tasks' Depends, and patches the top-level
// RunID. Kept here (vs. cronicle/config.go) so the state package owns
// the queue-payload mutation logic end-to-end.
func filterScheduleTasks(payload []byte, skip map[string]bool, newRunID string) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("filterScheduleTasks: decode: %w", err)
	}
	raw["RunID"] = newRunID

	tasksAny, _ := raw["Tasks"].([]any)
	kept := make([]any, 0, len(tasksAny))
	for _, t := range tasksAny {
		tm, ok := t.(map[string]any)
		if !ok {
			kept = append(kept, t)
			continue
		}
		name, _ := tm["Name"].(string)
		if skip[name] {
			continue
		}
		// Strip stale depends pointing at filtered-out tasks.
		if deps, ok := tm["Depends"].([]any); ok {
			filtered := make([]any, 0, len(deps))
			for _, d := range deps {
				if s, ok := d.(string); ok && skip[s] {
					continue
				}
				filtered = append(filtered, d)
			}
			if len(filtered) == 0 {
				tm["Depends"] = nil
			} else {
				tm["Depends"] = filtered
			}
		}
		kept = append(kept, tm)
	}
	raw["Tasks"] = kept

	out, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("filterScheduleTasks: encode: %w", err)
	}
	return out, nil
}

// jsonHasEmptyTasks returns true when the payload's Tasks array is
// empty/null. Used by Resume to detect "nothing left to run."
func jsonHasEmptyTasks(payload []byte) bool {
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return false
	}
	tasks, _ := raw["Tasks"].([]any)
	return len(tasks) == 0
}

// ---- Control-channel registry ----------------------------------------------
//
// Workers subscribe to GET /v1/workers/{id}/control as an SSE stream.
// The store keeps an in-memory map[worker_id]chan ControlMsg. Cancel
// looks up the worker holding the claim and pushes a message; if no
// active subscription exists the cancel still succeeds in the database
// (heartbeat-based detection picks it up next cycle).

// ControlMsg is the wire shape of the SSE control stream. Type is
// "cancel", "cancel_task", or "ping" today; future verbs (pause, drain)
// layer in here. Task is populated only for "cancel_task" messages —
// a hint to the worker to preempt just the named task; the load-bearing
// path remains the projection's sticky cancel + walker gate.
type ControlMsg struct {
	Type  string `json:"type"`
	RunID string `json:"run_id,omitempty"`
	Task  string `json:"task,omitempty"`
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
