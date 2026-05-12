package state

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ScheduleState captures the runtime control row for a single schedule.
// A schedule with no row is treated as active (not paused, not drained).
type ScheduleState struct {
	Name      string
	Paused    bool
	PausedAt  time.Time // zero when not paused
	PausedBy  string    // free-form actor identifier (user, api-key prefix, "system")
	Reason    string
	Drained   bool      // reserved for Phase 4 (quiesce)
	UpdatedAt time.Time
}

// SetSchedulePaused marks a schedule as paused. Upsert semantics: a fresh
// row is created if none exists, otherwise the paused flag, timestamp,
// actor, and reason are overwritten. Resuming an already-paused schedule
// is a no-op on the row itself (we don't refresh paused_at on idempotent
// pauses — that timestamp records when the pause began).
func (s *Store) SetSchedulePaused(name, actor, reason string) error {
	if name == "" {
		return errors.New("state.SetSchedulePaused: empty name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`
		INSERT INTO schedule_state(name, paused, paused_at, paused_by, reason, updated_at)
		VALUES (?, 1, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
		    paused = 1,
		    paused_at = CASE WHEN schedule_state.paused = 1 THEN schedule_state.paused_at ELSE excluded.paused_at END,
		    paused_by = excluded.paused_by,
		    reason = excluded.reason,
		    updated_at = excluded.updated_at
		`,
		name, now, actor, reason, now,
	)
	if err != nil {
		return fmt.Errorf("state.SetSchedulePaused: %w", err)
	}
	return nil
}

// ClearSchedulePaused resumes a schedule. Idempotent: a missing row or a
// row already at paused=0 returns nil. paused_at is cleared so the next
// pause starts a fresh epoch.
func (s *Store) ClearSchedulePaused(name, actor string) error {
	if name == "" {
		return errors.New("state.ClearSchedulePaused: empty name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`
		UPDATE schedule_state SET
		    paused = 0,
		    paused_at = '',
		    paused_by = ?,
		    reason = '',
		    updated_at = ?
		WHERE name = ?`,
		actor, now, name,
	)
	if err != nil {
		return fmt.Errorf("state.ClearSchedulePaused: %w", err)
	}
	return nil
}

// IsSchedulePaused returns true when the schedule has an active pause
// row. A missing row is treated as active, matching the cron-loop
// default (only pause/drain rows are persisted).
func (s *Store) IsSchedulePaused(name string) (bool, error) {
	if name == "" {
		return false, nil
	}
	var paused int
	row := s.db.QueryRow(`SELECT paused FROM schedule_state WHERE name = ?`, name)
	if err := row.Scan(&paused); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("state.IsSchedulePaused: %w", err)
	}
	return paused == 1, nil
}

// GetScheduleState returns the full control row for a schedule, or a
// zero-value ScheduleState{Name: name} when no row exists. Callers
// should not depend on a sentinel error to detect "no pause" — the
// boolean fields are the source of truth.
func (s *Store) GetScheduleState(name string) (ScheduleState, error) {
	st := ScheduleState{Name: name}
	if name == "" {
		return st, nil
	}
	var (
		paused, drained int
		pausedAt, updatedAt string
	)
	row := s.db.QueryRow(`
		SELECT paused, paused_at, paused_by, reason, drained, updated_at
		FROM schedule_state WHERE name = ?`, name)
	err := row.Scan(&paused, &pausedAt, &st.PausedBy, &st.Reason, &drained, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return st, nil
		}
		return st, fmt.Errorf("state.GetScheduleState: %w", err)
	}
	st.Paused = paused == 1
	st.Drained = drained == 1
	if pausedAt != "" {
		if t, perr := time.Parse(time.RFC3339Nano, pausedAt); perr == nil {
			st.PausedAt = t
		}
	}
	if updatedAt != "" {
		if t, perr := time.Parse(time.RFC3339Nano, updatedAt); perr == nil {
			st.UpdatedAt = t
		} else if t, perr := time.Parse("2006-01-02 15:04:05", updatedAt); perr == nil {
			// sqlite datetime('now') format fallback
			st.UpdatedAt = t
		}
	}
	return st, nil
}

// ListPausedSchedules returns every schedule currently flagged paused.
// Ordered by paused_at ascending so the oldest pauses (likely-forgotten)
// surface first in operator dashboards.
func (s *Store) ListPausedSchedules() ([]ScheduleState, error) {
	rows, err := s.db.Query(`
		SELECT name, paused_at, paused_by, reason, drained, updated_at
		FROM schedule_state
		WHERE paused = 1
		ORDER BY paused_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("state.ListPausedSchedules: %w", err)
	}
	defer rows.Close()

	var out []ScheduleState
	for rows.Next() {
		var (
			st                  ScheduleState
			pausedAt, updatedAt string
			drained             int
		)
		if err := rows.Scan(&st.Name, &pausedAt, &st.PausedBy, &st.Reason, &drained, &updatedAt); err != nil {
			return nil, fmt.Errorf("state.ListPausedSchedules: scan: %w", err)
		}
		st.Paused = true
		st.Drained = drained == 1
		if t, perr := time.Parse(time.RFC3339Nano, pausedAt); perr == nil {
			st.PausedAt = t
		}
		if t, perr := time.Parse(time.RFC3339Nano, updatedAt); perr == nil {
			st.UpdatedAt = t
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state.ListPausedSchedules: iter: %w", err)
	}
	return out, nil
}
