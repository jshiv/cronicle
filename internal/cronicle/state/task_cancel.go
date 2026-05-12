package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrTaskNotCancelable signals the task row is already terminal. The
// caller can treat this as a no-op (downstream tasks aren't affected
// because the task already completed) — distinct from the run-level
// ErrNotCancelable which prevents any change.
var ErrTaskNotCancelable = errors.New("state: task not cancelable (already terminal)")

// CancelTaskResult is what state.CancelTask reports after marking task
// rows canceled. CanceledTasks lists every task row that transitioned
// from a non-terminal state — the head task plus any cascaded dependents
// that hadn't started yet. WorkerID identifies the run's current claim
// holder so the caller can route an SSE control hint, even though the
// MVP cancel doesn't preempt in-flight tasks.
type CancelTaskResult struct {
	RunID          string   `json:"run_id"`
	WorkerID       string   `json:"worker_id,omitempty"`
	WasClaimed     bool     `json:"was_claimed"`
	CanceledTasks  []string `json:"canceled_tasks"`
	SkippedTerminal []string `json:"skipped_terminal,omitempty"` // already-completed tasks the cascade tried to touch
}

// CancelTask marks the named task and the cascade set (its transitive
// dependents in the same run, computed by the caller) as canceled. The
// run row itself is NOT touched — the rest of the DAG continues. If
// every remaining task ends up canceled or terminal, schedule_complete
// will still close the run normally; the runs.status sticky-cancel rule
// only fires when state.Cancel(runID) is called.
//
// In-flight preemption: the MVP does not interrupt a task whose body is
// currently executing — the per-task context plumbing isn't there yet.
// What the projection records is still accurate (sticky-cancel in
// applyTaskTerminal keeps the canceled status when the body's terminal
// event arrives), and downstream tasks are blocked by the walker's
// per-task cancel gate at launch time. Returning the WorkerID lets a
// future iteration route an SSE "cancel_task" hint without changing the
// API shape.
func (s *Store) CancelTask(runID, taskName string, cascade []string) (CancelTaskResult, error) {
	if runID == "" {
		return CancelTaskResult{}, errors.New("CancelTask: empty run_id")
	}
	if taskName == "" {
		return CancelTaskResult{}, errors.New("CancelTask: empty task name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return CancelTaskResult{}, fmt.Errorf("CancelTask: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res := CancelTaskResult{RunID: runID}

	// Worker lookup (best-effort hint).
	var (
		jobStatus    sql.NullString
		jobClaimedBy sql.NullString
	)
	err = tx.QueryRow(`SELECT status, claimed_by FROM jobs WHERE run_id = ?`, runID).
		Scan(&jobStatus, &jobClaimedBy)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// no queue row — single-node mode or pre-Phase-2b
	case err != nil:
		return CancelTaskResult{}, fmt.Errorf("CancelTask: read job: %w", err)
	case jobStatus.String == JobClaimed:
		res.WasClaimed = true
		res.WorkerID = jobClaimedBy.String
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	// Build the target set: head task + cascade, deduped, preserving
	// the operator's notion of "task" as the first entry.
	seen := map[string]bool{}
	targets := []string{}
	for _, name := range append([]string{taskName}, cascade...) {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		targets = append(targets, name)
	}

	// First check the head task: it must exist in the projection.
	// Cascade entries may or may not have rows yet (a queued-but-not-
	// started task has a row; a fan-out task that's never been touched
	// doesn't). The walker's gate handles the row-less case at launch
	// time, so we only require the head to be present.
	var headStatus string
	err = tx.QueryRow(`SELECT status FROM tasks WHERE run_id = ? AND name = ?`, runID, taskName).Scan(&headStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return CancelTaskResult{}, fmt.Errorf("CancelTask: task %q not found in run %q", taskName, runID)
	}
	if err != nil {
		return CancelTaskResult{}, fmt.Errorf("CancelTask: read head task: %w", err)
	}
	if headStatus == StatusSucceeded || headStatus == StatusFailed || headStatus == StatusCanceled {
		return CancelTaskResult{}, ErrTaskNotCancelable
	}

	// Mark the head + cascade canceled. For each target, INSERT-or-
	// UPDATE so a downstream task that doesn't yet have a row gets one
	// — the walker reads tasks.status to decide whether to launch, and
	// rows with status='canceled' will be skipped.
	for _, name := range targets {
		var existing sql.NullString
		err := tx.QueryRow(`SELECT status FROM tasks WHERE run_id = ? AND name = ?`, runID, name).Scan(&existing)
		if errors.Is(err, sql.ErrNoRows) {
			// Insert a fresh row in canceled state. The walker's
			// per-task gate will pick this up at launch time.
			if _, err := tx.Exec(`
				INSERT INTO tasks(run_id, name, status, started_at, ended_at, duration_ms)
				VALUES (?, ?, ?, ?, ?, 0)`,
				runID, name, StatusCanceled, now, now,
			); err != nil {
				return CancelTaskResult{}, fmt.Errorf("CancelTask: insert %q: %w", name, err)
			}
			res.CanceledTasks = append(res.CanceledTasks, name)
			continue
		}
		if err != nil {
			return CancelTaskResult{}, fmt.Errorf("CancelTask: read %q: %w", name, err)
		}
		if existing.String == StatusSucceeded || existing.String == StatusFailed || existing.String == StatusCanceled {
			// Terminal already — leave it alone (sticky-cancel rules
			// already preserve canceled; success/failure outranks our
			// retroactive cancel signal for already-completed work).
			res.SkippedTerminal = append(res.SkippedTerminal, name)
			continue
		}
		if _, err := tx.Exec(`
			UPDATE tasks SET status = ?, ended_at = ?
			WHERE run_id = ? AND name = ? AND status NOT IN (?, ?, ?)`,
			StatusCanceled, now, runID, name, StatusSucceeded, StatusFailed, StatusCanceled,
		); err != nil {
			return CancelTaskResult{}, fmt.Errorf("CancelTask: update %q: %w", name, err)
		}
		res.CanceledTasks = append(res.CanceledTasks, name)
	}

	if err := tx.Commit(); err != nil {
		return CancelTaskResult{}, fmt.Errorf("CancelTask: commit: %w", err)
	}
	return res, nil
}

// IsTaskCanceledInRun returns true if the projection has the (run, task)
// row in status='canceled'. Read-only — used by the DAG walker's
// per-task gate to skip canceled tasks at launch. Missing rows return
// false (the row hasn't been created yet, walker will let it run).
func (s *Store) IsTaskCanceledInRun(runID, taskName string) (bool, error) {
	if runID == "" || taskName == "" {
		return false, nil
	}
	var status string
	row := s.db.QueryRow(`SELECT status FROM tasks WHERE run_id = ? AND name = ?`, runID, taskName)
	if err := row.Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("state.IsTaskCanceledInRun: %w", err)
	}
	return status == StatusCanceled, nil
}
