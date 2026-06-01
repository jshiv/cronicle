package state

import (
	"database/sql"
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

// resetPublicSchema drops + recreates the public schema for a clean slate.
func resetPublicSchema(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw pg: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
}
