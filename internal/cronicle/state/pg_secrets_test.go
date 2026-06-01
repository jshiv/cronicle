package state

import (
	"os"
	"testing"
)

// TestPostgres_EncryptedSecrets validates the at-rest-encrypted secret path
// against a real Postgres (the kind cluster runs with CRONICLE_DEK_HEX set, so
// secrets land in the BYTEA value_ct/value_nonce columns rather than the
// plaintext column). The plaintext round-trip is covered by the main
// integration test; this one specifically exercises:
//   - Seal on write -> BYTEA columns through the pgx rebind driver
//   - Open on read after a reconnect (= producer pod restart) -> decrypt
//   - fail-loud when the DEK is absent but the row is encrypted
//
// Env-guarded by TEST_PG_DSN, same as TestPostgres_Backend_Integration.
func TestPostgres_EncryptedSecrets(t *testing.T) {
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set TEST_PG_DSN to run the Postgres encrypted-secret integration test")
	}
	resetPublicSchema(t, dsn)

	// 32-byte DEK (64 hex chars), same shape as CRONICLE_DEK_HEX in prod/kind.
	const dekHex = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	aead, err := NewAEAD(dekHex)
	if err != nil {
		t.Fatalf("NewAEAD: %v", err)
	}

	const name = "ANTHROPIC_API_KEY"
	const secret = "sk-ant-xchacha-roundtrip-9f3c-AND-some-padding-to-exceed-a-block"

	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.WithAEAD(aead)
	if err := s.PutSecret(name, secret, "tester"); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	_ = s.Close()

	// Reopen with the SAME DEK: value_ct/value_nonce must survive the BYTEA
	// round-trip and decrypt back to the original plaintext.
	s2, err := Open(dsn)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	s2.WithAEAD(aead)
	got, ok, err := s2.GetSecret(name)
	if err != nil || !ok {
		t.Fatalf("GetSecret = (%q, %v, %v), want decrypted value", got, ok, err)
	}
	if got != secret {
		t.Fatalf("decrypted = %q, want %q", got, secret)
	}

	// Confirm it was actually stored encrypted: empty plaintext column, non-empty
	// ciphertext + nonce (the same shape we observed in the kind Postgres).
	var plainLen, ctLen, nonceLen int
	row := s2.db.QueryRow(
		`SELECT COALESCE(length(value),0), COALESCE(length(value_ct),0), COALESCE(length(value_nonce),0)
		   FROM secrets WHERE name = ?`, name)
	if err := row.Scan(&plainLen, &ctLen, &nonceLen); err != nil {
		t.Fatalf("inspect row: %v", err)
	}
	if plainLen != 0 || ctLen == 0 || nonceLen == 0 {
		t.Fatalf("want ciphertext-only row, got plain=%d ct=%d nonce=%d", plainLen, ctLen, nonceLen)
	}
	_ = s2.Close()

	// Fail-loud: reopening WITHOUT a DEK must refuse to read the encrypted row
	// rather than silently returning garbage.
	s3, err := Open(dsn)
	if err != nil {
		t.Fatalf("reopen no-dek: %v", err)
	}
	defer s3.Close()
	if v, _, err := s3.GetSecret(name); err == nil {
		t.Fatalf("GetSecret without DEK should error on an encrypted row, got value %q nil err", v)
	}
}
