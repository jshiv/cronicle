package state

import "testing"

func TestSecretStore_putGetRoundtrip(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if err := s.PutSecret("ANTHROPIC_API_KEY", "sk-ant-xxx", "test"); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	v, ok, err := s.GetSecret("ANTHROPIC_API_KEY")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if !ok || v != "sk-ant-xxx" {
		t.Errorf("get: ok=%v v=%q", ok, v)
	}
}

func TestSecretStore_missingReturnsOkFalse(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	v, ok, err := s.GetSecret("DOES_NOT_EXIST")
	if err != nil {
		t.Errorf("err: %v", err)
	}
	if ok || v != "" {
		t.Errorf("missing should return ok=false; got ok=%v v=%q", ok, v)
	}
}

func TestSecretStore_putBumpsRowVersion(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	_ = s.PutSecret("FOO", "v1", "a")
	_ = s.PutSecret("FOO", "v2", "a")

	var version int
	row := s.db.QueryRow(`SELECT version FROM secrets WHERE name = 'FOO'`)
	if err := row.Scan(&version); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if version != 2 {
		t.Errorf("rotation should bump version; got %d", version)
	}
	v, _, _ := s.GetSecret("FOO")
	if v != "v2" {
		t.Errorf("value not updated: %q", v)
	}
}

func TestSecretStore_etagMonotonicAcrossMutations(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	prev, _ := s.SecretsEtag()
	for i, op := range []func(){
		func() { _ = s.PutSecret("A", "1", "") },
		func() { _ = s.PutSecret("B", "1", "") },
		func() { _ = s.PutSecret("A", "2", "") },   // rotation
		func() { _, _ = s.DeleteSecret("A", "") },   // deletion
		func() { _, _ = s.DeleteSecret("ZZZ", "") }, // no-op delete: must still bump
	} {
		op()
		got, err := s.SecretsEtag()
		if err != nil {
			t.Fatalf("etag %d: %v", i, err)
		}
		if got == prev {
			t.Errorf("etag did not advance after op %d (was %q, still %q)", i, prev, got)
		}
		prev = got
	}
}

func TestSecretStore_listSecretsAndNames(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	_ = s.PutSecret("C", "c-val", "")
	_ = s.PutSecret("A", "a-val", "")
	_ = s.PutSecret("B", "b-val", "")

	names, err := s.ListSecretNames()
	if err != nil {
		t.Fatalf("ListSecretNames: %v", err)
	}
	want := []string{"A", "B", "C"}
	if len(names) != 3 || names[0] != want[0] || names[1] != want[1] || names[2] != want[2] {
		t.Errorf("names: want %v, got %v", want, names)
	}

	values, err := s.ListSecrets()
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if values["A"] != "a-val" || values["B"] != "b-val" || values["C"] != "c-val" {
		t.Errorf("values: %v", values)
	}
}

func TestSecretStore_deleteRemovesAndReportsOK(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	_ = s.PutSecret("X", "x-val", "")

	ok, err := s.DeleteSecret("X", "")
	if err != nil || !ok {
		t.Fatalf("delete present: ok=%v err=%v", ok, err)
	}
	_, present, _ := s.GetSecret("X")
	if present {
		t.Errorf("X should be gone after delete")
	}

	ok, err = s.DeleteSecret("X", "")
	if err != nil {
		t.Fatalf("delete absent: %v", err)
	}
	if ok {
		t.Errorf("delete-of-absent should return ok=false (got true)")
	}
}
