package state

// At-rest envelope encryption for the secrets table.
//
// Cipher: XChaCha20-Poly1305 AEAD.
//
//	key      32 bytes — Data Encryption Key, supplied by the caller
//	         (typically CRONICLE_DEK_HEX env on the cronicle binary).
//	         Local dev runs without it: state.db stays plaintext, same
//	         posture as today. Production deployments set it once and
//	         the secrets table is opaque outside the running process.
//	nonce    24 bytes — random per ciphertext, stored alongside.
//	overhead 16 bytes — Poly1305 tag.
//
// The cipher choice and AAD-binding pattern mirror cronicle-infra's
// `internal/secrets.AEAD`. We re-implement here rather than importing
// because the cronicle binary is the OSS distribution — it must build
// without the cronicle-infra sibling repo on disk.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

// AEAD wraps a 32-byte DEK.
type AEAD struct {
	key []byte
}

// NewAEAD parses a hex-encoded 32-byte key. Returns error if length is wrong.
func NewAEAD(hexKey string) (*AEAD, error) {
	k, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("decode hex key: %w", err)
	}
	if len(k) != chacha20poly1305.KeySize {
		return nil, fmt.Errorf("dek must be %d bytes (got %d)",
			chacha20poly1305.KeySize, len(k))
	}
	return &AEAD{key: k}, nil
}

// Seal returns (ciphertext, nonce). Plaintext is bound to the secret's
// name as additional data so a ciphertext row moved to a different
// name (in-place row swap) fails to decrypt.
func (a *AEAD) Seal(plaintext []byte, name string) ([]byte, []byte, error) {
	aead, err := chacha20poly1305.NewX(a.key)
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	ct := aead.Seal(nil, nonce, plaintext, []byte(name))
	return ct, nonce, nil
}

// Open returns the plaintext.
func (a *AEAD) Open(ciphertext, nonce []byte, name string) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(a.key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != aead.NonceSize() {
		return nil, errors.New("bad nonce size")
	}
	return aead.Open(nil, nonce, ciphertext, []byte(name))
}
