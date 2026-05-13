package state

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// TaskState captures the runtime control row for a single (schedule,
// task) pair. A pair with no row is treated as active (not skipped).
type TaskState struct {
	Schedule  string
	Task      string
	Skipped   bool
	SkippedAt time.Time // zero when not skipped
	SkippedBy string    // free-form actor identifier
	Reason    string
	UpdatedAt time.Time
}

// SetTaskSkipped flags a task as skipped. Upsert semantics — an idempotent
// re-skip preserves the original skipped_at so "how long has this been
// disabled?" stays consistent. Empty schedule or task is rejected: the
// composite key requires both halves.
func (s *Store) SetTaskSkipped(schedule, task, actor, reason string) error {
	if schedule == "" || task == "" {
		return errors.New("state.SetTaskSkipped: schedule and task are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`
		INSERT INTO task_state(schedule, task, skipped, skipped_at, skipped_by, reason, updated_at)
		VALUES (?, ?, 1, ?, ?, ?, ?)
		ON CONFLICT(schedule, task) DO UPDATE SET
		    skipped = 1,
		    skipped_at = CASE WHEN task_state.skipped = 1 THEN task_state.skipped_at ELSE excluded.skipped_at END,
		    skipped_by = excluded.skipped_by,
		    reason = excluded.reason,
		    updated_at = excluded.updated_at
		`,
		schedule, task, now, actor, reason, now,
	)
	if err != nil {
		return fmt.Errorf("state.SetTaskSkipped: %w", err)
	}
	return nil
}

// ClearTaskSkipped removes the skip flag. Idempotent: a missing row or a
// row already at skipped=0 returns nil. skipped_at is cleared so the next
// skip starts a fresh epoch.
func (s *Store) ClearTaskSkipped(schedule, task, actor string) error {
	if schedule == "" || task == "" {
		return errors.New("state.ClearTaskSkipped: schedule and task are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`
		UPDATE task_state SET
		    skipped = 0,
		    skipped_at = '',
		    skipped_by = ?,
		    reason = '',
		    updated_at = ?
		WHERE schedule = ? AND task = ?`,
		actor, now, schedule, task,
	)
	if err != nil {
		return fmt.Errorf("state.ClearTaskSkipped: %w", err)
	}
	return nil
}

// IsTaskSkipped returns true when (schedule, task) has an active skip
// flag. Missing rows are treated as active. Empty schedule or task
// returns false rather than an error — the DAG walker calls this
// per-task on the hot path and shouldn't have to nil-check the
// arguments.
func (s *Store) IsTaskSkipped(schedule, task string) (bool, error) {
	if schedule == "" || task == "" {
		return false, nil
	}
	var skipped int
	row := s.db.QueryRow(`SELECT skipped FROM task_state WHERE schedule = ? AND task = ?`, schedule, task)
	if err := row.Scan(&skipped); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("state.IsTaskSkipped: %w", err)
	}
	return skipped == 1, nil
}

// GetTaskState returns the full control row for (schedule, task), or a
// zero-value TaskState with Schedule/Task populated when no row exists.
func (s *Store) GetTaskState(schedule, task string) (TaskState, error) {
	st := TaskState{Schedule: schedule, Task: task}
	if schedule == "" || task == "" {
		return st, nil
	}
	var (
		skipped              int
		skippedAt, updatedAt string
	)
	row := s.db.QueryRow(`
		SELECT skipped, skipped_at, skipped_by, reason, updated_at
		FROM task_state WHERE schedule = ? AND task = ?`,
		schedule, task)
	err := row.Scan(&skipped, &skippedAt, &st.SkippedBy, &st.Reason, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return st, nil
		}
		return st, fmt.Errorf("state.GetTaskState: %w", err)
	}
	st.Skipped = skipped == 1
	if skippedAt != "" {
		if t, perr := time.Parse(time.RFC3339Nano, skippedAt); perr == nil {
			st.SkippedAt = t
		}
	}
	if updatedAt != "" {
		if t, perr := time.Parse(time.RFC3339Nano, updatedAt); perr == nil {
			st.UpdatedAt = t
		}
	}
	return st, nil
}

// ListSkippedTasksForSchedule returns every (schedule, task) pair within a
// schedule that's currently flagged skipped. Empty result when nothing is
// skipped. Used by the listener's /v1/schedules/{name}/state response to
// surface the full skip set in one call.
func (s *Store) ListSkippedTasksForSchedule(schedule string) ([]TaskState, error) {
	if schedule == "" {
		return nil, nil
	}
	rows, err := s.db.Query(`
		SELECT task, skipped_at, skipped_by, reason, updated_at
		FROM task_state
		WHERE schedule = ? AND skipped = 1
		ORDER BY task ASC`, schedule)
	if err != nil {
		return nil, fmt.Errorf("state.ListSkippedTasksForSchedule: %w", err)
	}
	defer rows.Close()

	var out []TaskState
	for rows.Next() {
		st := TaskState{Schedule: schedule, Skipped: true}
		var skippedAt, updatedAt string
		if err := rows.Scan(&st.Task, &skippedAt, &st.SkippedBy, &st.Reason, &updatedAt); err != nil {
			return nil, fmt.Errorf("state.ListSkippedTasksForSchedule: scan: %w", err)
		}
		if t, perr := time.Parse(time.RFC3339Nano, skippedAt); perr == nil {
			st.SkippedAt = t
		}
		if t, perr := time.Parse(time.RFC3339Nano, updatedAt); perr == nil {
			st.UpdatedAt = t
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state.ListSkippedTasksForSchedule: iter: %w", err)
	}
	return out, nil
}
