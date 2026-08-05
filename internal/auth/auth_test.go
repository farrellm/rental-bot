package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
	"github.com/farrellm/rental-bot/migrations"
)

// openRepo returns a migrated database under t.TempDir. Sessions are a
// database feature, so these tests exercise the real schema rather than a
// fake: an expiry that is stored as the wrong string still parses in Go.
func openRepo(t *testing.T) *store.Repo {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "rental.db"), 2)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(t.Context(), migrations.FS); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db.Repo()
}

func newUser(t *testing.T, repo *store.Repo, username, password string) sqlc.User {
	t.Helper()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	u, err := repo.Write().UpsertUser(t.Context(), sqlc.UpsertUserParams{
		Username: username, PasswordHash: hash, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	return u
}

func TestPasswordVerifies(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	tests := []struct {
		password string
		want     bool
	}{
		{"correct horse battery staple", true},
		{"correct horse battery stapl", false},
		{"", false},
		{"CORRECT HORSE BATTERY STAPLE", false},
	}
	for _, tt := range tests {
		got, err := VerifyPassword(tt.password, hash)
		if err != nil {
			t.Errorf("VerifyPassword(%q): %v", tt.password, err)
			continue
		}
		if got != tt.want {
			t.Errorf("VerifyPassword(%q) = %v, want %v", tt.password, got, tt.want)
		}
	}
}

func TestPasswordHashCarriesItsParameters(t *testing.T) {
	// The point of the PHC encoding: nothing has to remember how a given
	// password was hashed, so the cost can be raised without a migration.
	hash, err := HashPassword("hunter2")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Errorf("hash = %q, want the argon2id PHC prefix", hash)
	}

	// A hash written under weaker parameters still verifies, and is reported
	// as due for an upgrade.
	weak := Params{Time: 1, Memory: 8 * 1024, Threads: 1, KeyLen: 16, SaltLen: 16}
	old, err := weak.Hash("hunter2")
	if err != nil {
		t.Fatalf("weak.Hash: %v", err)
	}
	ok, err := VerifyPassword("hunter2", old)
	if err != nil || !ok {
		t.Errorf("VerifyPassword against a weaker hash = %v, %v; want true, nil", ok, err)
	}
	if !NeedsRehash(old) {
		t.Error("NeedsRehash on a weaker hash = false, want true")
	}
	if NeedsRehash(hash) {
		t.Error("NeedsRehash on a current hash = true, want false")
	}
}

func TestPasswordRejectsAMalformedHash(t *testing.T) {
	// A corrupted row has to fail loudly. Reporting it as "wrong password"
	// would send an operator hunting for a typo that is not there.
	for _, bad := range []string{
		"", "not a hash", "$argon2i$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA",
		"$argon2id$v=16$m=65536,t=3,p=4$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=4$!!!$aGFzaA",
	} {
		if _, err := VerifyPassword("hunter2", bad); err == nil {
			t.Errorf("VerifyPassword against %q returned no error", bad)
		}
	}
}

func TestSessionIssueAndLookup(t *testing.T) {
	repo := openRepo(t)
	sessions := NewSessions(repo)
	user := newUser(t, repo, "alice", "hunter2")

	token, expires, err := sessions.Issue(t.Context(), user.ID, "Mozilla/5.0", "10.0.0.4")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if token == "" {
		t.Fatal("Issue returned an empty token")
	}
	if !expires.After(time.Now()) {
		t.Errorf("expiry %v is not in the future", expires)
	}

	got, gotUser, err := sessions.Lookup(t.Context(), token)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if gotUser.ID != user.ID {
		t.Errorf("Lookup returned user %d, want %d", gotUser.ID, user.ID)
	}
	if got.UserAgent != "Mozilla/5.0" || got.Ip != "10.0.0.4" {
		t.Errorf("session recorded %q / %q", got.UserAgent, got.Ip)
	}
}

func TestSessionStoresOnlyTheTokenHash(t *testing.T) {
	// A copy of the database must not hand over live sessions.
	repo := openRepo(t)
	sessions := NewSessions(repo)
	user := newUser(t, repo, "alice", "hunter2")

	token, _, err := sessions.Issue(t.Context(), user.ID, "", "")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	row, err := repo.Read().GetSessionByTokenHash(t.Context(), HashToken(token))
	if err != nil {
		t.Fatalf("GetSessionByTokenHash: %v", err)
	}
	if row.Session.TokenHash == token {
		t.Fatal("the raw token was stored, want only its hash")
	}
	if strings.Contains(row.Session.TokenHash, token) {
		t.Fatal("the stored hash contains the raw token")
	}
}

func TestSessionLookupRejectsUnknownAndExpired(t *testing.T) {
	repo := openRepo(t)
	sessions := NewSessions(repo)
	user := newUser(t, repo, "alice", "hunter2")

	if _, _, err := sessions.Lookup(t.Context(), ""); err != ErrNoSession {
		t.Errorf("Lookup of an empty token = %v, want ErrNoSession", err)
	}
	if _, _, err := sessions.Lookup(t.Context(), "nonsense"); err != ErrNoSession {
		t.Errorf("Lookup of an unknown token = %v, want ErrNoSession", err)
	}

	token, _, err := sessions.Issue(t.Context(), user.ID, "", "")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Age the clock past the TTL rather than sleeping for a month.
	sessions.now = func() time.Time { return time.Now().Add(DefaultTTL + time.Hour) }
	if _, _, err := sessions.Lookup(t.Context(), token); err != ErrNoSession {
		t.Errorf("Lookup of an expired session = %v, want ErrNoSession", err)
	}

	// An expired session is deleted rather than left to accumulate.
	if _, err := repo.Read().GetSessionByTokenHash(t.Context(), HashToken(token)); !store.NotFound(err) {
		t.Errorf("the expired session survived lookup: %v", err)
	}
}

func TestSessionSlidesOnlyWhenStale(t *testing.T) {
	// Touching the session on every request would put a write on the read
	// path, and the writer pool is one connection on purpose.
	repo := openRepo(t)
	sessions := NewSessions(repo)
	user := newUser(t, repo, "alice", "hunter2")

	token, _, err := sessions.Issue(t.Context(), user.ID, "", "")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	first, _, err := sessions.Lookup(t.Context(), token)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}

	// A request a minute later does not write.
	sessions.now = func() time.Time { return time.Now().Add(time.Minute) }
	if _, _, err := sessions.Lookup(t.Context(), token); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	unchanged, _, _ := sessions.Lookup(t.Context(), token)
	if unchanged.LastSeenAt != first.LastSeenAt {
		t.Errorf("last_seen_at moved after a minute: %q -> %q", first.LastSeenAt, unchanged.LastSeenAt)
	}

	// A request past the slide interval does.
	sessions.now = func() time.Time { return time.Now().Add(slideAfter + time.Minute) }
	if _, _, err := sessions.Lookup(t.Context(), token); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	slid, _, _ := sessions.Lookup(t.Context(), token)
	if slid.LastSeenAt == first.LastSeenAt {
		t.Errorf("last_seen_at did not move after %v", slideAfter)
	}
	if slid.ExpiresAt <= first.ExpiresAt {
		t.Errorf("expiry did not extend: %q -> %q", first.ExpiresAt, slid.ExpiresAt)
	}
}

func TestSessionRevoke(t *testing.T) {
	repo := openRepo(t)
	sessions := NewSessions(repo)
	user := newUser(t, repo, "alice", "hunter2")

	token, _, err := sessions.Issue(t.Context(), user.ID, "", "")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := sessions.Revoke(t.Context(), token); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, _, err := sessions.Lookup(t.Context(), token); err != ErrNoSession {
		t.Errorf("Lookup after Revoke = %v, want ErrNoSession", err)
	}
	// Signing out twice is the same as signing out once.
	if err := sessions.Revoke(t.Context(), token); err != nil {
		t.Errorf("second Revoke: %v", err)
	}
}

func TestSessionSweepRemovesExpired(t *testing.T) {
	repo := openRepo(t)
	sessions := NewSessions(repo)
	user := newUser(t, repo, "alice", "hunter2")

	if _, _, err := sessions.Issue(t.Context(), user.ID, "", ""); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	sessions.now = func() time.Time { return time.Now().Add(DefaultTTL + time.Hour) }
	n, err := sessions.Sweep(t.Context())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("Sweep removed %d sessions, want 1", n)
	}
}

func TestCSRFTokenIsBoundToItsSession(t *testing.T) {
	csrf := NewCSRF([]byte("a fixed key for the test"))

	a, b := HashToken("session-a"), HashToken("session-b")
	if !csrf.Valid(a, csrf.Token(a)) {
		t.Error("a session's own token did not validate")
	}
	// The attack this shape defends against: a token minted for one session,
	// or by an attacker who can write a cookie but cannot read the HttpOnly
	// session cookie, must not satisfy another session's check.
	if csrf.Valid(a, csrf.Token(b)) {
		t.Error("another session's CSRF token validated")
	}
	if csrf.Valid(a, "") || csrf.Valid("", csrf.Token(a)) {
		t.Error("an empty token or session validated")
	}
	if csrf.Valid(a, "deadbeef") {
		t.Error("an arbitrary token validated")
	}

	// A different key mints different tokens, which is what makes a restart
	// without a configured secret invalidate outstanding tokens.
	other := NewCSRF([]byte("a different key"))
	if csrf.Valid(a, other.Token(a)) {
		t.Error("a token minted under a different key validated")
	}
}

func TestSafeMethods(t *testing.T) {
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		if !Safe(m) {
			t.Errorf("Safe(%s) = false, want true", m)
		}
	}
	for _, m := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete, http.MethodPut} {
		if Safe(m) {
			t.Errorf("Safe(%s) = true, want false", m)
		}
	}
}

func TestLimiterBacksOffExponentially(t *testing.T) {
	l := NewLimiter()
	base := time.Now()
	l.now = func() time.Time { return base }

	// The first few failures cost nothing: typing a password wrong twice is a
	// Tuesday, not an attack.
	for range freeAttempts {
		if ok, _ := l.Allow("10.0.0.4"); !ok {
			t.Fatal("a free attempt was refused")
		}
		l.Fail("10.0.0.4")
	}

	ok, wait := l.Allow("10.0.0.4")
	if ok {
		t.Fatal("the attempt after the free ones was allowed")
	}
	if wait <= 0 || wait > backoffBase {
		t.Errorf("first backoff = %v, want (0, %v]", wait, backoffBase)
	}

	// Each further failure doubles the wait.
	l.Fail("10.0.0.4")
	_, longer := l.Allow("10.0.0.4")
	if longer <= wait {
		t.Errorf("backoff did not grow: %v then %v", wait, longer)
	}

	// Waiting it out lets the caller back in.
	l.now = func() time.Time { return base.Add(backoffMax + time.Second) }
	if ok, _ := l.Allow("10.0.0.4"); !ok {
		t.Error("the caller was still refused after the backoff elapsed")
	}
}

func TestLimiterCapsTheBackoff(t *testing.T) {
	// A locked-out operator waits minutes, not until the next restart.
	l := NewLimiter()
	base := time.Now()
	l.now = func() time.Time { return base }

	for range 100 {
		l.Fail("alice")
	}
	_, wait := l.Allow("alice")
	if wait > backoffMax {
		t.Errorf("backoff = %v, want at most %v", wait, backoffMax)
	}
	if wait <= 0 {
		t.Errorf("backoff = %v after 100 failures, want a delay", wait)
	}
}

func TestLimiterKeysAreIndependent(t *testing.T) {
	// Per IP and per account, so one attacker cannot lock out the operator by
	// hammering their username, and cannot escape their own limit by changing
	// which username they guess.
	l := NewLimiter()
	for range freeAttempts + 5 {
		l.Fail("ip:10.0.0.4")
	}
	if ok, _ := l.Allow("ip:10.0.0.4"); ok {
		t.Error("the failing key was allowed")
	}
	if ok, _ := l.Allow("ip:10.0.0.9"); !ok {
		t.Error("an unrelated key was refused")
	}
	if ok, _ := l.Allow("user:alice"); !ok {
		t.Error("an unrelated account was refused")
	}
}

func TestLimiterResetOnSuccess(t *testing.T) {
	l := NewLimiter()
	for range freeAttempts + 2 {
		l.Fail("alice")
	}
	l.Reset("alice")
	if ok, _ := l.Allow("alice"); !ok {
		t.Error("a reset key was still refused")
	}
}

// guardFor builds a guard over a real session manager, plus a handler that
// reports who it thinks the caller is.
func guardFor(t *testing.T) (*Guard, *store.Repo, http.Handler) {
	t.Helper()
	repo := openRepo(t)
	guard := NewGuard(NewSessions(repo), NewCSRF([]byte("test key")), false,
		func(w http.ResponseWriter, _ *http.Request, status int, detail string) {
			http.Error(w, detail, status)
		})

	handler := guard.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := UserFrom(r.Context())
		if !ok {
			t.Error("RequireSession admitted a request with no user in context")
		}
		w.Write([]byte(u.Username))
	}))
	return guard, repo, handler
}

// signIn issues a session and returns the request cookies and CSRF token.
func signIn(t *testing.T, guard *Guard, repo *store.Repo) (session, csrf string) {
	t.Helper()
	user := newUser(t, repo, "alice", "hunter2")
	token, _, err := guard.Sessions().Issue(context.Background(), user.ID, "", "")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return token, guard.csrf.Token(HashToken(token))
}

func TestRequireSessionRejectsWithoutACookie(t *testing.T) {
	_, _, handler := guardFor(t)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/properties", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestRequireSessionClearsAStaleCookie(t *testing.T) {
	// A browser holding a cookie that names nothing should stop sending it.
	_, _, handler := guardFor(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/properties", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: "long gone"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("the stale session cookie was not cleared")
	}
}

func TestRequireSessionAdmitsALiveSession(t *testing.T) {
	guard, repo, handler := guardFor(t)
	token, _ := signIn(t, guard, repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/properties", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: token})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "alice" {
		t.Errorf("handler saw %q, want alice", rec.Body.String())
	}
}

func TestRequireSessionEnforcesCSRFOnMutations(t *testing.T) {
	guard, repo, handler := guardFor(t)
	token, csrf := signIn(t, guard, repo)

	tests := []struct {
		name   string
		method string
		header string
		want   int
	}{
		{"a read needs no token", http.MethodGet, "", http.StatusOK},
		{"a write with the right token passes", http.MethodPost, csrf, http.StatusOK},
		{"a write with no token is refused", http.MethodPost, "", http.StatusForbidden},
		{"a write with a wrong token is refused", http.MethodPost, "deadbeef", http.StatusForbidden},
		{"delete is guarded too", http.MethodDelete, "", http.StatusForbidden},
		{"patch is guarded too", http.MethodPatch, "", http.StatusForbidden},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, "/api/v1/properties", nil)
		req.AddCookie(&http.Cookie{Name: SessionCookie, Value: token})
		if tt.header != "" {
			req.Header.Set(CSRFHeader, tt.header)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != tt.want {
			t.Errorf("%s: status = %d, want %d", tt.name, rec.Code, tt.want)
		}
	}
}

func TestSetCookiesMarksTheSessionHttpOnlyAndTheTokenNot(t *testing.T) {
	guard, repo, _ := guardFor(t)
	token, _ := signIn(t, guard, repo)

	rec := httptest.NewRecorder()
	guard.SetCookies(rec, token, time.Now().Add(time.Hour))

	got := map[string]*http.Cookie{}
	for _, c := range rec.Result().Cookies() {
		got[c.Name] = c
	}

	session, ok := got[SessionCookie]
	if !ok {
		t.Fatal("no session cookie was set")
	}
	if !session.HttpOnly {
		t.Error("the session cookie is not HttpOnly, so script can read it")
	}
	if session.SameSite != http.SameSiteLaxMode {
		t.Errorf("session SameSite = %v, want Lax", session.SameSite)
	}

	csrf, ok := got[CSRFCookie]
	if !ok {
		t.Fatal("no CSRF cookie was set")
	}
	if csrf.HttpOnly {
		t.Error("the CSRF cookie is HttpOnly, so the frontend cannot echo it")
	}
	if csrf.Value == "" {
		t.Error("the CSRF cookie is empty")
	}
	if csrf.Value == session.Value {
		t.Error("the CSRF cookie repeats the session token")
	}
}

func TestClientIPIgnoresForwardedHeaders(t *testing.T) {
	// Trusting X-Forwarded-For would let one client spread its attempts across
	// a limitless set of rate-limit keys.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	req.RemoteAddr = "10.0.0.4:51000"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")

	if got := ClientIP(req); got != "10.0.0.4" {
		t.Errorf("ClientIP = %q, want 10.0.0.4", got)
	}
}
