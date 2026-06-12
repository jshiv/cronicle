package state

import (
	"database/sql"
	"net/url"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestPostgres_Backend_Integration exercises the Postgres state.Backend
// against a real Postgres (set TEST_PG_DSN, e.g.
// postgres://postgres:pw@127.0.0.1:5433/cronicle?sslmode=disable).
// Skipped otherwise so CI (no Postgres) stays green. Covers the
// dialect-heavy surface: event→run projection (the ${last_run} read),
// the durable job queue, and secrets.
func TestPostgres_Backend_Integration(t *testing.T) {
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set TEST_PG_DSN to run the Postgres state.Backend integration test")
	}
	resetPublicSchema(t, dsn)

	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open(postgres): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.dialect != dialectPostgres {
		t.Fatalf("dialect = %v, want postgres", s.dialect)
	}

	apply := func(line string) {
		ev, ok := DecodeEvent([]byte(line))
		if !ok {
			t.Fatalf("decode: %s", line)
		}
		if err := s.Apply(ev); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}

	// --- run projection / the ${last_run} read path ---
	apply(`{"time":"2026-06-01T10:00:00Z","entry_type":"schedule_start","run_id":"R1","schedule":"daily","tasks":["t"]}`)
	apply(`{"time":"2026-06-01T10:00:05Z","entry_type":"schedule_complete","run_id":"R1","schedule":"daily","task_count":1,"duration_ms":5000,"success":true}`)
	apply(`{"time":"2026-06-01T11:00:00Z","entry_type":"schedule_start","run_id":"R2","schedule":"daily","tasks":["t"]}`)
	apply(`{"time":"2026-06-01T11:00:05Z","entry_type":"schedule_complete","run_id":"R2","schedule":"daily","task_count":1,"duration_ms":5000,"success":true}`)

	got, err := s.ListRuns(ListFilter{Schedule: "daily", Status: StatusSucceeded, Limit: 1})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(got) != 1 || got[0].RunID != "R2" {
		t.Fatalf("ListRuns last-success = %+v, want R2", got)
	}
	if got[0].StartedAt.IsZero() {
		t.Errorf("R2 started_at is zero — ${last_run} would be empty")
	}

	// --- durable job queue: enqueue → claim → ack ---
	if err := s.Enqueue("J1", "daily", []byte(`{"name":"daily"}`)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	job, err := s.Claim("W1", time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if job.RunID != "J1" {
		t.Fatalf("claimed %q, want J1", job.RunID)
	}
	if err := s.Ack("J1", "W1", true, ""); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	// --- workers registry (upserted on claim; explicit upsert too) ---
	if err := s.UpsertWorker("W1", "host-a"); err != nil {
		t.Fatalf("UpsertWorker: %v", err)
	}
	if ws, err := s.ListWorkers(); err != nil || len(ws) == 0 {
		t.Fatalf("ListWorkers: %v (n=%d)", err, len(ws))
	}

	// --- secrets ---
	if err := s.PutSecret("FOO", "bar", "tester"); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	v, ok, err := s.GetSecret("FOO")
	if err != nil || !ok || v != "bar" {
		t.Fatalf("GetSecret = (%q,%v,%v), want (bar,true,nil)", v, ok, err)
	}

	// --- runtime control state tables (CURRENT_TIMESTAMP defaults, upserts) ---
	if err := s.SetDrained("tester", "maintenance"); err != nil {
		t.Fatalf("SetDrained: %v", err)
	}
	if drained, err := s.IsDrained(); err != nil || !drained {
		t.Fatalf("IsDrained = (%v,%v), want true", drained, err)
	}
	if err := s.SetSchedulePaused("daily", "tester", "hold"); err != nil {
		t.Fatalf("SetSchedulePaused: %v", err)
	}
	if paused, err := s.IsSchedulePaused("daily"); err != nil || !paused {
		t.Fatalf("IsSchedulePaused = (%v,%v), want true", paused, err)
	}

	// --- control channel (pub/sub) ---
	ch, unsub := s.Subscribe("W1")
	defer unsub()
	if !s.PushControl("W1", ControlMsg{Type: "cancel", RunID: "J1"}) {
		t.Fatalf("PushControl: not delivered")
	}
	select {
	case msg := <-ch:
		if msg.Type != "cancel" || msg.RunID != "J1" {
			t.Fatalf("control msg = %+v, want cancel/J1", msg)
		}
	case <-time.After(time.Second):
		t.Fatalf("control msg not received")
	}

	// --- the crux: durability across a reconnect (= producer pod restart) ---
	_ = s.Close()
	s2, err := Open(dsn) // reconnect to the SAME Postgres; migrate is idempotent
	if err != nil {
		t.Fatalf("reopen(postgres): %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	got2, err := s2.ListRuns(ListFilter{Schedule: "daily", Status: StatusSucceeded, Limit: 1})
	if err != nil || len(got2) != 1 || got2[0].RunID != "R2" {
		t.Fatalf("after reconnect ListRuns = %+v (err %v), want R2 — history must survive", got2, err)
	}
}

// resetPublicSchema drops + recreates the public schema for a clean
// slate. DESTRUCTIVE — it must only ever run against a throwaway test
// database. guardThrowawayDB aborts if the target looks like a real
// deployment (e.g. someone pointed TEST_PG_DSN at the cronicle-infra
// api's shared Postgres, whose tables live in public). Learned the
// hard way: without this guard, a stray TEST_PG_DSN wipes the platform
// registry.
func resetPublicSchema(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw pg: %v", err)
	}
	defer db.Close()
	guardThrowawayDB(t, db)
	if _, err := db.Exec(`DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
}

// guardThrowawayDB aborts the test (rather than wiping data) if the
// target database holds tables that don't belong to cronicle's STATE
// schema. The state package only ever creates: runs, tasks, events,
// schema_versions, jobs, workers, schedule_state, task_state,
// run_state, runner_state, secrets, secrets_meta. Anything else in
// public — deployments, orgs, users, goose_db_version, … — means this
// is a shared/real database (e.g. the cronicle-infra api's), and a
// DROP SCHEMA public CASCADE would destroy it. Only run the destructive
// PG integration tests against a dedicated throwaway database.
func guardThrowawayDB(t *testing.T, db *sql.DB) {
	t.Helper()
	stateTables := map[string]bool{
		"runs": true, "tasks": true, "events": true, "schema_versions": true,
		"jobs": true, "workers": true, "schedule_state": true, "task_state": true,
		"run_state": true, "runner_state": true, "secrets": true, "secrets_meta": true,
	}
	rows, err := db.Query(`
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE'`)
	if err != nil {
		t.Fatalf("guard: list public tables: %v", err)
	}
	defer rows.Close()
	var foreign []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("guard: scan: %v", err)
		}
		if !stateTables[name] {
			foreign = append(foreign, name)
		}
	}
	if len(foreign) > 0 {
		t.Fatalf("REFUSING to reset public schema: it contains non-state tables %v — "+
			"TEST_PG_DSN appears to point at a shared/real database (e.g. the cronicle-infra api). "+
			"These tests DROP SCHEMA public CASCADE; point TEST_PG_DSN at a dedicated throwaway database instead.",
			foreign)
	}
}

// withSearchPath returns dsn with its search_path query param set to
// schema — the same shape the cronicle-infra control plane hands each
// per-deployment producer (postgres://…?search_path=dep_<id>).
func withSearchPath(t *testing.T, dsn, schema string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// dropSchema removes a per-deployment schema (CASCADE) so the test is
// rerunnable. Best-effort cleanup.
func dropSchema(t *testing.T, dsn, schema string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw pg: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`DROP SCHEMA IF EXISTS ` + quoteIdent(schema) + ` CASCADE`); err != nil {
		t.Fatalf("drop schema %s: %v", schema, err)
	}
}

// secretsColumnExists reports whether the named column is present on the
// secrets table in the given schema. This is the probe that #160 got
// wrong — an unscoped information_schema query returned a sibling
// schema's column and made the migration skip the ALTER.
func secretsColumnExists(t *testing.T, dsn, schema, col string) bool {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw pg: %v", err)
	}
	defer db.Close()
	var n int
	err = db.QueryRow(`
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'secrets' AND column_name = $2`,
		schema, col).Scan(&n)
	if err != nil {
		t.Fatalf("probe %s.secrets.%s: %v", schema, col, err)
	}
	return n > 0
}

// TestPostgres_MultiSchema_MigrationIsolation is the #160 regression
// guard. cronicle-infra runs MANY producers against ONE shared
// Postgres, each pinned to its own schema via search_path=dep_<id>.
//
// The v10 migration probes whether the secrets-encryption columns
// (value_ct/value_nonce) already exist before running the ALTER. Before
// #160 that probe used an unscoped information_schema query, so the
// SECOND deployment's migration saw the FIRST deployment's columns,
// wrongly concluded its own schema was migrated, skipped the ALTER, and
// then BackfillSecrets failed with "column value_ct does not exist" —
// killing every 2nd+ producer's state store.
//
// This test reproduces that exact topology: migrate schema A (which
// creates the columns), then migrate a fresh schema B in the same
// database, and assert B got its OWN columns and a working secrets
// round-trip. A single :memory: SQLite store can never model this —
// which is why both #159 and #160 escaped to a live cluster.
func TestPostgres_MultiSchema_MigrationIsolation(t *testing.T) {
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set TEST_PG_DSN to run the Postgres multi-schema migration test")
	}

	const schemaA = "dep_test_aaaa"
	const schemaB = "dep_test_bbbb"
	dropSchema(t, dsn, schemaA)
	dropSchema(t, dsn, schemaB)
	t.Cleanup(func() {
		dropSchema(t, dsn, schemaA)
		dropSchema(t, dsn, schemaB)
	})

	// --- Deployment A: first producer in the cluster. Always worked,
	// even before #160, because there's no sibling schema to leak from.
	sa, err := Open(withSearchPath(t, dsn, schemaA))
	if err != nil {
		t.Fatalf("Open(schema A): %v", err)
	}
	t.Cleanup(func() { _ = sa.Close() })
	if !secretsColumnExists(t, dsn, schemaA, "value_ct") ||
		!secretsColumnExists(t, dsn, schemaA, "value_nonce") {
		t.Fatalf("schema A missing v10 columns after migrate")
	}

	// --- Deployment B: the second producer. This is where #160 bit:
	// its migration must NOT be fooled by schema A's columns into
	// skipping its own ALTER.
	sb, err := Open(withSearchPath(t, dsn, schemaB))
	if err != nil {
		t.Fatalf("Open(schema B): %v (this is the #160 failure mode — "+
			"BackfillSecrets fails when the v10 ALTER was skipped)", err)
	}
	t.Cleanup(func() { _ = sb.Close() })

	if !secretsColumnExists(t, dsn, schemaB, "value_ct") {
		t.Errorf("schema B missing value_ct — the v10 probe leaked across schemas (#160)")
	}
	if !secretsColumnExists(t, dsn, schemaB, "value_nonce") {
		t.Errorf("schema B missing value_nonce — the v10 probe leaked across schemas (#160)")
	}

	// --- The symptom the operator actually saw: secrets unusable on B.
	// PutSecret writes to value_ct/value_nonce (when AEAD is bound) or
	// the plaintext column; GetSecret reads back. Without the columns,
	// the producer logged "state store open failed; projection disabled"
	// and the worker got 503 on every claim.
	if err := sb.PutSecret("API_KEY", "secret-b", "tester"); err != nil {
		t.Fatalf("PutSecret on schema B: %v", err)
	}
	v, ok, err := sb.GetSecret("API_KEY")
	if err != nil || !ok || v != "secret-b" {
		t.Fatalf("GetSecret on schema B = (%q,%v,%v), want (secret-b,true,nil)", v, ok, err)
	}

	// --- Isolation: A and B share one database but must not see each
	// other's secrets (different schemas → different secrets tables).
	if _, ok, _ := sa.GetSecret("API_KEY"); ok {
		t.Errorf("schema A sees schema B's secret — schemas are not isolated")
	}
}

// TestPostgres_MigrateTwiceIdempotent guards the #159/M7 crash-recovery
// path explicitly: running migrate() twice against the same schema must
// be a clean no-op. The v10 step in particular has no ALTER … IF NOT
// EXISTS on SQLite and relies on the column probe; a re-run that
// re-issued the ALTER would error with "duplicate column".
func TestPostgres_MigrateTwiceIdempotent(t *testing.T) {
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set TEST_PG_DSN to run the Postgres migrate-twice test")
	}
	const schema = "dep_test_idem"
	dropSchema(t, dsn, schema)
	t.Cleanup(func() { dropSchema(t, dsn, schema) })

	scoped := withSearchPath(t, dsn, schema)
	s1, err := Open(scoped)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	_ = s1.Close()

	// Reopen = reconnect = producer pod restart. migrate() runs again.
	s2, err := Open(scoped)
	if err != nil {
		t.Fatalf("second Open (migrate must be idempotent): %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	// And a third time via a direct migrate() call on the live store,
	// to exercise migrateV10Idempotent's already-applied branch.
	if err := s2.migrate(); err != nil {
		t.Fatalf("third migrate() call must be a no-op: %v", err)
	}
}
