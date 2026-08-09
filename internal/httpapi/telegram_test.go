package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/farrellm/rental-bot/internal/alert"
	"github.com/farrellm/rental-bot/internal/config"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
	"github.com/farrellm/rental-bot/internal/telegram"
)

// channelServer wires a server with an alert channel. The channel stays
// unconfigured unless a test says otherwise: that is the state a fresh clone is
// in, and it has to be a working one.
func channelServer(t *testing.T, configured bool) (http.Handler, Options, func(method, target string, body any) *http.Request) {
	t.Helper()

	opts := Options{}
	if configured {
		opts.Config.Telegram = config.Default().Telegram
		opts.Config.Telegram.BotUsername = "rental_records_bot"
		opts.Config.Secrets.TelegramBotToken = "123:abc"
	}

	opts, request := authed(t, opts)
	if configured {
		opts.Telegram = telegram.NewStore(opts.Repo, 10*time.Minute)
	}

	return New(opts), opts, func(method, target string, body any) *http.Request {
		if body == nil {
			return request(method, target, nil)
		}
		return request(method, target, jsonBody(t, body))
	}
}

func TestChannelStandingWithoutConfiguration(t *testing.T) {
	h, _, request := channelServer(t, false)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(http.MethodGet, "/api/v1/telegram", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: an unconfigured channel is a working state", rec.Code)
	}

	var out channelStanding
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Configured || out.Paired {
		t.Errorf("standing = %+v, want neither configured nor paired", out)
	}
	if out.State != "not-configured" {
		t.Errorf("state = %q, want not-configured", out.State)
	}
	// An empty screen that names the keys to fill beats one that says only
	// "not configured".
	if !containsString(out.Missing, "telegram.bot_username") {
		t.Errorf("missing = %v, want it to name telegram.bot_username", out.Missing)
	}
	if !containsString(out.Missing, "RENTAL_BOT_TELEGRAM_BOT_TOKEN") {
		t.Errorf("missing = %v, want it to name the token variable", out.Missing)
	}
}

// The routes that need a bot say so rather than pretending.
func TestChannelRoutesWithoutConfiguration(t *testing.T) {
	h, _, request := channelServer(t, false)

	for _, path := range []string{"/api/v1/telegram/pairing-code", "/api/v1/telegram/test"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, request(http.MethodPost, path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("POST %s = %d, want 503", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "telegram.bot_username") {
			t.Errorf("POST %s did not say which key to set: %s", path, rec.Body)
		}
	}
}

func TestChannelRoutesNeedASession(t *testing.T) {
	h, _, _ := channelServer(t, true)

	for _, tt := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/telegram"},
		{http.MethodPost, "/api/v1/telegram/pairing-code"},
		{http.MethodPost, "/api/v1/telegram/test"},
		{http.MethodGet, "/api/v1/notifications"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a session = %d, want 401", tt.method, tt.path, rec.Code)
		}
	}
}

func TestIssuingAPairingCode(t *testing.T) {
	h, _, request := channelServer(t, true)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(http.MethodPost, "/api/v1/telegram/pairing-code", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body)
	}

	var out pairingCodeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Code == "" {
		t.Fatal("no code was returned")
	}
	if out.Send != "/start "+out.Code {
		t.Errorf("send = %q, want the exact line to copy", out.Send)
	}
	if out.BotUsername != "rental_records_bot" {
		t.Errorf("bot_username = %q", out.BotUsername)
	}
	// A code in a cache is a code somebody else can read.
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	// The standing carries the expiry and never the code: only the hash is on
	// file, so this is the only response that could have shown it.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, request(http.MethodGet, "/api/v1/telegram", nil))
	var standing channelStanding
	if err := json.Unmarshal(rec.Body.Bytes(), &standing); err != nil {
		t.Fatal(err)
	}
	if standing.PairingCode != "" {
		t.Error("the standing carries the pairing code; only its hash is stored")
	}
	if standing.PairingExpiresAt == "" {
		t.Error("the standing does not say when the code lapses")
	}
	if standing.State != "not-connected" {
		t.Errorf("state = %q, want not-connected while a code is outstanding", standing.State)
	}
}

// §8.2 puts re-pairing behind server access. An endpoint that could mint a
// code for a paired bot would let a hijacked session move the alert channel to
// the attacker's own chat -- and that channel is what would report the hijack.
func TestAPairedChannelRefusesANewCode(t *testing.T) {
	h, opts, request := channelServer(t, true)
	pairChat(t, opts, 4471)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(http.MethodPost, "/api/v1/telegram/pairing-code", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "-unpair-telegram") {
		t.Errorf("the refusal does not say how to actually do it: %s", rec.Body)
	}
}

func TestStandingOnAPairedChannel(t *testing.T) {
	h, opts, request := channelServer(t, true)
	pairChat(t, opts, 4471)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(http.MethodGet, "/api/v1/telegram", nil))
	var out channelStanding
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Configured || !out.Paired {
		t.Fatalf("standing = %+v, want configured and paired", out)
	}
	if out.State != "paired" {
		t.Errorf("state = %q, want paired", out.State)
	}
	if out.ChatID == nil || *out.ChatID != 4471 {
		t.Errorf("chat_id = %v, want 4471", out.ChatID)
	}
	if out.CooldownSeconds == 0 {
		t.Error("the standing does not say how long a condition stays quiet")
	}
}

func TestTestNoticeNeedsAPairedChat(t *testing.T) {
	h, _, request := channelServer(t, true)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(http.MethodPost, "/api/v1/telegram/test", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "pairing code") {
		t.Errorf("the refusal does not name the next move: %s", rec.Body)
	}
}

func TestTestNoticeGoesThroughTheBus(t *testing.T) {
	opts := Options{}
	opts.Config.Telegram = config.Default().Telegram
	opts.Config.Telegram.BotUsername = "rental_records_bot"
	opts.Config.Secrets.TelegramBotToken = "123:abc"

	opts, request := authed(t, opts)
	opts.Telegram = telegram.NewStore(opts.Repo, 10*time.Minute)
	raised := &recordingPublisher{}
	opts.Alerts = raised
	pairChat(t, opts, 4471)

	h := New(opts)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(http.MethodPost, "/api/v1/telegram/test", jsonBody(t, nil)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body)
	}

	if len(raised.alerts) != 1 {
		t.Fatalf("raised %d alerts, want 1", len(raised.alerts))
	}
	if raised.alerts[0].Severity != alert.Info {
		t.Errorf("severity = %q, want info", raised.alerts[0].Severity)
	}
	// Resolved straight away, so the register does not carry a test as an open
	// condition forever.
	if len(raised.resolved) != 1 || raised.resolved[0] != raised.alerts[0].Key {
		t.Errorf("resolved = %v, want the key just raised", raised.resolved)
	}
}

// The register works with no bot at all, because the log sink writes to it
// either way. That is what lets the operator see what would have been sent.
func TestTheRegisterListsNoticesNewestFirst(t *testing.T) {
	h, opts, request := channelServer(t, false)

	for i, title := range []string{"oldest", "middle", "newest"} {
		at := time.Date(2026, 8, 9, 12, i, 0, 0, time.UTC).Format(time.RFC3339)
		if _, err := opts.Repo.Write().InsertNotification(t.Context(), sqlc.InsertNotificationParams{
			DedupeKey: "k." + title, Channel: "log", Severity: "warning", Title: title,
			FirstSeenAt: at, CreatedAt: at, UpdatedAt: at,
		}); err != nil {
			t.Fatalf("InsertNotification: %v", err)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(http.MethodGet, "/api/v1/notifications", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}

	var out noticeList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 3 {
		t.Fatalf("got %d notices, want 3", len(out.Items))
	}
	if out.Items[0].Title != "newest" {
		t.Errorf("first item = %q, want the newest", out.Items[0].Title)
	}

	// And the tally on the standing counts them, bot or no bot.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, request(http.MethodGet, "/api/v1/telegram", nil))
	var standing channelStanding
	if err := json.Unmarshal(rec.Body.Bytes(), &standing); err != nil {
		t.Fatal(err)
	}
	if standing.Sent != 3 || standing.Cleared != 0 {
		t.Errorf("tally = %d sent, %d cleared; want 3 and 0", standing.Sent, standing.Cleared)
	}
}

func TestTheRegisterPages(t *testing.T) {
	h, opts, request := channelServer(t, false)

	for i := range 4 {
		at := time.Date(2026, 8, 9, 12, i, 0, 0, time.UTC).Format(time.RFC3339)
		if _, err := opts.Repo.Write().InsertNotification(t.Context(), sqlc.InsertNotificationParams{
			DedupeKey: "k." + itoa(int64(i)), Channel: "log", Severity: "info", Title: "condition",
			FirstSeenAt: at, CreatedAt: at, UpdatedAt: at,
		}); err != nil {
			t.Fatalf("InsertNotification: %v", err)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(http.MethodGet, "/api/v1/notifications?limit=2", nil))
	var first noticeList
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("first page = %d items, cursor %q; want 2 and a cursor", len(first.Items), first.NextCursor)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, request(http.MethodGet, "/api/v1/notifications?limit=2&cursor="+first.NextCursor, nil))
	var second noticeList
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 2 {
		t.Fatalf("second page = %d items, want 2", len(second.Items))
	}
	if second.Items[0].ID == first.Items[0].ID {
		t.Error("the second page repeats the first")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, request(http.MethodGet, "/api/v1/notifications?cursor=not-a-cursor", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a forged cursor = %d, want 400", rec.Code)
	}
}

// A restated condition is one line with a tally, not two lines. The register
// renders that mark, so the API has to carry it.
func TestARestatedConditionIsOneLine(t *testing.T) {
	h, opts, request := channelServer(t, false)
	ctx := t.Context()

	at := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	row, err := opts.Repo.Write().InsertNotification(ctx, sqlc.InsertNotificationParams{
		DedupeKey: "gmail.watch.lapsed", Channel: "log", Severity: "warning",
		Title: "The Gmail watch has lapsed", FirstSeenAt: at, CreatedAt: at, UpdatedAt: at,
	})
	if err != nil {
		t.Fatalf("InsertNotification: %v", err)
	}
	for range 4 {
		if err := opts.Repo.Write().RecordNotificationSent(ctx, sqlc.RecordNotificationSentParams{
			LastSentAt: at, Severity: "warning", Title: "The Gmail watch has lapsed",
			UpdatedAt: at, ID: row.ID,
		}); err != nil {
			t.Fatalf("RecordNotificationSent: %v", err)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(http.MethodGet, "/api/v1/notifications", nil))
	var out noticeList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("got %d notices, want 1", len(out.Items))
	}
	if out.Items[0].SendCount != 4 {
		t.Errorf("send_count = %d, want 4", out.Items[0].SendCount)
	}
	if out.Items[0].FirstSeenAt != at {
		t.Errorf("first_seen_at = %q, want the first sighting %q", out.Items[0].FirstSeenAt, at)
	}
}

// Not configured is not a fault: /readyz answering 503 over a subsystem nobody
// asked for is a check that teaches its reader to ignore it.
func TestReadyzIsHealthyWithoutAChannel(t *testing.T) {
	rec := serve(t, Options{DB: healthyDB()}, http.MethodGet, "/readyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}

	var body struct {
		Checks []Check `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, c := range body.Checks {
		if c.Name != "telegram" {
			continue
		}
		if !c.OK {
			t.Errorf("the telegram check failed with no bot configured: %s", c.Detail)
		}
		if !strings.Contains(c.Detail, "telegram.bot_username") {
			t.Errorf("the telegram check does not say which key to set: %s", c.Detail)
		}
		return
	}
	t.Error("/readyz reports nothing about the alert channel")
}

// pairChat puts a chat on file, the way the poller would.
func pairChat(t *testing.T, opts Options, chatID int64) {
	t.Helper()
	code, _, err := opts.Telegram.IssuePairingCode(t.Context())
	if err != nil {
		t.Fatalf("IssuePairingCode: %v", err)
	}
	if err := opts.Telegram.Pair(t.Context(), code, chatID); err != nil {
		t.Fatalf("Pair: %v", err)
	}
}
