package state

import (
	"testing"
)

// TestMigrate_V10IdempotentOnReRun simulates the crash window the
// previous migrate() exposed: if the process died between the v10
// ALTER TABLE and the version row insert, the next startup would
// rerun the ALTER and fail with "duplicate column name".
//
// migrate() is now structured so each version's DDL + version-row
// insert happen as adjacent steps (tiny crash window), AND v10's
// runner probes the column list first via migrateV10Idempotent. Calling
// migrate() repeatedly against the same DB must therefore succeed.
func TestMigrate_V10IdempotentOnReRun(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.db.Close()

	// First call set up everything to v10. Now simulate a crash-recover
	// where the version row didn't land: roll the schema_versions row
	// for v10 back manually, then re-invoke migrate.
	if _, err := s.db.Exec(`DELETE FROM schema_versions WHERE version = 10`); err != nil {
		t.Fatalf("simulate partial-migration: %v", err)
	}
	if err := s.migrate(); err != nil {
		t.Fatalf("re-running migrate after partial v10 must succeed; got: %v", err)
	}

	// And the version row is back.
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_versions WHERE version = 10`).Scan(&n); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if n != 1 {
		t.Errorf("expected version 10 row to be recorded after recovery; got %d", n)
	}
}

// TestMigrateV10Idempotent_DirectInvocation confirms migrateV10Idempotent
// itself short-circuits when the columns are already present — this is
// the helper that makes the crash-recovery path work.
func TestMigrateV10Idempotent_DirectInvocation(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.db.Close()

	// Columns are present after Open's initial migrate. A direct call
	// to migrateV10Idempotent must not error and must not re-issue the
	// ALTER (which would fail with "duplicate column name").
	if err := s.migrateV10Idempotent(); err != nil {
		t.Errorf("calling migrateV10Idempotent against a v10 schema must be a no-op; got: %v", err)
	}
}
