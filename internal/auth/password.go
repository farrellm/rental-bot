// Package auth holds password hashing, server-side sessions, CSRF, and the
// middleware that guards the API (docs/DESIGN.md §10).
//
// This is a single-operator system, so there is no registration endpoint and
// no password reset flow: users are created by `rental-bot -create-user`, and
// the only credential is a username and a password.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Params are the argon2id cost parameters.
//
// They are encoded into every hash, so raising them later is a matter of
// changing DefaultParams: existing hashes keep verifying under the parameters
// they were written with, and rehash on the next successful sign-in.
type Params struct {
	Time    uint32 // passes over memory
	Memory  uint32 // KiB
	Threads uint8
	KeyLen  uint32
	SaltLen uint32
}

// DefaultParams targets roughly 100ms on a small self-hosted box: enough to
// make an offline attack on a stolen database expensive, little enough that a
// sign-in still feels immediate.
var DefaultParams = Params{
	Time:    3,
	Memory:  64 * 1024, // 64 MiB
	Threads: 4,
	KeyLen:  32,
	SaltLen: 16,
}

// ErrHashFormat reports that a stored hash could not be parsed.
var ErrHashFormat = errors.New("auth: password hash is not in argon2id PHC format")

// HashPassword hashes a password with DefaultParams.
func HashPassword(password string) (string, error) {
	return DefaultParams.Hash(password)
}

// Hash returns the PHC-encoded argon2id hash of password.
//
// The encoding is the standard
// $argon2id$v=19$m=...,t=...,p=...$salt$hash, so the parameters travel with the
// hash. Nothing has to remember how a given password was hashed, and the cost
// can be raised without a migration.
func (p Params) Hash(password string) (string, error) {
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}

	sum := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads,
		b64.EncodeToString(salt), b64.EncodeToString(sum)), nil
}

// VerifyPassword reports whether password matches the PHC-encoded hash.
//
// The comparison is constant time. A malformed hash is an error rather than a
// false, so a corrupted row cannot quietly turn into "wrong password" and send
// an operator hunting for a typo that is not there.
func VerifyPassword(password, encoded string) (bool, error) {
	p, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}

	got := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NeedsRehash reports whether a stored hash was written with weaker parameters
// than DefaultParams, so a caller holding a verified password can upgrade it.
func NeedsRehash(encoded string) bool {
	p, _, _, err := decodeHash(encoded)
	if err != nil {
		return true
	}
	return p.Time < DefaultParams.Time ||
		p.Memory < DefaultParams.Memory ||
		p.KeyLen < DefaultParams.KeyLen
}

func decodeHash(encoded string) (p Params, salt, sum []byte, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return p, nil, nil, ErrHashFormat
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return p, nil, nil, ErrHashFormat
	}
	if version != argon2.Version {
		return p, nil, nil, fmt.Errorf("%w: version %d", ErrHashFormat, version)
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return p, nil, nil, ErrHashFormat
	}

	b64 := base64.RawStdEncoding
	if salt, err = b64.DecodeString(parts[4]); err != nil {
		return p, nil, nil, ErrHashFormat
	}
	if sum, err = b64.DecodeString(parts[5]); err != nil {
		return p, nil, nil, ErrHashFormat
	}

	p.SaltLen = uint32(len(salt))
	p.KeyLen = uint32(len(sum))
	return p, salt, sum, nil
}
