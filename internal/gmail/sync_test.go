package gmail

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

func TestSyncFilesAMessageAndItsAttachment(t *testing.T) {
	h := newHarness(t)
	h.connect(t)

	raw := message("Me <me@example.com>", "Fwd: Ace Hardware receipt",
		"Forwarding this for 412 Elm St.",
		[3]string{"receipt.pdf", "application/pdf", "%PDF-1.4 a receipt"})

	h.fake.addMessage("m1", fakeMessage{
		threadID: "t1", internalDate: time.Now().Add(-time.Hour), snippet: "Forwarding this",
		raw: raw,
	})
	h.fake.setHistory("", fakeHistoryPage{messageIDs: []string{"m1"}, historyID: 5100})

	result, err := h.syncer.Sync(t.Context())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if result.Stored != 1 || result.Ignored != 0 || result.Failed != 0 {
		t.Fatalf("result = %+v, want one stored message", result)
	}

	msg, err := h.repo.Read().GetEmailMessageByGmailID(t.Context(), "m1")
	if err != nil {
		t.Fatalf("GetEmailMessageByGmailID: %v", err)
	}
	if msg.Status != "received" {
		t.Errorf("status = %q, want received", msg.Status)
	}
	if msg.FromAddr != "me@example.com" {
		t.Errorf("from = %q, want the bare address without the display name", msg.FromAddr)
	}
	if msg.Subject != "Fwd: Ace Hardware receipt" {
		t.Errorf("subject = %q", msg.Subject)
	}

	// The raw .eml is on disk, byte for byte.
	archived, err := os.ReadFile(filepath.Join(h.archive.Root(), msg.RawPath))
	if err != nil {
		t.Fatalf("read the archived message: %v", err)
	}
	if string(archived) != string(raw) {
		t.Error("the archived bytes are not what Gmail returned")
	}

	// The attachment is a document, with its provenance.
	attachments, err := h.repo.Read().ListEmailAttachments(t.Context(), msg.ID)
	if err != nil {
		t.Fatalf("ListEmailAttachments: %v", err)
	}
	if len(attachments) != 1 {
		t.Fatalf("filed %d attachments, want 1", len(attachments))
	}
	att := attachments[0]
	if att.Filename != "receipt.pdf" || att.Mime != "application/pdf" {
		t.Errorf("attachment = %q %q", att.Filename, att.Mime)
	}
	if att.DocumentID == nil {
		t.Fatal("the attachment has no document")
	}

	doc, err := h.repo.Read().GetDocument(t.Context(), *att.DocumentID)
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if doc.SourceMessageID == nil || *doc.SourceMessageID != msg.ID {
		t.Errorf("document source_message_id = %v, want %d", doc.SourceMessageID, msg.ID)
	}
	if doc.PropertyID != nil {
		t.Error("an ingested document was matched to a property; that is M4's job and it is deterministic Go, not a guess here")
	}

	// The bytes are in the content-addressed store under their digest.
	if _, err := h.blobs.Stat(doc.Sha256); err != nil {
		t.Errorf("the document's bytes are not in the blob store: %v", err)
	}

	// The mailbox shows what happened.
	if labels := h.fake.labelsOn("m1"); len(labels) == 0 {
		t.Error("the message was not labelled in Gmail")
	}
}

// The poller walks history the webhook already delivered. That overlap has to
// cost nothing, and this is the test that says so.
func TestSyncingTwiceFilesOneCopy(t *testing.T) {
	h := newHarness(t)
	h.connect(t)

	h.fake.addMessage("m1", fakeMessage{
		internalDate: time.Now(),
		raw: message("me@example.com", "Receipt", "body",
			[3]string{"receipt.pdf", "application/pdf", "the same bytes"}),
	})
	h.fake.setHistory("", fakeHistoryPage{messageIDs: []string{"m1"}, historyID: 5100})

	first, err := h.syncer.Sync(t.Context())
	if err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	second, err := h.syncer.Sync(t.Context())
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}

	if first.Stored != 1 {
		t.Errorf("first sync stored %d, want 1", first.Stored)
	}
	if second.Stored != 0 {
		t.Errorf("second sync stored %d, want 0", second.Stored)
	}
	// Not re-fetched either: the row is checked before the download.
	if n := h.fake.fetchCount("m1"); n != 1 {
		t.Errorf("the message was fetched %d times, want 1", n)
	}

	msg, err := h.repo.Read().GetEmailMessageByGmailID(t.Context(), "m1")
	if err != nil {
		t.Fatalf("GetEmailMessageByGmailID: %v", err)
	}
	attachments, err := h.repo.Read().ListEmailAttachments(t.Context(), msg.ID)
	if err != nil {
		t.Fatalf("ListEmailAttachments: %v", err)
	}
	if len(attachments) != 1 {
		t.Errorf("filed %d attachments after two syncs, want 1", len(attachments))
	}
}

// A public-ish inbox will receive spam. The allowlist is the first and cheapest
// defense, and nothing outside it gets filed.
func TestSenderOutsideTheAllowlistIsIgnored(t *testing.T) {
	h := newHarness(t, "me@example.com")
	h.connect(t)

	h.fake.addMessage("m1", fakeMessage{
		internalDate: time.Now(),
		raw: message("Nigerian Prince <spam@elsewhere.example>", "You have won", "click here",
			[3]string{"malware.pdf", "application/pdf", "definitely a receipt"}),
	})
	h.fake.setHistory("", fakeHistoryPage{messageIDs: []string{"m1"}, historyID: 5100})

	result, err := h.syncer.Sync(t.Context())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if result.Ignored != 1 || result.Stored != 0 {
		t.Fatalf("result = %+v, want one ignored message", result)
	}

	msg, err := h.repo.Read().GetEmailMessageByGmailID(t.Context(), "m1")
	if err != nil {
		t.Fatalf("GetEmailMessageByGmailID: %v", err)
	}
	if msg.Status != "ignored" {
		t.Errorf("status = %q, want ignored", msg.Status)
	}

	attachments, err := h.repo.Read().ListEmailAttachments(t.Context(), msg.ID)
	if err != nil {
		t.Fatalf("ListEmailAttachments: %v", err)
	}
	if len(attachments) != 0 {
		t.Errorf("filed %d attachments from an ignored sender, want 0", len(attachments))
	}
}

// After a multi-day outage the cursor is older than Gmail's history. The walk
// falls back to listing by timestamp rather than losing the mail.
func TestHistoryTooOldFallsBackToATimestampWalk(t *testing.T) {
	h := newHarness(t)
	h.connect(t)

	h.fake.addMessage("m1", fakeMessage{
		internalDate: time.Now(), raw: message("me@example.com", "Receipt", "body"),
	})
	h.fake.historyTooOld = true
	h.fake.listed = []string{"m1"}

	result, err := h.syncer.Sync(t.Context())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !result.Resynced {
		t.Error("the result does not report the resync; the operator should see this happened")
	}
	if result.Stored != 1 {
		t.Errorf("stored %d, want 1: the fallback has to find the mail the cursor lost", result.Stored)
	}

	// The cursor was replaced with the mailbox's current one, so the next sync
	// walks history again rather than resyncing forever.
	account, err := h.tokens.Account(t.Context())
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	if account.HistoryID != h.fake.profileHistoryID {
		t.Errorf("history id = %d, want %d", account.HistoryID, h.fake.profileHistoryID)
	}
}

// "There was a 40 MB PDF and we did not take it" is a fact worth keeping.
func TestOversizedAttachmentIsRecordedAndSkipped(t *testing.T) {
	h := newHarness(t)
	// Small enough to reject the 500-byte attachment, loose enough that the
	// message carrying it still comes down: the whole-message cap is four
	// times this, and rejecting the message would be testing something else.
	h.syncer.cfg.MaxAttachmentBytes = 300
	h.connect(t)

	h.fake.addMessage("m1", fakeMessage{
		internalDate: time.Now(),
		raw: message("me@example.com", "A big one", "body",
			[3]string{"huge.pdf", "application/pdf", strings.Repeat("x", 500)}),
	})
	h.fake.setHistory("", fakeHistoryPage{messageIDs: []string{"m1"}, historyID: 5100})

	if _, err := h.syncer.Sync(t.Context()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	msg, err := h.repo.Read().GetEmailMessageByGmailID(t.Context(), "m1")
	if err != nil {
		t.Fatalf("GetEmailMessageByGmailID: %v", err)
	}
	attachments, err := h.repo.Read().ListEmailAttachments(t.Context(), msg.ID)
	if err != nil {
		t.Fatalf("ListEmailAttachments: %v", err)
	}
	if len(attachments) != 1 {
		t.Fatalf("recorded %d attachments, want 1", len(attachments))
	}
	if attachments[0].DocumentID != nil {
		t.Error("an over-cap attachment was stored anyway")
	}
	if attachments[0].SkippedReason == "" {
		t.Error("an over-cap attachment was recorded without saying why it was skipped")
	}
	if attachments[0].SizeBytes != 500 {
		t.Errorf("size = %d, want 500: the size is what makes the skip explicable", attachments[0].SizeBytes)
	}
}

// A message too large to fetch at all is recorded rather than dropped. It will
// be exactly as large next time, so a retry is pointless and silence is worse.
func TestMessagePastTheCapIsRecordedAsFailed(t *testing.T) {
	h := newHarness(t)
	h.syncer.cfg.MaxAttachmentBytes = 16 // a whole-message cap of 64 bytes
	h.connect(t)

	h.fake.addMessage("m1", fakeMessage{
		internalDate: time.Now(),
		raw:          message("me@example.com", "A very long subject line indeed", strings.Repeat("y", 400)),
	})
	h.fake.setHistory("", fakeHistoryPage{messageIDs: []string{"m1"}, historyID: 5100})

	result, err := h.syncer.Sync(t.Context())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if result.Failed != 1 {
		t.Fatalf("result = %+v, want one failed message", result)
	}

	msg, err := h.repo.Read().GetEmailMessageByGmailID(t.Context(), "m1")
	if err != nil {
		t.Fatalf("GetEmailMessageByGmailID: %v", err)
	}
	if msg.Status != "failed" || msg.Error == "" {
		t.Errorf("status = %q, error = %q; want failed with a reason", msg.Status, msg.Error)
	}
	if msg.RawPath != "" {
		t.Error("a message that was never downloaded claims an archived original")
	}

	// And it is not fetched again on the next sync.
	if _, err := h.syncer.Sync(t.Context()); err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if n := h.fake.fetchCount("m1"); n != 1 {
		t.Errorf("the oversized message was fetched %d times, want 1", n)
	}
}

// A message that will not parse is kept, not dropped: its bytes are exactly
// what a parser fix has to be replayed against.
func TestMalformedMessageIsStoredWithItsRawBytes(t *testing.T) {
	h := newHarness(t)
	h.connect(t)

	h.fake.addMessage("m1", fakeMessage{
		internalDate: time.Now(),
		raw:          []byte("this is not an email at all"),
	})
	h.fake.setHistory("", fakeHistoryPage{messageIDs: []string{"m1"}, historyID: 5100})

	result, err := h.syncer.Sync(t.Context())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if result.Failed != 1 {
		t.Fatalf("result = %+v, want one failed message", result)
	}

	msg, err := h.repo.Read().GetEmailMessageByGmailID(t.Context(), "m1")
	if err != nil {
		t.Fatalf("GetEmailMessageByGmailID: %v", err)
	}
	if msg.Status != "failed" {
		t.Errorf("status = %q, want failed", msg.Status)
	}
	if msg.Error == "" {
		t.Error("a failed message kept no record of why")
	}
	if msg.RawPath == "" {
		t.Fatal("a failed message has no archived original, which is the one it most needs")
	}
	if _, err := os.Stat(filepath.Join(h.archive.Root(), msg.RawPath)); err != nil {
		t.Errorf("the archived original is missing: %v", err)
	}
}

// The same receipt forwarded twice is one document with two messages pointing
// at it. That falls out of content addressing rather than being a feature.
func TestTheSameAttachmentTwiceIsOneDocument(t *testing.T) {
	h := newHarness(t)
	h.connect(t)

	body := [3]string{"receipt.pdf", "application/pdf", "identical bytes"}
	h.fake.addMessage("m1", fakeMessage{internalDate: time.Now(),
		raw: message("me@example.com", "First forward", "body", body)})
	h.fake.addMessage("m2", fakeMessage{internalDate: time.Now(),
		raw: message("me@example.com", "Second forward", "body", body)})
	h.fake.setHistory("", fakeHistoryPage{messageIDs: []string{"m1", "m2"}, historyID: 5100})

	if _, err := h.syncer.Sync(t.Context()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	first := documentFor(t, h, "m1")
	second := documentFor(t, h, "m2")
	if first.ID != second.ID {
		t.Errorf("two forwards of one receipt made documents %d and %d, want one", first.ID, second.ID)
	}
}

func TestSyncWithoutAnAccountSaysSo(t *testing.T) {
	h := newHarness(t)
	if _, err := h.syncer.Sync(t.Context()); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Sync with no account = %v, want ErrNotConnected", err)
	}
}

// The token is at rest as ciphertext. A database copy without the key does not
// hand over the mailbox.
func TestTheStoredRefreshTokenIsEncrypted(t *testing.T) {
	h := newHarness(t)
	h.connect(t)

	row, err := h.repo.Read().GetGmailAccount(t.Context())
	if err != nil {
		t.Fatalf("GetGmailAccount: %v", err)
	}
	if strings.Contains(row.RefreshTokenEnc, "refresh-token") {
		t.Fatal("the refresh token is in the database in the clear")
	}
	if row.Address != "bot@example.com" {
		t.Errorf("address = %q, want the one the profile reported", row.Address)
	}
	if row.HistoryID == "" {
		t.Error("connecting did not seed the cursor; the first sync would walk history that predates the grant")
	}
}

func TestWatchRecordsItsExpiry(t *testing.T) {
	h := newHarness(t)
	h.connect(t)

	watcher := NewWatcher(h.tokens, "projects/rental/topics/gmail")
	if _, err := watcher.Renew(t.Context()); err != nil {
		t.Fatalf("Renew: %v", err)
	}

	account, err := h.tokens.Account(t.Context())
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	if account.WatchExpiresAt == nil {
		t.Fatal("the watch expiry was not recorded")
	}
	if account.WatchLapsed(time.Now()) {
		t.Error("a watch registered a moment ago reads as lapsed")
	}
	if h.fake.watches != 1 {
		t.Errorf("registered %d watches, want 1", h.fake.watches)
	}
}

func TestDisconnectForgetsTheAccountAndKeepsTheMail(t *testing.T) {
	h := newHarness(t)
	h.connect(t)

	h.fake.addMessage("m1", fakeMessage{internalDate: time.Now(),
		raw: message("me@example.com", "Receipt", "body")})
	h.fake.setHistory("", fakeHistoryPage{messageIDs: []string{"m1"}, historyID: 5100})
	if _, err := h.syncer.Sync(t.Context()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if err := h.tokens.Disconnect(t.Context()); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if _, err := h.tokens.Account(t.Context()); !errors.Is(err, ErrNotConnected) {
		t.Errorf("Account after Disconnect = %v, want ErrNotConnected", err)
	}
	if _, err := h.repo.Read().GetEmailMessageByGmailID(t.Context(), "m1"); err != nil {
		t.Errorf("disconnecting removed the mail that had already arrived: %v", err)
	}
}

func documentFor(t *testing.T, h *harness, gmailID string) sqlc.Document {
	t.Helper()
	msg, err := h.repo.Read().GetEmailMessageByGmailID(t.Context(), gmailID)
	if err != nil {
		t.Fatalf("GetEmailMessageByGmailID(%s): %v", gmailID, err)
	}
	attachments, err := h.repo.Read().ListEmailAttachments(t.Context(), msg.ID)
	if err != nil {
		t.Fatalf("ListEmailAttachments: %v", err)
	}
	if len(attachments) != 1 || attachments[0].DocumentID == nil {
		t.Fatalf("%s has no filed attachment", gmailID)
	}
	doc, err := h.repo.Read().GetDocument(t.Context(), *attachments[0].DocumentID)
	if err != nil {
		if store.NotFound(err) {
			t.Fatalf("%s points at a document that does not exist", gmailID)
		}
		t.Fatalf("GetDocument: %v", err)
	}
	return doc
}
