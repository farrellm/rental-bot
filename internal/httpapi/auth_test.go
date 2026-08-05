package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/farrellm/rental-bot/internal/auth"
)

// login posts credentials and returns the recorder.
func login(t *testing.T, opts Options, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		jsonBody(t, map[string]string{"username": username, "password": password}))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	New(opts).ServeHTTP(rec, req)
	return rec
}

func TestLoginSetsBothCookies(t *testing.T) {
	opts, _ := authed(t, Options{})
	rec := login(t, opts, "alice", testPassword)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}

	var user userResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &user); err != nil {
		t.Fatal(err)
	}
	if user.Username != "alice" {
		t.Errorf("Username = %q, want alice", user.Username)
	}
	// The response carries the operator, never their secrets.
	if strings.Contains(rec.Body.String(), "argon2") || strings.Contains(rec.Body.String(), "password") {
		t.Errorf("the login response leaks credential material: %s", rec.Body)
	}

	got := map[string]*http.Cookie{}
	for _, c := range rec.Result().Cookies() {
		got[c.Name] = c
	}
	session, ok := got[auth.SessionCookie]
	if !ok || session.Value == "" {
		t.Fatal("no session cookie was set")
	}
	if !session.HttpOnly {
		t.Error("the session cookie is not HttpOnly")
	}
	csrf, ok := got[auth.CSRFCookie]
	if !ok || csrf.Value == "" {
		t.Fatal("no CSRF cookie was set")
	}
	if csrf.HttpOnly {
		t.Error("the CSRF cookie is HttpOnly, so the frontend cannot echo it")
	}
}

func TestLoginRefusesBadCredentialsIdentically(t *testing.T) {
	// An unknown username and a wrong password have to be indistinguishable,
	// or the form becomes an oracle for which accounts exist.
	opts, _ := authed(t, Options{})

	wrongPassword := login(t, opts, "alice", "not the password")
	unknownUser := login(t, opts, "mallory", testPassword)

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"wrong password": wrongPassword,
		"unknown user":   unknownUser,
	} {
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, rec.Code)
		}
		if len(rec.Result().Cookies()) != 0 {
			t.Errorf("%s: a refused sign-in set cookies", name)
		}
	}

	var a, b Problem
	if err := json.Unmarshal(wrongPassword.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(unknownUser.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if a.Detail != b.Detail {
		t.Errorf("the two failures read differently: %q vs %q", a.Detail, b.Detail)
	}
	if !strings.Contains(a.Detail, "incorrect") {
		t.Errorf("detail = %q, want it to say the credentials are incorrect", a.Detail)
	}
}

func TestLoginRejectsAnEmptyForm(t *testing.T) {
	opts, _ := authed(t, Options{})
	for _, tt := range []struct{ user, pass string }{
		{"", testPassword}, {"alice", ""}, {"   ", testPassword},
	} {
		rec := login(t, opts, tt.user, tt.pass)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("login(%q, %q) = %d, want 400", tt.user, tt.pass, rec.Code)
		}
	}
}

func TestLoginRateLimits(t *testing.T) {
	opts, _ := authed(t, Options{})
	limiter := auth.NewLimiter()
	opts.Limiter = limiter
	handler := New(opts)

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
			jsonBody(t, map[string]string{"username": "alice", "password": "wrong"}))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	var limited *httptest.ResponseRecorder
	for range 10 {
		rec := post()
		if rec.Code == http.StatusTooManyRequests {
			limited = rec
			break
		}
	}
	if limited == nil {
		t.Fatal("ten wrong passwords in a row were never rate limited")
	}
	if limited.Header().Get("Retry-After") == "" {
		// The client has to be able to say how long, not just "later".
		t.Error("the 429 carries no Retry-After")
	}
	if !strings.Contains(limited.Body.String(), "Try again in") {
		t.Errorf("the 429 does not say how long to wait: %s", limited.Body)
	}
}

func TestLoginResetsTheLimitOnSuccess(t *testing.T) {
	opts, _ := authed(t, Options{})
	handler := New(opts)

	attempt := func(password string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
			jsonBody(t, map[string]string{"username": "alice", "password": password}))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	// Two typos then the right password: an operator who fumbles twice is not
	// locked out of their own house.
	attempt("wrong")
	attempt("wrong")
	if code := attempt(testPassword); code != http.StatusOK {
		t.Fatalf("sign-in after two typos = %d, want 200", code)
	}
	if code := attempt(testPassword); code != http.StatusOK {
		t.Errorf("sign-in after a success = %d, want 200", code)
	}
}

func TestMeReturnsTheSignedInUser(t *testing.T) {
	opts, request := authed(t, Options{})

	rec := httptest.NewRecorder()
	New(opts).ServeHTTP(rec, request(http.MethodGet, "/api/v1/auth/me", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var user userResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &user); err != nil {
		t.Fatal(err)
	}
	if user.Username != "alice" {
		t.Errorf("Username = %q, want alice", user.Username)
	}

	// It reissues the CSRF cookie, so a restart that rotated the signing key
	// costs one request rather than a save that fails with no explanation.
	var reissued bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.CSRFCookie && c.Value != "" {
			reissued = true
		}
	}
	if !reissued {
		t.Error("GET /auth/me did not reissue the CSRF cookie")
	}
}

func TestMeNeedsASession(t *testing.T) {
	opts, _ := authed(t, Options{})
	rec := serve(t, opts, http.MethodGet, "/api/v1/auth/me")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	opts, request := authed(t, Options{})
	handler := New(opts)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request(http.MethodPost, "/api/v1/auth/logout", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body %s)", rec.Code, rec.Body)
	}

	var cleared int
	for _, c := range rec.Result().Cookies() {
		if (c.Name == auth.SessionCookie || c.Name == auth.CSRFCookie) && c.MaxAge < 0 {
			cleared++
		}
	}
	if cleared != 2 {
		t.Errorf("%d cookies were cleared, want 2", cleared)
	}

	// The same cookie no longer opens anything.
	after := httptest.NewRecorder()
	handler.ServeHTTP(after, request(http.MethodGet, "/api/v1/auth/me", nil))
	if after.Code != http.StatusUnauthorized {
		t.Errorf("the session still worked after logout: %d", after.Code)
	}
}

func TestLogoutNeedsACSRFToken(t *testing.T) {
	// Signing someone out across origins is a nuisance rather than a breach,
	// but it is still a state change and it goes through the same gate.
	opts, request := authed(t, Options{})

	req := request(http.MethodPost, "/api/v1/auth/logout", nil)
	req.Header.Del(auth.CSRFHeader)

	rec := httptest.NewRecorder()
	New(opts).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestLoginRejectsUnknownFields(t *testing.T) {
	// A client that sends "usernmae" has a bug, and quietly ignoring it would
	// look like a wrong password.
	opts, _ := authed(t, Options{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"username":"alice","password":"x","remember":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	New(opts).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "remember") {
		t.Errorf("the error does not name the offending field: %s", rec.Body)
	}
}

func TestGuardedRoutesFailClosedWithoutAGuard(t *testing.T) {
	// Misconfiguration must not open the API. A server built without a guard
	// refuses rather than serving the ledger to anyone who asks.
	rec := serve(t, Options{DB: healthyDB()}, http.MethodGet, "/api/v1/status")

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestMethodNotAllowedIsProblemJSONEverywhere(t *testing.T) {
	opts, _ := authed(t, Options{})
	handler := New(opts)

	tests := []struct {
		method, target, allow string
	}{
		{http.MethodGet, "/api/v1/auth/login", "POST"},
		{http.MethodDelete, "/api/v1/auth/me", "GET, HEAD"},
		{http.MethodPut, "/api/v1/status", "GET, HEAD"},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.target, nil))

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", tt.method, tt.target, rec.Code)
			continue
		}
		if got := rec.Header().Get("Allow"); got != tt.allow {
			t.Errorf("%s %s: Allow = %q, want %q", tt.method, tt.target, got, tt.allow)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
			t.Errorf("%s %s: Content-Type = %q, want problem+json", tt.method, tt.target, ct)
		}
	}
}
