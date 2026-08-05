package auth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// Cookie and header names. The session cookie is HttpOnly so script cannot
// read it; the CSRF cookie deliberately is not, because the frontend has to
// echo it back in a header.
const (
	SessionCookie = "rb_session"
	CSRFCookie    = "rb_csrf"
	CSRFHeader    = "X-CSRF-Token"
)

// Deny writes an error response. httpapi supplies httpapi.WriteProblem, which
// keeps every error in this API RFC 7807 shaped without auth having to import
// httpapi and close a cycle.
type Deny func(w http.ResponseWriter, r *http.Request, status int, detail string)

// Guard authenticates requests and enforces CSRF on the mutating ones.
type Guard struct {
	sessions *Sessions
	csrf     *CSRF
	deny     Deny
	// secure sets the Secure attribute on both cookies. It follows the
	// configured base URL's scheme rather than being hardcoded: a Secure
	// cookie is mandatory in production and would silently break a phone
	// testing against http:// over the LAN in development.
	secure bool
}

// NewGuard returns a guard over sessions.
func NewGuard(sessions *Sessions, csrf *CSRF, secure bool, deny Deny) *Guard {
	return &Guard{sessions: sessions, csrf: csrf, deny: deny, secure: secure}
}

type contextKey int

const (
	userKey contextKey = iota
	sessionKey
)

// UserFrom returns the authenticated user, if the request came through
// RequireSession.
func UserFrom(ctx context.Context) (sqlc.User, bool) {
	u, ok := ctx.Value(userKey).(sqlc.User)
	return u, ok
}

// SessionFrom returns the live session, if the request came through
// RequireSession.
func SessionFrom(ctx context.Context) (sqlc.Session, bool) {
	s, ok := ctx.Value(sessionKey).(sqlc.Session)
	return s, ok
}

// RequireSession rejects a request that carries no live session, and any
// mutating request whose CSRF token does not match.
//
// A request without a session is a 401, never a redirect and never a 404: the
// client is a single-page app, and it needs to be told to sign in rather than
// handed a login page it did not ask for (docs/DESIGN.md §9.2).
func (g *Guard) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(SessionCookie)
		if err != nil {
			g.deny(w, r, http.StatusUnauthorized, "Sign in to continue.")
			return
		}

		session, user, err := g.sessions.Lookup(r.Context(), cookie.Value)
		if err != nil {
			if errors.Is(err, ErrNoSession) {
				// The browser is holding a cookie that names nothing. Clear it
				// so it stops being sent and the next sign-in starts clean.
				g.ClearCookies(w)
				g.deny(w, r, http.StatusUnauthorized, "Your session has ended. Sign in again.")
				return
			}
			g.deny(w, r, http.StatusInternalServerError, "Could not read the session.")
			return
		}

		if !Safe(r.Method) && !g.csrf.Valid(session.TokenHash, r.Header.Get(CSRFHeader)) {
			g.deny(w, r, http.StatusForbidden, "This request is missing a valid CSRF token.")
			return
		}

		ctx := context.WithValue(r.Context(), userKey, user)
		ctx = context.WithValue(ctx, sessionKey, session)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// SetCookies writes the session and CSRF cookies for a freshly issued token.
func (g *Guard) SetCookies(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   g.secure,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:  CSRFCookie,
		Value: g.csrf.Token(HashToken(token)),
		Path:  "/",
		// Expires with the session, and readable by script on purpose: the
		// frontend has to copy it into the CSRF header.
		Expires:  expires,
		HttpOnly: false,
		Secure:   g.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// RefreshCSRF reissues the CSRF cookie for a live session, which is what
// GET /auth/me does. After a restart without a configured secret key the
// signing key is new, so the token the browser holds no longer verifies.
func (g *Guard) RefreshCSRF(w http.ResponseWriter, sessionTokenHash string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookie,
		Value:    g.csrf.Token(sessionTokenHash),
		Path:     "/",
		Expires:  expires,
		HttpOnly: false,
		Secure:   g.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearCookies expires both cookies.
func (g *Guard) ClearCookies(w http.ResponseWriter) {
	for _, name := range []string{SessionCookie, CSRFCookie} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: name == SessionCookie,
			Secure:   g.secure,
			SameSite: http.SameSiteLaxMode,
		})
	}
}

// Sessions returns the session manager, for handlers that sign in and out.
func (g *Guard) Sessions() *Sessions { return g.sessions }

// ClientIP returns the address to rate-limit against.
//
// It reads RemoteAddr and nothing else. X-Forwarded-For is attacker-controlled
// unless a trusted proxy is known to rewrite it, and trusting it here would
// let one client spread its attempts across a limitless set of keys and defeat
// the limiter entirely.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
