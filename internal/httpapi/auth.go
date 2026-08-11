package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/farrellm/rental-bot/internal/auth"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// userResponse is the operator as the frontend sees them. The password hash
// and the TOTP secret are not in it, and never will be.
type userResponse struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
}

func newUserResponse(u sqlc.User) userResponse {
	return userResponse{ID: u.ID, Username: u.Username, Email: u.Email}
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// badCredentials is what every failed sign-in says, whether the username is
// unknown or the password is wrong. Distinguishing them would turn the form
// into an oracle for which accounts exist.
const badCredentials = "Username or password is incorrect."

// decoyHash is verified against when the username is unknown, so a sign-in
// attempt costs the same either way. Without it the response time answers
// "does this account exist?" for anyone who cares to measure.
//
// It is computed once, on the first miss, rather than at startup: a process
// that never sees a bad username should not pay for one.
var decoyHash = sync.OnceValue(func() string {
	h, err := auth.HashPassword("decoy for constant-time sign-in")
	if err != nil {
		return ""
	}
	return h
})

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	username := strings.TrimSpace(req.Username)
	if username == "" || req.Password == "" {
		WriteProblem(w, r, http.StatusBadRequest, "Enter a username and a password.")
		return
	}

	// Two keys, both checked. Per-IP alone lets a botnet spread a guess across
	// hosts; per-account alone lets one host walk the account list.
	ipKey := "ip:" + auth.ClientIP(r)
	userKey := "user:" + strings.ToLower(username)
	for _, key := range []string{ipKey, userKey} {
		if ok, wait := s.limiter.Allow(key); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			WriteProblem(w, r, http.StatusTooManyRequests,
				"Too many sign-in attempts. Try again in "+humanWait(wait)+".")
			return
		}
	}

	ctx := r.Context()
	log := loggerFrom(ctx)

	user, err := s.repo.Read().GetUserByUsername(ctx, username)
	if err != nil && !store.NotFound(err) {
		log.Error("read user", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not check those credentials.")
		return
	}

	hash := user.PasswordHash
	found := err == nil
	if !found {
		hash = decoyHash()
	}

	ok, verifyErr := auth.VerifyPassword(req.Password, hash)
	if verifyErr != nil && found {
		// A stored hash that will not parse is a corrupted row, not a wrong
		// password, and it has to be visible in the log rather than sending
		// the operator hunting for a typo.
		log.Error("stored password hash is unreadable", "user_id", user.ID, "error", verifyErr)
	}
	if !found || !ok {
		s.limiter.Fail(ipKey)
		s.limiter.Fail(userKey)
		log.Info("sign-in refused", "username", username, "ip", auth.ClientIP(r))
		WriteProblem(w, r, http.StatusUnauthorized, badCredentials)
		return
	}

	s.limiter.Reset(ipKey)
	s.limiter.Reset(userKey)

	// The password is in hand and already verified, so an upgrade to stronger
	// parameters costs nothing extra. This is the only moment it is possible.
	if auth.NeedsRehash(user.PasswordHash) {
		if rehashed, err := auth.HashPassword(req.Password); err == nil {
			now := timestamp()
			if _, err := s.repo.Write().UpsertUser(ctx, sqlc.UpsertUserParams{
				Username: user.Username, Email: user.Email, PasswordHash: rehashed,
				CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				// Not fatal: the sign-in is valid either way.
				log.Warn("could not upgrade password hash", "user_id", user.ID, "error", err)
			}
		}
	}

	token, expires, err := s.guard.Sessions().Issue(ctx, user.ID, r.UserAgent(), auth.ClientIP(r))
	if err != nil {
		log.Error("issue session", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not start a session.")
		return
	}

	s.guard.SetCookies(w, token, expires)
	log.Info("signed in", "user_id", user.ID, "username", user.Username)
	writeJSON(w, r, http.StatusOK, newUserResponse(user))
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(auth.SessionCookie); err == nil {
		if err := s.guard.Sessions().Revoke(r.Context(), cookie.Value); err != nil {
			loggerFrom(r.Context()).Error("revoke session", "error", err)
			WriteProblem(w, r, http.StatusInternalServerError, "Could not end the session.")
			return
		}
	}
	s.guard.ClearCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFrom(r.Context())
	if !ok {
		WriteProblem(w, r, http.StatusUnauthorized, "Sign in to continue.")
		return
	}

	// Reissue the CSRF cookie on the way past. The frontend calls this on
	// load, so a restart that rotated the signing key costs one extra request
	// rather than a save that fails with no explanation.
	if session, ok := auth.SessionFrom(r.Context()); ok {
		expires, err := time.Parse(time.RFC3339, session.ExpiresAt)
		if err == nil {
			s.guard.RefreshCSRF(w, session.TokenHash, expires)
		}
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, newUserResponse(user))
}

// humanWait renders a backoff the way a person would say it.
func humanWait(d time.Duration) string {
	if d < time.Minute {
		seconds := int(d.Seconds()) + 1
		if seconds == 1 {
			return "a second"
		}
		return strconv.Itoa(seconds) + " seconds"
	}
	minutes := int(d.Minutes()) + 1
	if minutes == 1 {
		return "a minute"
	}
	return strconv.Itoa(minutes) + " minutes"
}
