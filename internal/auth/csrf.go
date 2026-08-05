package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
)

// CSRF issues and checks the double-submit token that guards mutating
// requests (docs/DESIGN.md §7.1).
//
// The token is HMAC(key, session token hash) rather than an independent random
// value. Plain double-submit only compares a cookie against a header, which an
// attacker who can write a cookie on a sibling subdomain can satisfy — and a
// dynamic-DNS host shares its parent domain with strangers. Binding the token
// to the session means forging one requires the session cookie, which is
// HttpOnly and unreadable.
//
// Nothing is stored: the token is recomputed on each request.
type CSRF struct {
	key []byte
}

// NewCSRF returns a CSRF issuer keyed by key.
//
// An empty key gets a random one generated at startup, which is the case when
// RENTAL_BOT_SECRET_KEY is unset. That is safe but not durable: a restart
// invalidates outstanding CSRF tokens. GET /api/v1/auth/me reissues the
// cookie, and the frontend calls it on load, so the cost is one extra request
// after a restart rather than a failed save.
func NewCSRF(key []byte) *CSRF {
	if len(key) == 0 {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			// crypto/rand does not fail on any system this runs on, and a
			// server that cannot generate a key must not start pretending it
			// has CSRF protection.
			panic("auth: cannot read random key: " + err.Error())
		}
	}
	return &CSRF{key: key}
}

// Token returns the CSRF token for a session, named by that session's token
// hash. An empty hash yields an empty token: there is nothing to protect
// before a session exists.
func (c *CSRF) Token(sessionTokenHash string) string {
	if sessionTokenHash == "" {
		return ""
	}
	mac := hmac.New(sha256.New, c.key)
	mac.Write([]byte(sessionTokenHash))
	return hex.EncodeToString(mac.Sum(nil))
}

// Valid reports whether presented is the token for this session.
func (c *CSRF) Valid(sessionTokenHash, presented string) bool {
	if sessionTokenHash == "" || presented == "" {
		return false
	}
	return hmac.Equal([]byte(c.Token(sessionTokenHash)), []byte(presented))
}

// Safe reports whether a method is read-only and therefore exempt from the
// CSRF check. These are the RFC 9110 safe methods.
func Safe(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}
