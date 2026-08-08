package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// stateLifetime is how long an issued state stays acceptable. Long enough for a
// consent screen and a password prompt, short enough that one captured in a
// browser history is useless by the time anyone reads it.
const stateLifetime = 15 * time.Minute

// ErrBadState reports a state parameter this server did not issue to this
// session, or issued too long ago.
var ErrBadState = errors.New("auth: the state parameter does not check out")

// IssueState returns an opaque value to carry through a third-party redirect
// and back.
//
// This is what stops login-CSRF on the OAuth callback. Without it an attacker
// starts the flow with their own Google account, captures the resulting code,
// and feeds the callback URL to the operator — whose session then has the
// attacker's mailbox connected to it, and whose forwarded receipts go somewhere
// they did not intend.
//
// So the state is bound to the session as well as signed: a value issued to one
// session does not verify against another, and an attacker cannot mint one for
// a session they do not hold. Nothing is stored — the value carries its own
// expiry and its own proof, the same way the CSRF token does.
func (g *Guard) IssueState(purpose, sessionTokenHash string) (string, error) {
	if sessionTokenHash == "" {
		return "", errors.New("auth: cannot issue a state without a session")
	}

	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("auth: read random: %w", err)
	}

	encodedNonce := base64.RawURLEncoding.EncodeToString(nonce)
	expiry := strconv.FormatInt(time.Now().Add(stateLifetime).Unix(), 10)
	body := encodedNonce + "." + expiry

	return body + "." + g.stateMAC(purpose, sessionTokenHash, body), nil
}

// CheckState verifies a state that came back through the redirect.
func (g *Guard) CheckState(purpose, sessionTokenHash, presented string) error {
	if sessionTokenHash == "" || presented == "" {
		return ErrBadState
	}

	parts := strings.Split(presented, ".")
	if len(parts) != 3 {
		return ErrBadState
	}
	body := parts[0] + "." + parts[1]

	// The MAC first: an unverified expiry is a number the caller chose.
	if !hmac.Equal([]byte(g.stateMAC(purpose, sessionTokenHash, body)), []byte(parts[2])) {
		return ErrBadState
	}

	expiry, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return ErrBadState
	}
	if time.Now().After(time.Unix(expiry, 0)) {
		return fmt.Errorf("%w: it expired", ErrBadState)
	}
	return nil
}

// stateMAC binds a state to its purpose and its session.
//
// The purpose is in the MAC so a state issued for one flow cannot be replayed
// into another when there is more than one — M4 and M5 add their own.
func (g *Guard) stateMAC(purpose, sessionTokenHash, body string) string {
	mac := hmac.New(sha256.New, g.csrf.key)
	// Length-prefixed rather than concatenated, so that no two different
	// triples can produce the same input.
	for _, field := range []string{purpose, sessionTokenHash, body} {
		mac.Write([]byte(strconv.Itoa(len(field)) + ":" + field))
	}
	return hex.EncodeToString(mac.Sum(nil))
}
