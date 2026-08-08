package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/farrellm/rental-bot/internal/config"
	"github.com/farrellm/rental-bot/internal/gmail"
	"github.com/farrellm/rental-bot/internal/jobs"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// intakeServer wires a server with the queue and archive the intake routes
// need. Ingestion itself stays unconfigured unless a test says otherwise:
// that is the state a fresh clone is in, and it has to be a working one.
func intakeServer(t *testing.T, opts Options) (http.Handler, Options, func(method, target string, body any) *http.Request) {
	t.Helper()

	opts, request := authed(t, opts)
	if opts.Queue == nil {
		opts.Queue = jobs.NewQueue(opts.Repo)
	}
	if opts.Archive == nil {
		archive, err := gmail.NewArchive(filepath.Join(t.TempDir(), "raw-email"))
		if err != nil {
			t.Fatalf("NewArchive: %v", err)
		}
		opts.Archive = archive
	}

	return New(opts), opts, func(method, target string, body any) *http.Request {
		if body == nil {
			return request(method, target, nil)
		}
		return request(method, target, jsonBody(t, body))
	}
}

func TestIntakeStandingWithoutConfiguration(t *testing.T) {
	h, _, request := intakeServer(t, Options{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(http.MethodGet, "/api/v1/gmail", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: an unconfigured mailbox is a working state", rec.Code)
	}

	var out intakeStanding
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Configured || out.Connected {
		t.Errorf("configured = %v, connected = %v; want both false", out.Configured, out.Connected)
	}
	if out.State != "not-configured" {
		t.Errorf("state = %q, want not-configured", out.State)
	}
	// An empty screen that names the keys to fill beats one that says only
	// "not configured".
	if len(out.Missing) == 0 {
		t.Error("the standing names no missing configuration keys")
	}
	if !containsString(out.Missing, "gmail.client_id") {
		t.Errorf("missing = %v, want it to name gmail.client_id", out.Missing)
	}
}

// The routes that need a mailbox say so rather than pretending.
func TestGmailRoutesWithoutConfiguration(t *testing.T) {
	h, _, request := intakeServer(t, Options{})

	for _, tt := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/gmail/connect"},
		{http.MethodPost, "/api/v1/gmail/sync"},
		{http.MethodDelete, "/api/v1/gmail"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, request(tt.method, tt.path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503", tt.method, tt.path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "gmail.client_id") {
			t.Errorf("%s %s did not say which key to set: %s", tt.method, tt.path, rec.Body.String())
		}
	}
}

// Failing open here would put an unauthenticated enqueue endpoint on the
// public internet.
func TestPushWithoutAVerifierIsRefused(t *testing.T) {
	h, _, _ := intakeServer(t, Options{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhooks/gmail",
		strings.NewReader(`{"message":{"messageId":"1"}}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestPushWithoutATokenIsRefused(t *testing.T) {
	h, _, _ := intakeServer(t, Options{
		PushVerifier: gmail.NewVerifier("https://rental.example.com/webhooks/gmail", "push@example.iam.gserviceaccount.com"),
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhooks/gmail",
		strings.NewReader(`{"message":{"messageId":"1"}}`)))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	// The body says nothing about which check failed: a verifier that explains
	// itself is a verifier that helps whoever is probing it.
	if strings.Contains(rec.Body.String(), "bearer") || strings.Contains(rec.Body.String(), "audience") {
		t.Errorf("the refusal explains itself: %s", rec.Body.String())
	}
}

// The push endpoint needs no session: Pub/Sub has no cookie, and the OIDC
// token is the whole of its authorization.
func TestPushIsOutsideTheSession(t *testing.T) {
	h, _, _ := intakeServer(t, Options{
		PushVerifier: gmail.NewVerifier("https://rental.example.com/webhooks/gmail", "push@example.iam.gserviceaccount.com"),
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhooks/gmail", strings.NewReader("{}")))

	// 401 from the verifier, not from the session middleware. The distinction
	// is visible in the detail.
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "Sign in") {
		t.Error("the push endpoint went through the session middleware")
	}
}

func TestPushRejectsOtherMethods(t *testing.T) {
	h, _, _ := intakeServer(t, Options{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/webhooks/gmail", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/problem+json") {
		t.Errorf("content type = %q, want problem+json", got)
	}
}

func TestRegisterListsMessagesNewestFirst(t *testing.T) {
	h, opts, request := intakeServer(t, Options{})
	ctx := t.Context()

	for i, at := range []string{
		"2026-08-01T09:00:00Z",
		"2026-08-06T14:02:00Z",
		"2026-08-03T11:30:00Z",
	} {
		msg, err := opts.Repo.Write().CreateEmailMessage(ctx, sqlc.CreateEmailMessageParams{
			GmailMessageID: "m" + string(rune('1'+i)),
			FromAddr:       "me@example.com",
			Subject:        "Receipt " + string(rune('1'+i)),
			ReceivedAt:     at,
			RawPath:        "2026/08/m.eml",
			Status:         "received",
			CreatedAt:      at, UpdatedAt: at,
		})
		if err != nil {
			t.Fatalf("CreateEmailMessage: %v", err)
		}
		if _, err := opts.Repo.Write().CreateEmailAttachment(ctx, sqlc.CreateEmailAttachmentParams{
			EmailMessageID: msg.ID, PartID: "2", Filename: "receipt.pdf",
			Mime: "application/pdf", SizeBytes: 1024,
			CreatedAt: at, UpdatedAt: at,
		}); err != nil {
			t.Fatalf("CreateEmailAttachment: %v", err)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(http.MethodGet, "/api/v1/email-messages", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var out emailMessageList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 3 {
		t.Fatalf("returned %d messages, want 3", len(out.Items))
	}
	if out.Items[0].ReceivedAt != "2026-08-06T14:02:00Z" {
		t.Errorf("first item received at %q, want the newest", out.Items[0].ReceivedAt)
	}
	// The register shows an enclosure count on every line, so the list carries
	// attachments rather than needing a request per line.
	if len(out.Items[0].Attachments) != 1 {
		t.Errorf("the list item carries %d attachments, want 1", len(out.Items[0].Attachments))
	}
}

func TestRegisterPages(t *testing.T) {
	h, opts, request := intakeServer(t, Options{})
	ctx := t.Context()

	for i := range 3 {
		at := time.Date(2026, 8, 1+i, 9, 0, 0, 0, time.UTC).Format(time.RFC3339)
		if _, err := opts.Repo.Write().CreateEmailMessage(ctx, sqlc.CreateEmailMessageParams{
			GmailMessageID: "m" + string(rune('1'+i)),
			ReceivedAt:     at, Status: "received", CreatedAt: at, UpdatedAt: at,
		}); err != nil {
			t.Fatalf("CreateEmailMessage: %v", err)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(http.MethodGet, "/api/v1/email-messages?limit=2", nil))

	var first emailMessageList
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("first page: %d items, cursor %q", len(first.Items), first.NextCursor)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, request(http.MethodGet, "/api/v1/email-messages?limit=2&cursor="+first.NextCursor, nil))

	var second emailMessageList
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 {
		t.Errorf("second page: %d items, want 1", len(second.Items))
	}
	if len(second.Items) > 0 && second.Items[0].ID == first.Items[0].ID {
		t.Error("the second page repeats the first")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, request(http.MethodGet, "/api/v1/email-messages?cursor=not-a-cursor", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a forged cursor = %d, want 400", rec.Code)
	}
}

// An .eml is HTML plus whatever the sender put in it, and this origin holds the
// operator's session.
func TestRawMessageIsServedAsAnAttachment(t *testing.T) {
	h, opts, request := intakeServer(t, Options{})
	ctx := t.Context()

	raw := []byte("From: me@example.com\r\nSubject: Receipt\r\n\r\nbody")
	path, err := opts.Archive.Put("m1", time.Now(), raw)
	if err != nil {
		t.Fatalf("archive.Put: %v", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	msg, err := opts.Repo.Write().CreateEmailMessage(ctx, sqlc.CreateEmailMessageParams{
		GmailMessageID: "m1", ReceivedAt: now, RawPath: path,
		Status: "received", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateEmailMessage: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(http.MethodGet, "/api/v1/email-messages/"+itoa(msg.ID)+"/raw", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if rec.Body.String() != string(raw) {
		t.Error("the served bytes are not the archived ones")
	}

	for header, want := range map[string]string{
		"Content-Disposition":     "attachment",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'; sandbox",
	} {
		if got := rec.Header().Get(header); !strings.Contains(got, want) {
			t.Errorf("%s = %q, want it to contain %q", header, got, want)
		}
	}
}

// A message that was never downloaded has no original, and says so rather than
// producing an empty file.
func TestRawMessageWithoutAnArchive(t *testing.T) {
	h, opts, request := intakeServer(t, Options{})

	now := time.Now().UTC().Format(time.RFC3339)
	msg, err := opts.Repo.Write().CreateEmailMessage(t.Context(), sqlc.CreateEmailMessageParams{
		GmailMessageID: "m1", ReceivedAt: now, Status: "failed",
		Error: "past the size cap", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateEmailMessage: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(http.MethodGet, "/api/v1/email-messages/"+itoa(msg.ID)+"/raw", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "never downloaded") {
		t.Errorf("the 404 does not say why: %s", rec.Body.String())
	}
}

func TestIntakeRoutesNeedASession(t *testing.T) {
	h, _, _ := intakeServer(t, Options{})

	for _, path := range []string{
		"/api/v1/gmail",
		"/api/v1/email-messages",
		"/api/v1/email-messages/1",
		"/api/v1/email-messages/1/raw",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a session = %d, want 401", path, rec.Code)
		}
	}
}

// Not configured is not a fault: /readyz answering 503 over a subsystem nobody
// asked for is a check that teaches its reader to ignore it.
func TestReadyzIsHealthyWithoutIngestion(t *testing.T) {
	rec := serve(t, Options{DB: healthyDB()}, http.MethodGet, "/readyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var body struct {
		Checks []Check `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, c := range body.Checks {
		if c.Name == "gmail" {
			found = true
			if !c.OK {
				t.Errorf("the gmail check is failing when ingestion is not configured: %s", c.Detail)
			}
			if !strings.Contains(c.Detail, "gmail.client_id") {
				t.Errorf("the check does not say how to enable it: %q", c.Detail)
			}
		}
	}
	if !found {
		t.Error("/readyz reports no gmail check")
	}
}

func TestIntakeStandingCarriesTheConfiguredSenders(t *testing.T) {
	cfg := config.Default()
	cfg.Gmail.AllowedSenders = []string{"me@example.com", "office@example.com"}

	h, _, request := intakeServer(t, Options{Config: cfg})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(http.MethodGet, "/api/v1/gmail", nil))

	var out intakeStanding
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.AllowedSenders) != 2 {
		t.Errorf("allowed senders = %v, want the two configured", out.AllowedSenders)
	}
	if out.PollIntervalSeconds != int64(cfg.Gmail.PollInterval.Duration.Seconds()) {
		t.Errorf("poll interval = %d, want %v", out.PollIntervalSeconds, cfg.Gmail.PollInterval)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
