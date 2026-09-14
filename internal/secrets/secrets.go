// Package secrets encrypts third-party credentials at rest.
//
// A tenant's Google refresh token is a long-lived key to their calendar. It
// lives in the same SQLite file as their ordinary business data, which gets
// copied to backups and object storage — so it is encrypted before it is
// written, and the key lives in the environment rather than on disk beside it.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// ErrNoKey reports that TOKEN_KEY is unset. Callers should treat this as a
// refusal to store credentials rather than a reason to store them in the clear.
var ErrNoKey = errors.New("secrets: TOKEN_KEY is not set — refusing to store a credential unencrypted (generate one with: openssl rand -hex 32)")

// Box seals and opens credentials with AES-256-GCM.
type Box struct {
	aead cipher.AEAD
}

// NewBox builds a Box from a 64-character hex key (32 bytes).
func NewBox(hexKey string) (*Box, error) {
	if hexKey == "" {
		return nil, ErrNoKey
	}
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("secrets: TOKEN_KEY must be hex: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("secrets: TOKEN_KEY must decode to 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: build cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: build gcm: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext, returning nonce||ciphertext.
func (b *Box) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("secrets: generate nonce: %w", err)
	}
	// Prepending the nonce to its own ciphertext keeps the stored value
	// self-contained; GCM authenticates the rest.
	return b.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Open decrypts a value produced by Seal. It fails rather than returning
// garbage if the data was tampered with or the key has changed.
func (b *Box) Open(sealed []byte) ([]byte, error) {
	ns := b.aead.NonceSize()
	if len(sealed) < ns {
		return nil, errors.New("secrets: value is too short to be valid")
	}
	plaintext, err := b.aead.Open(nil, sealed[:ns], sealed[ns:], nil)
	if err != nil {
		return nil, fmt.Errorf("secrets: could not decrypt — wrong TOKEN_KEY, or the value was altered: %w", err)
	}
	return plaintext, nil
}
