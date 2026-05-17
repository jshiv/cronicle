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
//
// At-rest encryption is opt-in via Store.WithAEAD. When bound:
//   - PutSecret seals into value_ct/value_nonce and writes value=''.
//   - GetSecret/ListSecrets open ct/nonce when present, otherwise fall
//     back to the plaintext `value` column. That fallback path matters
//     for two transitions: legacy rows written before v10 ran, and
//     a controlled rollback to a prior cronicle binary that doesn't
//     know about ct/nonce.
//
// When AEAD is nil the table behaves exactly as it did in v9: writes
// land in `value`, reads come from `value`. ct/nonce columns are NULL
// and unused. This keeps the local-dev path zero-ceremony.

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
	if s.aead != nil {
		ct, nonce, err := s.aead.Seal([]byte(value), name)
		if err != nil {
			return fmt.Errorf("state.PutSecret: seal: %w", err)
		}
		// Plaintext column intentionally written empty: the row's
		// authoritative value lives in value_ct/value_nonce. Keeping
		// the column rather than dropping it preserves one-release
		// rollback safety (a prior binary that hasn't learned about
		// ct/nonce reads '' and treats the secret as cleared, vs.
		// crashing on a missing column).
		if _, err := tx.Exec(`
			INSERT INTO secrets (name, value, value_ct, value_nonce, version, updated_at, updated_by)
			VALUES (?, '', ?, ?, 1, datetime('now'), ?)
			ON CONFLICT(name) DO UPDATE SET
			    value       = '',
			    value_ct    = excluded.value_ct,
			    value_nonce = excluded.value_nonce,
			    version     = secrets.version + 1,
			    updated_at  = datetime('now'),
			    updated_by  = excluded.updated_by`,
			name, ct, nonce, actor,
		); err != nil {
			return fmt.Errorf("state.PutSecret: upsert: %w", err)
		}
	} else {
		if _, err := tx.Exec(`
			INSERT INTO secrets (name, value, version, updated_at, updated_by)
			VALUES (?, ?, 1, datetime('now'), ?)
			ON CONFLICT(name) DO UPDATE SET
			    value      = excluded.value,
			    value_ct   = NULL,
			    value_nonce = NULL,
			    version    = secrets.version + 1,
			    updated_at = datetime('now'),
			    updated_by = excluded.updated_by`,
			name, value, actor,
		); err != nil {
			return fmt.Errorf("state.PutSecret: upsert: %w", err)
		}
	}
	if err := bumpSecretsEtag(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state.PutSecret: commit: %w", err)
	}
	return nil
}

// resolveSecretValue picks the plaintext: ct/nonce when present (and
// AEAD bound), otherwise the plaintext column. Returns an error if
// the row is encrypted but no AEAD is bound — better to fail loud
// than to silently hand back an empty string for what the caller
// asked for as a real secret.
func (s *Store) resolveSecretValue(name string, value string, ct, nonce []byte) (string, error) {
	if len(ct) > 0 {
		if s.aead == nil {
			return "", fmt.Errorf("state: secret %q is encrypted but no DEK is configured", name)
		}
		pt, err := s.aead.Open(ct, nonce, name)
		if err != nil {
			return "", fmt.Errorf("state: open secret %q: %w", name, err)
		}
		return string(pt), nil
	}
	return value, nil
}

// GetSecret returns the value for name. (name not found) → (empty, false, nil).
// Errors are reserved for real I/O / scan / decrypt failures.
func (s *Store) GetSecret(name string) (string, bool, error) {
	var (
		value string
		ct    []byte
		nonce []byte
	)
	row := s.db.QueryRow(`SELECT value, value_ct, value_nonce FROM secrets WHERE name = ?`, name)
	switch err := row.Scan(&value, &ct, &nonce); {
	case err == nil:
		pt, err := s.resolveSecretValue(name, value, ct, nonce)
		if err != nil {
			return "", false, err
		}
		return pt, true, nil
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	default:
		return "", false, fmt.Errorf("state.GetSecret: %w", err)
	}
}

// ListSecrets returns the full name → value map. The map is freshly
// allocated; callers may mutate it without affecting the store.
func (s *Store) ListSecrets() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT name, value, value_ct, value_nonce FROM secrets`)
	if err != nil {
		return nil, fmt.Errorf("state.ListSecrets: query: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var (
			name, value string
			ct, nonce   []byte
		)
		if err := rows.Scan(&name, &value, &ct, &nonce); err != nil {
			return nil, fmt.Errorf("state.ListSecrets: scan: %w", err)
		}
		pt, err := s.resolveSecretValue(name, value, ct, nonce)
		if err != nil {
			return nil, err
		}
		out[name] = pt
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
