package state

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RunState captures the in-flight control row for a single run. A run
// with no row is treated as active. Distinct from runs.status — that
// column is the lifecycle (queued/running/...); paused is a control
// overlay the DAG walker honors at each task-launch boundary.
type RunState struct {
	RunID     string
	Paused    bool
	PausedAt  time.Time
	PausedBy  string
	Reason    string
	UpdatedAt time.Time
}

// PauseRun flags a run as paused. The DAG walker will block at the
// next task-launch boundary until ResumeRun clears the flag. In-flight
// tasks are NOT preempted — pause is "stop launching new work", not
// "kill what's running." Use Cancel for the latter.
//
// Idempotent: re-pausing preserves paused_at so "how long has this
// been paused?" stays consistent.
func (s *Store) PauseRun(runID, actor, reason string) error {
	if runID == "" {
		return errors.New("state.PauseRun: empty run_id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`
		INSERT INTO run_state(run_id, paused, paused_at, paused_by, reason, updated_at)
		VALUES (?, 1, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET
		    paused = 1,
		    paused_at = CASE WHEN run_state.paused = 1 THEN run_state.paused_at ELSE excluded.paused_at END,
		    paused_by = excluded.paused_by,
		    reason = excluded.reason,
		    updated_at = excluded.updated_at
		`,
		runID, now, actor, reason, now,
	)
	if err != nil {
		return fmt.Errorf("state.PauseRun: %w", err)
	}
	return nil
}

// ResumeRun clears the pause flag — the natural partner to PauseRun.
// Idempotent: missing or already-active rows return nil. The DAG
// walker's poll loop notices on its next tick (default 500ms).
//
// Distinct from RetryFailed: ResumeRun targets the in-flight pause
// row, RetryFailed re-enqueues a terminal run with skip-succeeded.
func (s *Store) ResumeRun(runID, actor string) error {
	if runID == "" {
		return errors.New("state.ResumeRun: empty run_id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`
		UPDATE run_state SET
		    paused = 0,
		    paused_at = '',
		    paused_by = ?,
		    reason = '',
		    updated_at = ?
		WHERE run_id = ?`,
		actor, now, runID,
	)
	if err != nil {
		return fmt.Errorf("state.ResumeRun: %w", err)
	}
	return nil
}

// IsRunPaused returns true when the run has an active pause row.
// Missing rows are treated as active (hot path: the DAG walker calls
// this before every task launch). Empty run_id returns false.
func (s *Store) IsRunPaused(runID string) (bool, error) {
	if runID == "" {
		return false, nil
	}
	var paused int
	row := s.db.QueryRow(`SELECT paused FROM run_state WHERE run_id = ?`, runID)
	if err := row.Scan(&paused); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("state.IsRunPaused: %w", err)
	}
	return paused == 1, nil
}

// GetRunState returns the full control row, or a zero-value RunState
// with RunID populated when no row exists.
func (s *Store) GetRunState(runID string) (RunState, error) {
	st := RunState{RunID: runID}
	if runID == "" {
		return st, nil
	}
	var (
		paused              int
		pausedAt, updatedAt string
	)
	row := s.db.QueryRow(`
		SELECT paused, paused_at, paused_by, reason, updated_at
		FROM run_state WHERE run_id = ?`, runID)
	err := row.Scan(&paused, &pausedAt, &st.PausedBy, &st.Reason, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return st, nil
		}
		return st, fmt.Errorf("state.GetRunState: %w", err)
	}
	st.Paused = paused == 1
	if pausedAt != "" {
		if t, perr := time.Parse(time.RFC3339Nano, pausedAt); perr == nil {
			st.PausedAt = t
		}
	}
	if updatedAt != "" {
		if t, perr := time.Parse(time.RFC3339Nano, updatedAt); perr == nil {
			st.UpdatedAt = t
		}
	}
	return st, nil
}
