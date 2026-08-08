// Package secret encrypts the handful of columns that must not be readable
// from a database copy.
//
// docs/DESIGN.md §9.2 names them: loan numbers, policy numbers, the Gmail
// refresh token, the Telegram bot token, scraper cookies. M3 is the milestone
// that fills the first one, so this is where the AES-GCM lives.
//
// The key comes from RENTAL_BOT_SECRET_KEY or a 0600 key file and never from
// the database — a key stored beside the thing it protects protects nothing.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// ErrNoKey reports that encryption was asked for without a key configured.
//
// It is a distinct error because the answer is operational — set
// RENTAL_BOT_SECRET_KEY — rather than a bug in a caller.
var ErrNoKey = errors.New("secret: no encryption key is configured")

// ErrCorrupt reports ciphertext that does not authenticate.
//
// GCM cannot tell "someone edited this row" from "this is the wrong key", and
// neither can this error. Both mean the plaintext is not recoverable and
// nothing should pretend otherwise.
var ErrCorrupt = errors.New("secret: the ciphertext does not authenticate")

// info separates this key from every other use of the configured secret.
//
// The same bytes already sign CSRF tokens. Deriving rather than using them
// directly means a weakness in one construction is not a weakness in the other,
// and it lets the configured value be a passphrase of any length rather than
// exactly 32 bytes.
const info = "rental-bot/field-encryption/v1"

// Box seals and opens field values under one derived key.
type Box struct {
	aead cipher.AEAD
}

// New derives the field-encryption key from the configured secret.
//
// An empty key is ErrNoKey rather than a zero key: the milestone that
// introduces an encrypted column is the milestone that makes the key required,
// and silently encrypting under nothing would be worse than refusing to start.
func New(key []byte) (*Box, error) {
	if len(key) == 0 {
		return nil, ErrNoKey
	}

	derived := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, key, nil, []byte(info)), derived); err != nil {
		return nil, fmt.Errorf("secret: derive key: %w", err)
	}

	block, err := aes.NewCipher(derived)
	if err != nil {
		return nil, fmt.Errorf("secret: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret: gcm: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext into a value that can be stored in a TEXT column.
//
// The nonce is fresh per call and is carried in front of the ciphertext, so a
// row is self-describing and re-encrypting the same value twice produces two
// different strings. That last part matters: identical ciphertext would tell a
// reader of the database that two rows hold the same secret.
func (b *Box) Seal(plaintext []byte) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secret: nonce: %w", err)
	}
	sealed := b.aead.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Open decrypts a value produced by Seal.
func (b *Box) Open(value string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("%w: not base64", ErrCorrupt)
	}
	if len(raw) < b.aead.NonceSize() {
		return nil, fmt.Errorf("%w: too short to carry a nonce", ErrCorrupt)
	}
	nonce, ciphertext := raw[:b.aead.NonceSize()], raw[b.aead.NonceSize():]

	plaintext, err := b.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, ErrCorrupt
	}
	return plaintext, nil
}

// SealString is Seal for a value that is already text.
func (b *Box) SealString(plaintext string) (string, error) {
	return b.Seal([]byte(plaintext))
}

// OpenString is Open for a value that is text.
func (b *Box) OpenString(value string) (string, error) {
	plaintext, err := b.Open(value)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}
