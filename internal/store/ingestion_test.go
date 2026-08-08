package store

import (
	"testing"

	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// The whole ingestion pipeline is idempotent on this one constraint (§4.2), so
// it is worth a test of its own rather than being assumed from the DDL.
func TestGmailMessageIDIsTheIdempotencyKey(t *testing.T) {
	repo := openRepo(t)
	ctx := t.Context()

	params := sqlc.CreateEmailMessageParams{
		GmailMessageID: "18f0c0ffee",
		FromAddr:       "me@example.com",
		Subject:        "Fwd: receipt",
		ReceivedAt:     now(),
		Status:         "received",
		CreatedAt:      now(),
		UpdatedAt:      now(),
	}
	first, err := repo.Write().CreateEmailMessage(ctx, params)
	if err != nil {
		t.Fatalf("CreateEmailMessage: %v", err)
	}

	if _, err := repo.Write().CreateEmailMessage(ctx, params); !Conflict(err) {
		t.Fatalf("second insert of the same gmail_message_id = %v, want a uniqueness conflict", err)
	}

	got, err := repo.Read().GetEmailMessageByGmailID(ctx, params.GmailMessageID)
	if err != nil {
		t.Fatalf("GetEmailMessageByGmailID: %v", err)
	}
	if got.ID != first.ID {
		t.Errorf("GetEmailMessageByGmailID returned id %d, want %d", got.ID, first.ID)
	}
}

// An attachment that arrived twice in one re-synced message is one row.
func TestAttachmentsAreUniquePerPart(t *testing.T) {
	repo := openRepo(t)
	ctx := t.Context()
	msg := newEmailMessage(t, repo, "18f0decafe")

	params := sqlc.CreateEmailAttachmentParams{
		EmailMessageID: msg.ID,
		PartID:         "2",
		Filename:       "receipt.pdf",
		Mime:           "application/pdf",
		SizeBytes:      1024,
		CreatedAt:      now(),
		UpdatedAt:      now(),
	}
	if _, err := repo.Write().CreateEmailAttachment(ctx, params); err != nil {
		t.Fatalf("CreateEmailAttachment: %v", err)
	}
	if _, err := repo.Write().CreateEmailAttachment(ctx, params); !Conflict(err) {
		t.Fatalf("second insert of the same part = %v, want a uniqueness conflict", err)
	}

	// Two attachments sharing a filename are still two attachments.
	params.PartID = "3"
	if _, err := repo.Write().CreateEmailAttachment(ctx, params); err != nil {
		t.Fatalf("CreateEmailAttachment for a second part: %v", err)
	}

	rows, err := repo.Read().ListEmailAttachments(ctx, msg.ID)
	if err != nil {
		t.Fatalf("ListEmailAttachments: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("ListEmailAttachments returned %d rows, want 2", len(rows))
	}
}

// 0003 rebuilds jobs to move dedupe_key onto a partial index. A full UNIQUE
// would let a key be used once and never again, which the ten-minute poller
// would hit on its second tick.
func TestJobDedupeKeyIsReusableOnceTheJobHasRun(t *testing.T) {
	repo := openRepo(t)
	ctx := t.Context()
	key := "gmail.sync"

	first, err := repo.Write().EnqueueJob(ctx, sqlc.EnqueueJobParams{
		Kind: "gmail.sync", Payload: "{}", DedupeKey: &key,
		RunAfter: now(), MaxAttempts: 5, CreatedAt: now(), UpdatedAt: now(),
	})
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	_, err = repo.Write().EnqueueJob(ctx, sqlc.EnqueueJobParams{
		Kind: "gmail.sync", Payload: "{}", DedupeKey: &key,
		RunAfter: now(), MaxAttempts: 5, CreatedAt: now(), UpdatedAt: now(),
	})
	if !Conflict(err) {
		t.Fatalf("a second pending job under one key = %v, want a uniqueness conflict", err)
	}

	if err := repo.Write().CompleteJob(ctx, sqlc.CompleteJobParams{UpdatedAt: now(), ID: first.ID}); err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}

	if _, err := repo.Write().EnqueueJob(ctx, sqlc.EnqueueJobParams{
		Kind: "gmail.sync", Payload: "{}", DedupeKey: &key,
		RunAfter: now(), MaxAttempts: 5, CreatedAt: now(), UpdatedAt: now(),
	}); err != nil {
		t.Fatalf("EnqueueJob after the first one finished: %v", err)
	}
}

// A document that arrived attached to an email carries where it came from, and
// deleting the message leaves the document on file with that provenance
// cleared -- the bytes are still evidence of something.
func TestDocumentKeepsItsSourceMessage(t *testing.T) {
	repo := openRepo(t)
	ctx := t.Context()
	msg := newEmailMessage(t, repo, "18f0facade")

	doc, err := repo.Write().CreateDocument(ctx, sqlc.CreateDocumentParams{
		Kind:            "receipt",
		Title:           "Ace Hardware",
		Mime:            "application/pdf",
		Sha256:          "0f9d1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7",
		StoragePath:     "0f/9d/0f9d1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7",
		SourceMessageID: &msg.ID,
		CreatedAt:       now(),
		UpdatedAt:       now(),
	})
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if doc.SourceMessageID == nil || *doc.SourceMessageID != msg.ID {
		t.Fatalf("SourceMessageID = %v, want %d", doc.SourceMessageID, msg.ID)
	}
}

func newEmailMessage(t *testing.T, repo *Repo, gmailID string) sqlc.EmailMessage {
	t.Helper()
	msg, err := repo.Write().CreateEmailMessage(t.Context(), sqlc.CreateEmailMessageParams{
		GmailMessageID: gmailID,
		FromAddr:       "me@example.com",
		ReceivedAt:     now(),
		Status:         "received",
		CreatedAt:      now(),
		UpdatedAt:      now(),
	})
	if err != nil {
		t.Fatalf("CreateEmailMessage: %v", err)
	}
	return msg
}
