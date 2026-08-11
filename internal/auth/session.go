package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

const (
	// DefaultTTL is how long a session lives without being used. Thirty days
	// suits a single operator checking a receipt from a phone; the session is
	// revocable from the database either way.
	DefaultTTL = 30 * 24 * time.Hour

	// slideAfter is how stale last_seen_at has to be before a request writes
	// to extend it. Touching a session on every request would put a write on
	// the read path, and the writer pool is one connection on purpose.
	slideAfter = 24 * time.Hour

	tokenBytes = 32
)

// ErrNoSession reports that a token names no live session: absent, unknown,
// or expired. The three are deliberately indistinguishable to a caller, since
// every one of them means "sign in again".
var ErrNoSession = errors.New("auth: no live session")

// Sessions issues and resolves server-side sessions.
//
// The token the client holds is opaque random bytes and is never stored. Only
// its SHA-256 hash reaches the database, so a copy of the database does not
// hand over live sessions (docs/DESIGN.md §7.1).
type Sessions struct {
	repo *store.Repo
	ttl  time.Duration
	// now is swapped in tests to age a session without sleeping.
	now func() time.Time
}

// NewSessions returns a session manager over repo.
func NewSessions(repo *store.Repo) *Sessions {
	return &Sessions{repo: repo, ttl: DefaultTTL, now: time.Now}
}

// Issue creates a session for userID and returns the token to hand the client.
func (s *Sessions) Issue(ctx context.Context, userID int64, userAgent, ip string) (string, time.Time, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("auth: read token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	now := s.now().UTC()
	expires := now.Add(s.ttl)

	// A user agent is attacker-controlled and unbounded; the column is for
	// recognising your own devices, not for storing whatever was sent.
	if len(userAgent) > 255 {
		userAgent = userAgent[:255]
	}

	if _, err := s.repo.Write().CreateSession(ctx, sqlc.CreateSessionParams{
		UserID:     userID,
		TokenHash:  HashToken(token),
		ExpiresAt:  domain.Stamp(expires),
		UserAgent:  userAgent,
		Ip:         ip,
		LastSeenAt: domain.Stamp(now),
		CreatedAt:  domain.Stamp(now),
		UpdatedAt:  domain.Stamp(now),
	}); err != nil {
		return "", time.Time{}, fmt.Errorf("auth: create session: %w", err)
	}
	return token, expires, nil
}

// Lookup resolves a token to its session and user, sliding the expiry when the
// session has not been seen for a while. An expired session is deleted rather
// than left to accumulate.
func (s *Sessions) Lookup(ctx context.Context, token string) (sqlc.Session, sqlc.User, error) {
	if token == "" {
		return sqlc.Session{}, sqlc.User{}, ErrNoSession
	}

	hash := HashToken(token)
	row, err := s.repo.Read().GetSessionByTokenHash(ctx, hash)
	if err != nil {
		if store.NotFound(err) {
			return sqlc.Session{}, sqlc.User{}, ErrNoSession
		}
		return sqlc.Session{}, sqlc.User{}, fmt.Errorf("auth: read session: %w", err)
	}

	now := s.now().UTC()
	expires, err := time.Parse(time.RFC3339, row.Session.ExpiresAt)
	if err != nil || !now.Before(expires) {
		// Unparseable is treated as expired: a session whose expiry cannot be
		// read is a session that cannot be trusted to end.
		_, _ = s.repo.Write().DeleteSessionByTokenHash(ctx, hash)
		return sqlc.Session{}, sqlc.User{}, ErrNoSession
	}

	if seen, err := time.Parse(time.RFC3339, row.Session.LastSeenAt); err == nil {
		if now.Sub(seen) >= slideAfter {
			if err := s.repo.Write().TouchSession(ctx, sqlc.TouchSessionParams{
				LastSeenAt: domain.Stamp(now),
				ExpiresAt:  domain.Stamp(now.Add(s.ttl)),
				UpdatedAt:  domain.Stamp(now),
				ID:         row.Session.ID,
			}); err != nil {
				return sqlc.Session{}, sqlc.User{}, fmt.Errorf("auth: touch session: %w", err)
			}
		}
	}

	return row.Session, row.User, nil
}

// Revoke ends the session a token names. Revoking an unknown token is not an
// error: signing out twice is the same as signing out once.
func (s *Sessions) Revoke(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	if _, err := s.repo.Write().DeleteSessionByTokenHash(ctx, HashToken(token)); err != nil {
		return fmt.Errorf("auth: delete session: %w", err)
	}
	return nil
}

// RevokeAll ends every session belonging to a user, which is what a password
// change means.
func (s *Sessions) RevokeAll(ctx context.Context, userID int64) error {
	if _, err := s.repo.Write().DeleteSessionsForUser(ctx, userID); err != nil {
		return fmt.Errorf("auth: delete sessions: %w", err)
	}
	return nil
}

// Sweep removes sessions that have expired and returns how many went.
func (s *Sessions) Sweep(ctx context.Context) (int64, error) {
	n, err := s.repo.Write().DeleteExpiredSessions(ctx, domain.Stamp(s.now().UTC()))
	if err != nil {
		return 0, fmt.Errorf("auth: sweep sessions: %w", err)
	}
	return n, nil
}

// HashToken returns the hex SHA-256 of a session token, which is the only form
// of it that is ever stored.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
