package state

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
)

// SecretStore implementation backed by the v9 `secrets` + `secrets_meta`
// tables. Reads are cheap PK lookups; writes touch two rows (the
// secret row + the monotonic etag counter) in a single transaction so
// SecretsEtag() always reflects the latest mutation.

// PutSecret upserts (name, value) and bumps the global etag. version
// on the row increments via ON CONFLICT, so the column tracks per-row
// edit count (useful for audit "secret X was rotated N times").
func (s *Store) PutSecret(name, value, actor string) error {
	if name == "" {
		return errors.New("state.PutSecret: empty name")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("state.PutSecret: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		INSERT INTO secrets (name, value, version, updated_at, updated_by)
		VALUES (?, ?, 1, datetime('now'), ?)
		ON CONFLICT(name) DO UPDATE SET
		    value      = excluded.value,
		    version    = secrets.version + 1,
		    updated_at = datetime('now'),
		    updated_by = excluded.updated_by`,
		name, value, actor,
	); err != nil {
		return fmt.Errorf("state.PutSecret: upsert: %w", err)
	}
	if err := bumpSecretsEtag(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state.PutSecret: commit: %w", err)
	}
	return nil
}

// GetSecret returns the value for name. (name not found) → (empty, false, nil).
// Errors are reserved for real I/O / scan failures.
func (s *Store) GetSecret(name string) (string, bool, error) {
	var value string
	row := s.db.QueryRow(`SELECT value FROM secrets WHERE name = ?`, name)
	switch err := row.Scan(&value); {
	case err == nil:
		return value, true, nil
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	default:
		return "", false, fmt.Errorf("state.GetSecret: %w", err)
	}
}

// ListSecrets returns the full name → value map. The map is freshly
// allocated; callers may mutate it without affecting the store.
func (s *Store) ListSecrets() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT name, value FROM secrets`)
	if err != nil {
		return nil, fmt.Errorf("state.ListSecrets: query: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return nil, fmt.Errorf("state.ListSecrets: scan: %w", err)
		}
		out[name] = value
	}
	return out, rows.Err()
}

// ListSecretNames returns just the names, sorted. Used by paths that
// should never touch plaintext (CLI list, audit log lines).
func (s *Store) ListSecretNames() ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM secrets ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("state.ListSecretNames: query: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("state.ListSecretNames: scan: %w", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Defensive: sqlite ORDER BY is already sorted, but normalize in
	// case a future backend doesn't.
	sort.Strings(out)
	return out, nil
}

// DeleteSecret removes name if present. Bumps the etag whether or not
// a row was removed — the caller asked, that's a control-plane event
// observers may care about regardless. ok reports actual deletion so
// the CLI can print "removed" vs "no such secret".
func (s *Store) DeleteSecret(name, actor string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("state.DeleteSecret: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`DELETE FROM secrets WHERE name = ?`, name)
	if err != nil {
		return false, fmt.Errorf("state.DeleteSecret: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("state.DeleteSecret: rows affected: %w", err)
	}
	if err := bumpSecretsEtag(tx); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("state.DeleteSecret: commit: %w", err)
	}
	// actor is captured by future audit hooks; today we just consume
	// it to satisfy the interface so callers don't drift.
	_ = actor
	return n > 0, nil
}

// SecretsEtag returns the monotonic counter as a decimal string.
// Format is opaque to callers; only equality + ordering matter.
func (s *Store) SecretsEtag() (string, error) {
	var n int64
	row := s.db.QueryRow(`SELECT etag_counter FROM secrets_meta WHERE id = 1`)
	if err := row.Scan(&n); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Row should always exist after migration v9 (INSERT OR
			// IGNORE in the migration). Defensive 0 keeps callers
			// from blowing up if the row was somehow deleted.
			return "0", nil
		}
		return "", fmt.Errorf("state.SecretsEtag: %w", err)
	}
	return strconv.FormatInt(n, 10), nil
}

func bumpSecretsEtag(tx *sql.Tx) error {
	if _, err := tx.Exec(`UPDATE secrets_meta SET etag_counter = etag_counter + 1 WHERE id = 1`); err != nil {
		return fmt.Errorf("state: bump secrets etag: %w", err)
	}
	return nil
}
