package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/farrellm/rental-bot/internal/alert"
	"github.com/farrellm/rental-bot/internal/auth"
	"github.com/farrellm/rental-bot/internal/blob"
	"github.com/farrellm/rental-bot/internal/config"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
	"github.com/farrellm/rental-bot/migrations"
)

// fakeDB stands in for the store so a test can make the database unwell.
type fakeDB struct {
	pingErr    error
	version    int
	versionErr error
	applied    []store.Migration
	appliedErr error
}

func (f fakeDB) Ping(context.Context) error { return f.pingErr }
func (f fakeDB) SchemaVersion(context.Context) (int, error) {
	return f.version, f.versionErr
}
func (f fakeDB) Applied(context.Context) ([]store.Migration, error) {
	return f.applied, f.appliedErr
}
func (f fakeDB) Path() string { return "/var/lib/rental-bot/rental.db" }

func healthyDB() fakeDB {
	return fakeDB{
		version: 1,
		applied: []store.Migration{{
			Version: 1, Name: "init", Checksum: "abc123", AppliedAt: "2026-08-04T21:00:00Z",
		}},
	}
}

func serve(t *testing.T, opts Options, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	h := New(opts)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

// authed returns options wired to a real database and guard, plus a request
// factory that carries a live session.
//
// The health endpoints still take a fake, because a fake can be made unwell.
// Anything behind the session middleware needs a real users table: faking the
// whole generated query surface would test the fake rather than the API.
func authed(t *testing.T, opts Options) (Options, func(method, target string, body io.Reader) *http.Request) {
	t.Helper()

	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "rental.db"), 2)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(t.Context(), migrations.FS); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	repo := db.Repo()
	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	user, err := repo.Write().UpsertUser(t.Context(), sqlc.UpsertUserParams{
		Username: "alice", PasswordHash: hash, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}

	guard := auth.NewGuard(auth.NewSessions(repo), auth.NewCSRF([]byte("test key")), false, WriteProblem)
	token, _, err := guard.Sessions().Issue(t.Context(), user.ID, "", "")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	csrf := auth.NewCSRF([]byte("test key")).Token(auth.HashToken(token))

	opts.Repo = repo
	opts.Guard = guard
	if opts.DB == nil {
		opts.DB = healthyDB()
	}
	// Documents need somewhere to land. A temp directory per test keeps the
	// content-addressed store real rather than faked -- dedupe and the
	// disposition allowlist are both about what actually lands on disk.
	if opts.Blobs == nil {
		blobs, err := blob.New(filepath.Join(t.TempDir(), "blobs"))
		if err != nil {
			t.Fatalf("blob.New: %v", err)
		}
		opts.Blobs = blobs
	}
	if opts.Config.Storage.MaxUploadBytes == 0 {
		opts.Config.Storage.MaxUploadBytes = config.Default().Storage.MaxUploadBytes
	}

	return opts, func(method, target string, body io.Reader) *http.Request {
		req := httptest.NewRequest(method, target, body)
		req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: token})
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if !auth.Safe(method) {
			req.Header.Set(auth.CSRFHeader, csrf)
		}
		return req
	}
}

const testPassword = "correct horse battery staple"

// jsonBody renders a request body.
func jsonBody(t *testing.T, v any) io.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(b)
}

func TestHealthzIgnoresTheDatabase(t *testing.T) {
	// Liveness must not follow readiness down: a database blip should not
	// convince an orchestrator to restart a perfectly alive process.
	rec := serve(t, Options{DB: fakeDB{pingErr: errors.New("disk on fire")}}, http.MethodGet, "/healthz")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q, want %q", body["status"], "ok")
	}
}

func TestReadyz(t *testing.T) {
	tests := []struct {
		name     string
		db       fakeDB
		want     int
		wantWord string
		wantText string
	}{
		{
			name:     "healthy",
			db:       healthyDB(),
			want:     http.StatusOK,
			wantWord: "operational",
		},
		{
			name:     "database unreachable",
			db:       fakeDB{pingErr: errors.New("unable to open database file")},
			want:     http.StatusServiceUnavailable,
			wantWord: "degraded",
			wantText: "writable by the service user",
		},
		{
			name:     "never migrated",
			db:       fakeDB{version: 0},
			want:     http.StatusServiceUnavailable,
			wantWord: "degraded",
			wantText: "make migrate",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(t, Options{DB: tt.db}, http.MethodGet, "/readyz")
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.want, rec.Body)
			}

			var body struct {
				Status string  `json:"status"`
				Checks []Check `json:"checks"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Status != tt.wantWord {
				t.Errorf("status = %q, want %q", body.Status, tt.wantWord)
			}
			if len(body.Checks) == 0 {
				t.Fatal("no checks reported")
			}
			if tt.wantText != "" && !strings.Contains(rec.Body.String(), tt.wantText) {
				// A failing check has to say what to do about it.
				t.Errorf("body does not mention %q: %s", tt.wantText, rec.Body)
			}
		})
	}
}

func TestStatusNeedsASession(t *testing.T) {
	// The M0 gap, closed: status exposes build identity and schema state, and
	// there is a session middleware to put it behind now.
	opts, _ := authed(t, Options{DB: healthyDB()})
	rec := serve(t, opts, http.MethodGet, "/api/v1/status")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

func TestHealthEndpointsStayOpen(t *testing.T) {
	// A process manager has no session, and a liveness probe that needs one
	// reports on the wrong thing.
	opts, _ := authed(t, Options{DB: healthyDB()})
	for _, path := range []string{"/healthz", "/readyz"} {
		if rec := serve(t, opts, http.MethodGet, path); rec.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200", path, rec.Code)
		}
	}
}

func TestStatus(t *testing.T) {
	started := time.Now().Add(-90 * time.Second)
	opts, request := authed(t, Options{DB: healthyDB(), StartedAt: started})

	rec := httptest.NewRecorder()
	New(opts).ServeHTTP(rec, request(http.MethodGet, "/api/v1/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	var st Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Status != "operational" {
		t.Errorf("Status = %q, want operational", st.Status)
	}
	if st.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1", st.SchemaVersion)
	}
	if st.UptimeSeconds < 89 || st.UptimeSeconds > 120 {
		t.Errorf("UptimeSeconds = %d, want about 90", st.UptimeSeconds)
	}
	if len(st.Migrations) != 1 || st.Migrations[0].Name != "init" {
		t.Errorf("Migrations = %+v, want the one recorded migration", st.Migrations)
	}
	if st.Version == "" || st.GoVersion == "" {
		t.Errorf("build identity is incomplete: %+v", st)
	}
}

func TestStatusStillAnswersWhenDegraded(t *testing.T) {
	// The card should keep showing build identity and uptime even when the
	// database is gone — that is exactly when the operator is looking.
	//
	// The session store is a separate database from the one being reported on,
	// so the operator can still sign in and read the bad news.
	opts, request := authed(t, Options{DB: fakeDB{
		pingErr:    errors.New("no such file"),
		versionErr: errors.New("no such table"),
		appliedErr: errors.New("no such table"),
	}})

	rec := httptest.NewRecorder()
	New(opts).ServeHTTP(rec, request(http.MethodGet, "/api/v1/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var st Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Status != "degraded" {
		t.Errorf("Status = %q, want degraded", st.Status)
	}
	if st.Version == "" {
		t.Error("Version is empty; the card has nothing to show")
	}
	if st.Migrations == nil {
		t.Error("Migrations is null; it should be an empty list")
	}
}

func TestUnknownAPIPathIsProblemJSON(t *testing.T) {
	rec := serve(t, Options{DB: healthyDB()}, http.MethodGet, "/api/v1/no-such-thing")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}

	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Status != http.StatusNotFound || p.Title == "" {
		t.Errorf("problem = %+v", p)
	}
	if p.Instance == "" {
		t.Error("problem carries no request ID, so it cannot be correlated with the logs")
	}
}

func TestWrongMethodIsProblemJSON(t *testing.T) {
	rec := serve(t, Options{DB: healthyDB()}, http.MethodPost, "/healthz")

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q, want %q", got, "GET, HEAD")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

func TestRequestIDIsEchoedAndPreserved(t *testing.T) {
	h := New(Options{DB: healthyDB()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Header().Get(requestIDHeader) == "" {
		t.Error("no request ID assigned")
	}

	// A proxy that already assigned one keeps it, so the two logs line up.
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set(requestIDHeader, "from-caddy")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get(requestIDHeader); got != "from-caddy" {
		t.Errorf("request ID = %q, want %q", got, "from-caddy")
	}
}

func TestSPAFallback(t *testing.T) {
	spa := fstest.MapFS{
		"index.html":            &fstest.MapFile{Data: []byte("<!doctype html><title>rental-bot</title>")},
		"assets/app-a1b2c3.js":  &fstest.MapFile{Data: []byte("console.log(1)")},
		"assets/app-a1b2c3.css": &fstest.MapFile{Data: []byte("body{}")},
	}
	opts := Options{DB: healthyDB(), SPA: spa}

	t.Run("root serves the shell", func(t *testing.T) {
		rec := serve(t, opts, http.MethodGet, "/")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "rental-bot") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}
	})

	t.Run("client-side route serves the shell", func(t *testing.T) {
		rec := serve(t, opts, http.MethodGet, "/properties/12/documents")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "rental-bot") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("fingerprinted assets are cached hard", func(t *testing.T) {
		rec := serve(t, opts, http.MethodGet, "/assets/app-a1b2c3.js")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
			t.Errorf("Cache-Control = %q, want an immutable directive", got)
		}
	})

	t.Run("API paths never fall through to the shell", func(t *testing.T) {
		rec := serve(t, opts, http.MethodGet, "/api/v1/nope")
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})
}

func TestWithoutSPAServesAnExplanation(t *testing.T) {
	rec := serve(t, Options{DB: healthyDB()}, http.MethodGet, "/")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "make build") {
		t.Errorf("placeholder does not say how to fix it: %s", rec.Body)
	}
}

func TestPanicBecomesAProblem(t *testing.T) {
	s := &server{log: slog.Default()}
	h := withRequestID(nil, s.withRecover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

// A recovered panic is the one class of failure that leaves no trace on any
// screen, so it is the one that has to be said out loud.
func TestPanicRaisesACriticalAlert(t *testing.T) {
	raised := &recordingPublisher{}
	s := &server{log: slog.Default(), alerts: raised}
	h := withRequestID(nil, s.withRecover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("nil map write")
	})))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/properties", nil))

	if len(raised.alerts) != 1 {
		t.Fatalf("raised %d alerts, want 1", len(raised.alerts))
	}
	if got := raised.alerts[0].Severity; got != alert.Critical {
		t.Errorf("severity = %q, want %q", got, alert.Critical)
	}
	if !strings.Contains(raised.alerts[0].Detail, "nil map write") {
		t.Errorf("detail = %q, want the panic value", raised.alerts[0].Detail)
	}
}

// Nothing in cmd builds a server without a bus, but a nil one is a working
// state and must not become a second panic inside the recover.
func TestPanicWithoutABusStillAnswers(t *testing.T) {
	s := &server{log: slog.Default()}
	h := withRequestID(nil, s.withRecover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// recordingPublisher stands in for the alert bus.
type recordingPublisher struct {
	alerts   []alert.Alert
	resolved []string
}

func (p *recordingPublisher) Publish(_ context.Context, a alert.Alert) {
	p.alerts = append(p.alerts, a)
}

func (p *recordingPublisher) Resolve(_ context.Context, key, _ string) {
	p.resolved = append(p.resolved, key)
}
