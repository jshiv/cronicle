package state

import (
	"encoding/hex"
	"testing"
)

// Store tests for the at-rest secrets path: they pin the seal/open
// round-trip through PutSecret/GetSecret, the AAD-binding guard
// (renaming a row breaks decryption), the backfill of legacy rows,
// and the loud failure when an encrypted row is read without an AEAD.

func newTestAEAD(t *testing.T) *AEAD {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	a, err := NewAEAD(hex.EncodeToString(key))
	if err != nil {
		t.Fatalf("NewAEAD: %v", err)
	}
	return a
}

func TestAEAD_sealOpenRoundtrip(t *testing.T) {
	a := newTestAEAD(t)
	pt := []byte("sk-ant-shhh")
	ct, nonce, err := a.Seal(pt, "ANTHROPIC_API_KEY")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	out, err := a.Open(ct, nonce, "ANTHROPIC_API_KEY")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if string(out) != string(pt) {
		t.Errorf("roundtrip mismatch: want %q, got %q", pt, out)
	}
}

func TestAEAD_rejectsAADSwap(t *testing.T) {
	// AAD = secret name. A ciphertext copied from row A to row B must
	// fail to decrypt — without this, a Postgres-replica compromise
	// that lets an attacker overwrite secret names could redirect
	// known-plaintext rows ("PUBLIC_FOO") into sensitive slots
	// ("ANTHROPIC_API_KEY").
	a := newTestAEAD(t)
	ct, nonce, err := a.Seal([]byte("payload"), "A_NAME")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := a.Open(ct, nonce, "B_NAME"); err == nil {
		t.Errorf("Open with mismatched name should fail (AAD binding); got nil error")
	}
}

func TestStore_putGetWithAEAD_doesNotStorePlaintext(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	s.WithAEAD(newTestAEAD(t))

	if err := s.PutSecret("ANTHROPIC_API_KEY", "sk-ant-real", "test"); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}

	// pg_dump simulation: read the underlying columns directly.
	var (
		value     string
		ct, nonce []byte
	)
	row := s.db.QueryRow(`SELECT value, value_ct, value_nonce FROM secrets WHERE name = 'ANTHROPIC_API_KEY'`)
	if err := row.Scan(&value, &ct, &nonce); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if value != "" {
		t.Errorf("plaintext column should be empty under AEAD; got %q", value)
	}
	if len(ct) == 0 || len(nonce) == 0 {
		t.Errorf("ct/nonce should be populated; got ct=%d nonce=%d", len(ct), len(nonce))
	}

	v, ok, err := s.GetSecret("ANTHROPIC_API_KEY")
	if err != nil || !ok || v != "sk-ant-real" {
		t.Errorf("roundtrip: ok=%v v=%q err=%v", ok, v, err)
	}
}

func TestStore_getEncryptedWithoutAEAD_errors(t *testing.T) {
	// Seal a row, then drop the AEAD on a fresh handle — the second
	// reader has no key, must fail loud rather than hand back ''.
	s, _ := Open(":memory:")
	defer s.Close()
	s.WithAEAD(newTestAEAD(t))
	if err := s.PutSecret("SECRET", "value", ""); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	// Drop key without resetting rows.
	s.aead = nil

	_, _, err := s.GetSecret("SECRET")
	if err == nil {
		t.Errorf("reading encrypted row with no AEAD should error")
	}
}

func TestStore_legacyPlaintextRowReadableUnderAEAD(t *testing.T) {
	// Pre-v10 rows have ct=NULL, value=<plaintext>. After binding an
	// AEAD, those rows must remain readable through the plaintext
	// column until BackfillSecrets re-seals them.
	s, _ := Open(":memory:")
	defer s.Close()
	if err := s.PutSecret("LEGACY", "legacy-plaintext", ""); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	s.WithAEAD(newTestAEAD(t))

	v, ok, err := s.GetSecret("LEGACY")
	if err != nil || !ok || v != "legacy-plaintext" {
		t.Errorf("legacy fallback: ok=%v v=%q err=%v", ok, v, err)
	}
}

func TestStore_backfillEncryptsLegacyRows(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	_ = s.PutSecret("A", "a-pt", "")
	_ = s.PutSecret("B", "b-pt", "")
	s.WithAEAD(newTestAEAD(t))

	n, err := s.BackfillSecrets()
	if err != nil {
		t.Fatalf("BackfillSecrets: %v", err)
	}
	if n != 2 {
		t.Errorf("backfill count: want 2, got %d", n)
	}

	for _, name := range []string{"A", "B"} {
		var (
			value     string
			ct, nonce []byte
		)
		row := s.db.QueryRow(`SELECT value, value_ct, value_nonce FROM secrets WHERE name = ?`, name)
		if err := row.Scan(&value, &ct, &nonce); err != nil {
			t.Fatalf("scan %s: %v", name, err)
		}
		if value != "" || len(ct) == 0 || len(nonce) == 0 {
			t.Errorf("%s not encrypted: value=%q ct=%d nonce=%d", name, value, len(ct), len(nonce))
		}
	}

	// Second call is a no-op (no plaintext rows remaining).
	n2, err := s.BackfillSecrets()
	if err != nil {
		t.Fatalf("BackfillSecrets second pass: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second backfill should be no-op; touched %d", n2)
	}
}

func TestStore_backfillWithoutAEADIsNoop(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	_ = s.PutSecret("X", "x-pt", "")

	n, err := s.BackfillSecrets()
	if err != nil || n != 0 {
		t.Errorf("backfill no-AEAD: want (0, nil); got (%d, %v)", n, err)
	}
}
