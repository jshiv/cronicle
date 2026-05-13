package state

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RunnerState captures the runner-wide control row. The store keeps
// exactly one row (id=1, enforced by CHECK constraint); these helpers
// hide that detail behind a flat struct.
type RunnerState struct {
	Drained   bool
	DrainedAt time.Time
	DrainedBy string
	Reason    string
	UpdatedAt time.Time
}

// SetDrained flips the runner into drained mode. Idempotent — re-draining
// preserves drained_at so "how long has the runner been off?" reads
// correctly. Once drained:
//
//   - The cron tick (ProduceSchedule) skips with entry_type=schedule_skipped
//     and reason=drained.
//   - Listener triggers (/trigger, /tasks/{task}/trigger, /trigger-from)
//     return 503.
//   - The DAG walker blocks at every task-launch boundary, mirroring
//     the per-run pause loop.
//
// In-flight tasks complete normally; drain is a "stop accepting new
// work" verb, not a kill switch. Use cancel for in-flight termination.
func (s *Store) SetDrained(actor, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`
		UPDATE runner_state SET
		    drained = 1,
		    drained_at = CASE WHEN drained = 1 THEN drained_at ELSE ? END,
		    drained_by = ?,
		    reason = ?,
		    updated_at = ?
		WHERE id = 1`,
		now, actor, reason, now,
	)
	if err != nil {
		return fmt.Errorf("state.SetDrained: %w", err)
	}
	return nil
}

// ClearDrained takes the runner out of drained mode. Idempotent: an
// already-active runner returns nil. drained_at is cleared so the next
// drain starts a fresh epoch.
func (s *Store) ClearDrained(actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`
		UPDATE runner_state SET
		    drained = 0,
		    drained_at = '',
		    drained_by = ?,
		    reason = '',
		    updated_at = ?
		WHERE id = 1`,
		actor, now,
	)
	if err != nil {
		return fmt.Errorf("state.ClearDrained: %w", err)
	}
	return nil
}

// IsDrained returns true when the runner is in drained mode. Hot-path
// query for cron tick + walker; single PK SELECT against the singleton
// row.
func (s *Store) IsDrained() (bool, error) {
	var drained int
	row := s.db.QueryRow(`SELECT drained FROM runner_state WHERE id = 1`)
	if err := row.Scan(&drained); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Row missing — treat as active. The migration seeds the
			// row, but a freshly upgraded DB hasn't necessarily seen
			// it commit yet in a hot test.
			return false, nil
		}
		return false, fmt.Errorf("state.IsDrained: %w", err)
	}
	return drained == 1, nil
}

// GetRunnerState returns the full singleton row, or a zero-value
// RunnerState when the row hasn't been seeded.
func (s *Store) GetRunnerState() (RunnerState, error) {
	var (
		st                    RunnerState
		drained               int
		drainedAt, updatedAt  string
	)
	row := s.db.QueryRow(`
		SELECT drained, drained_at, drained_by, reason, updated_at
		FROM runner_state WHERE id = 1`)
	err := row.Scan(&drained, &drainedAt, &st.DrainedBy, &st.Reason, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return st, nil
		}
		return st, fmt.Errorf("state.GetRunnerState: %w", err)
	}
	st.Drained = drained == 1
	if drainedAt != "" {
		if t, perr := time.Parse(time.RFC3339Nano, drainedAt); perr == nil {
			st.DrainedAt = t
		}
	}
	if updatedAt != "" {
		if t, perr := time.Parse(time.RFC3339Nano, updatedAt); perr == nil {
			st.UpdatedAt = t
		}
	}
	return st, nil
}
