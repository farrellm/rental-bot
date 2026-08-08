package gmail

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/farrellm/rental-bot/internal/blob"
	"github.com/farrellm/rental-bot/internal/config"
	"github.com/farrellm/rental-bot/internal/secret"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/migrations"
)

// fakeGmail is a scripted Gmail, enough of it to drive a whole sync.
//
// The pipeline is worth testing against something that answers like the real
// API rather than against a mocked client interface: the bugs that matter here
// are in the walk, the base64url decoding, and the parse, and a mock would
// assert that none of those exist.
type fakeGmail struct {
	mu sync.Mutex

	// messages maps a message id to its raw RFC 822 bytes.
	messages map[string]fakeMessage
	// history is what ListHistory returns, keyed by the page token ("" first).
	history map[string]fakeHistoryPage
	// listed is what ListMessagesSince returns.
	listed []string

	profileHistoryID uint64
	// historyTooOld makes the next history call answer 404, which is what Gmail
	// does when the cursor predates what it keeps.
	historyTooOld bool

	// Recorded calls, for assertions.
	fetched  []string
	labelled map[string][]string
	watches  int
	server   *httptest.Server
}

type fakeMessage struct {
	threadID     string
	internalDate time.Time
	snippet      string
	raw          []byte
}

type fakeHistoryPage struct {
	messageIDs []string
	nextToken  string
	historyID  uint64
}

func newFakeGmail(t *testing.T) *fakeGmail {
	t.Helper()
	f := &fakeGmail{
		messages:         map[string]fakeMessage{},
		history:          map[string]fakeHistoryPage{},
		labelled:         map[string][]string{},
		profileHistoryID: 5000,
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeGmail) URL() string { return f.server.URL }

func (f *fakeGmail) addMessage(id string, msg fakeMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages[id] = msg
}

func (f *fakeGmail) setHistory(token string, page fakeHistoryPage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.history[token] = page
}

func (f *fakeGmail) fetchCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, fetched := range f.fetched {
		if fetched == id {
			n++
		}
	}
	return n
}

func (f *fakeGmail) labelsOn(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.labelled[id]...)
}

func (f *fakeGmail) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	path := r.URL.Path
	switch {
	case path == "/token":
		writeJSON(w, map[string]any{
			"access_token":  "access-token",
			"refresh_token": "refresh-token",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})

	case path == "/revoke":
		w.WriteHeader(http.StatusOK)

	case path == "/gmail/v1/users/me/profile":
		writeJSON(w, map[string]any{
			"emailAddress": "bot@example.com",
			"historyId":    strconv.FormatUint(f.profileHistoryID, 10),
		})

	case path == "/gmail/v1/users/me/watch":
		f.watches++
		writeJSON(w, map[string]any{
			"historyId":  strconv.FormatUint(f.profileHistoryID, 10),
			"expiration": strconv.FormatInt(time.Now().Add(7*24*time.Hour).UnixMilli(), 10),
		})

	case path == "/gmail/v1/users/me/stop":
		w.WriteHeader(http.StatusNoContent)

	case path == "/gmail/v1/users/me/history":
		if f.historyTooOld {
			w.WriteHeader(http.StatusNotFound)
			writeJSON(w, map[string]any{"error": map[string]any{
				"message": "Requested entity was not found.", "status": "NOT_FOUND",
			}})
			return
		}
		page := f.history[r.URL.Query().Get("pageToken")]
		records := []any{}
		for _, id := range page.messageIDs {
			records = append(records, map[string]any{
				"messagesAdded": []any{map[string]any{"message": map[string]any{"id": id}}},
			})
		}
		body := map[string]any{"history": records, "historyId": strconv.FormatUint(page.historyID, 10)}
		if page.nextToken != "" {
			body["nextPageToken"] = page.nextToken
		}
		writeJSON(w, body)

	case path == "/gmail/v1/users/me/messages":
		items := []any{}
		for _, id := range f.listed {
			items = append(items, map[string]string{"id": id})
		}
		writeJSON(w, map[string]any{"messages": items})

	case path == "/gmail/v1/users/me/labels":
		if r.Method == http.MethodPost {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			writeJSON(w, map[string]string{"id": "Label_" + body["name"], "name": body["name"]})
			return
		}
		writeJSON(w, map[string]any{"labels": []any{
			map[string]string{"id": "INBOX", "name": "INBOX"},
		}})

	case strings.HasSuffix(path, "/modify"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/gmail/v1/users/me/messages/"), "/modify")
		var body struct {
			AddLabelIDs []string `json:"addLabelIds"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.labelled[id] = append(f.labelled[id], body.AddLabelIDs...)
		writeJSON(w, map[string]string{"id": id})

	case strings.HasPrefix(path, "/gmail/v1/users/me/messages/"):
		id := strings.TrimPrefix(path, "/gmail/v1/users/me/messages/")
		msg, ok := f.messages[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			writeJSON(w, map[string]any{"error": map[string]any{"message": "no such message"}})
			return
		}
		f.fetched = append(f.fetched, id)
		writeJSON(w, map[string]any{
			"id":           id,
			"threadId":     msg.threadID,
			"snippet":      msg.snippet,
			"internalDate": strconv.FormatInt(msg.internalDate.UnixMilli(), 10),
			"raw":          base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(msg.raw),
		})

	default:
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]any{"error": map[string]any{"message": "unrouted: " + path}})
	}
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// harness is a connected account over a real database, blob store and archive.
type harness struct {
	fake    *fakeGmail
	repo    *store.Repo
	tokens  *Store
	syncer  *Syncer
	blobs   *blob.Store
	archive *Archive
	cfg     config.Config
}

func newHarness(t *testing.T, senders ...string) *harness {
	t.Helper()
	if len(senders) == 0 {
		senders = []string{"me@example.com"}
	}
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

	blobs, err := blob.New(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}
	archive, err := NewArchive(filepath.Join(dir, "raw-email"))
	if err != nil {
		t.Fatalf("NewArchive: %v", err)
	}

	box, err := secret.New([]byte("a key for the tests"))
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}

	cfg := config.Default()
	cfg.Gmail.ClientID = "test-client"
	cfg.Gmail.AllowedSenders = senders
	cfg.Gmail.MaxAttachmentBytes = 1 << 20
	cfg.Secrets.GmailClientSecret = "test-secret"

	fake := newFakeGmail(t)
	tokens := NewStore(repo, box, cfg, "https://rental.example.com/api/v1/gmail/callback")
	tokens.SetBaseURL(fake.URL())

	quiet := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
	return &harness{
		fake: fake, repo: repo, tokens: tokens, blobs: blobs, archive: archive, cfg: cfg,
		syncer: NewSyncer(tokens, repo, blobs, archive, cfg.Gmail, quiet),
	}
}

// connect runs the real authorization-code exchange against the fake.
func (h *harness) connect(t *testing.T) Account {
	t.Helper()
	account, err := h.tokens.Connect(t.Context(), "an-authorization-code")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return account
}

// message builds a raw multipart email with the given attachments.
func message(from, subject, body string, attachments ...[3]string) []byte {
	const boundary = "sep-1234"
	var sb strings.Builder

	fmt.Fprintf(&sb, "From: %s\r\n", from)
	sb.WriteString("To: bot@example.com\r\n")
	fmt.Fprintf(&sb, "Subject: %s\r\n", subject)
	sb.WriteString("MIME-Version: 1.0\r\n")

	if len(attachments) == 0 {
		sb.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
		sb.WriteString(body)
		return []byte(sb.String())
	}

	fmt.Fprintf(&sb, "Content-Type: multipart/mixed; boundary=%q\r\n\r\n", boundary)
	fmt.Fprintf(&sb, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", boundary, body)

	// Each attachment is {filename, mime, content}.
	for _, att := range attachments {
		fmt.Fprintf(&sb, "--%s\r\n", boundary)
		fmt.Fprintf(&sb, "Content-Type: %s\r\n", att[1])
		fmt.Fprintf(&sb, "Content-Disposition: attachment; filename=%q\r\n", att[0])
		sb.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
		sb.WriteString(base64.StdEncoding.EncodeToString([]byte(att[2])))
		sb.WriteString("\r\n")
	}
	fmt.Fprintf(&sb, "--%s--\r\n", boundary)
	return []byte(sb.String())
}
