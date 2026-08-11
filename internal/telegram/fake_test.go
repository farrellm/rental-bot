package telegram

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/farrellm/rental-bot/internal/jobs"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/migrations"
)

const testToken = "123456:test-token"

// fakeTelegram is a scripted Bot API, enough of it to drive a pairing and a
// delivery.
//
// The pipeline is worth testing against something that answers like the real
// API rather than against a mocked client interface: what can go wrong here is
// in the request the library builds, the offset it sends, and what this
// process does with a 429 — and a mock would assert that none of those exist.
type fakeTelegram struct {
	mu sync.Mutex

	// updates is what getUpdates hands out, in order, one batch per call.
	updates [][]map[string]any
	// sent records every sendMessage.
	sent []sentMessage
	// failSends makes sendMessage answer 502 until it is cleared, so a test
	// can take the channel down.
	failSends bool

	server *httptest.Server
}

type sentMessage struct {
	ChatID int64
	Text   string
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	t.Helper()
	f := &fakeTelegram{}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeTelegram) URL() string { return f.server.URL }

func (f *fakeTelegram) serve(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path
	if i := strings.LastIndex(method, "/"); i >= 0 {
		method = method[i+1:]
	}

	switch method {
	case "getUpdates":
		f.serveGetUpdates(w, r)
	case "sendMessage":
		f.serveSendMessage(w, r)
	case "getMe":
		writeResult(w, map[string]any{"id": 1, "is_bot": true, "username": "rental_records_bot"})
	default:
		http.Error(w, "unrouted: "+method, http.StatusNotFound)
	}
}

// serveGetUpdates hands out one scripted batch per call, then holds the poll
// open the way Telegram does rather than spinning.
func (f *fakeTelegram) serveGetUpdates(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	var batch []map[string]any
	if len(f.updates) > 0 {
		batch, f.updates = f.updates[0], f.updates[1:]
	}
	f.mu.Unlock()

	if batch == nil {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
		batch = []map[string]any{}
	}
	writeResult(w, batch)
}

func (f *fakeTelegram) serveSendMessage(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	failing := f.failSends
	f.mu.Unlock()

	if failing {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}

	if err := r.ParseMultipartForm(1 << 20); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	var chatID int64
	if _, err := jsonNumber(r.FormValue("chat_id"), &chatID); err != nil {
		http.Error(w, "bad chat_id", http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	f.sent = append(f.sent, sentMessage{ChatID: chatID, Text: r.FormValue("text")})
	f.mu.Unlock()

	writeResult(w, map[string]any{"message_id": 1, "date": time.Now().Unix(),
		"chat": map[string]any{"id": chatID, "type": "private"}})
}

// queueUpdate scripts one inbound message.
func (f *fakeTelegram) queueUpdate(id, chatID int64, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, []map[string]any{{
		"update_id": id,
		"message": map[string]any{
			"message_id": id,
			"date":       time.Now().Unix(),
			"chat":       map[string]any{"id": chatID, "type": "private"},
			"text":       text,
		},
	}})
}

func (f *fakeTelegram) sentMessages() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMessage(nil), f.sent...)
}

func (f *fakeTelegram) setFailSends(failing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failSends = failing
}

func writeResult(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func jsonNumber(s string, dst *int64) (bool, error) {
	if s == "" {
		return false, nil
	}
	return true, json.Unmarshal([]byte(s), dst)
}

// harness is the channel over a real database, a real queue, and a real spool.
type harness struct {
	fake   *fakeTelegram
	repo   *store.Repo
	store  *Store
	queue  *jobs.Queue
	spool  *Spool
	sender *Sender
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()

	db, err := store.Open(t.Context(), filepath.Join(dir, "rental.db"), 2)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(t.Context(), migrations.FS); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	repo := db.Repo()

	spool, err := NewSpool(filepath.Join(dir, "spool"))
	if err != nil {
		t.Fatalf("NewSpool: %v", err)
	}

	fake := newFakeTelegram(t)
	client, err := NewClient(testToken, fake.URL())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	st := NewStore(repo, 10*time.Minute)
	sender := NewSender(st, client, jobs.NewQueue(repo), nil, spool, SenderOptions{
		BaseURL: "https://rental.example.com",
		Logger:  quiet(),
	})
	// The spacing is a second in production and would make every test that
	// sends twice take two seconds. TestTheRateLimitSpacesMessages puts it
	// back.
	sender.spacing = 0

	return &harness{
		fake: fake, repo: repo, store: st,
		queue: jobs.NewQueue(repo), spool: spool, sender: sender,
	}
}

// pair puts a chat on file without going through the poller.
func (h *harness) pair(t *testing.T, chatID int64) {
	t.Helper()
	code, _, err := h.store.IssuePairingCode(t.Context())
	if err != nil {
		t.Fatalf("IssuePairingCode: %v", err)
	}
	if err := h.store.Pair(t.Context(), code, chatID); err != nil {
		t.Fatalf("Pair: %v", err)
	}
}

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}
