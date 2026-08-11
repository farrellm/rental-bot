package gmail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/farrellm/rental-bot/internal/blob"
	"github.com/farrellm/rental-bot/internal/config"
	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// maxHistoryPages bounds one walk.
//
// A sync that has fallen a long way behind should make progress and come back
// rather than hold a worker for an hour. The cursor advances page by page, so
// the next run resumes where this one stopped.
const maxHistoryPages = 20

// Syncer performs the history walk and files what it finds.
type Syncer struct {
	tokens  *Store
	repo    *store.Repo
	blobs   *blob.Store
	archive *Archive
	cfg     config.Gmail
	log     *slog.Logger
	now     func() time.Time

	// running serialises the walk. The poller and a webhook-driven job can
	// overlap, and one process is the whole of the concurrency here, so a mutex
	// is the entire answer -- no advisory lock table, no lease.
	running sync.Mutex
}

// NewSyncer wires the pieces a sync needs.
func NewSyncer(tokens *Store, repo *store.Repo, blobs *blob.Store, archive *Archive, cfg config.Gmail, logger *slog.Logger) *Syncer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Syncer{
		tokens: tokens, repo: repo, blobs: blobs, archive: archive,
		cfg: cfg, log: logger,
		now: func() time.Time { return time.Now().UTC() },
	}
}

// Result reports what one sync did.
type Result struct {
	// Fetched is how many messages were looked at, new or not.
	Fetched int
	// Stored is how many were new to the database.
	Stored int
	// Ignored is how many came from a sender outside the allowlist.
	Ignored int
	// Failed is how many would not parse. Their raw bytes are on disk.
	Failed int
	// Resynced reports that the cursor was too old and the walk fell back to
	// listing by timestamp.
	Resynced bool
}

// Sync walks forward from the stored cursor and files everything new.
//
// Every step is idempotent on gmail_message_id, which is what makes the
// fallback poller free: it walks history the webhook already delivered, and
// the overlap costs one skipped insert per message.
func (s *Syncer) Sync(ctx context.Context) (Result, error) {
	s.running.Lock()
	defer s.running.Unlock()

	client, account, err := s.tokens.Client(ctx)
	if err != nil {
		return Result{}, err
	}

	labels, err := s.ensureLabels(ctx, client)
	if err != nil {
		// Labelling is how the mailbox shows what happened, not how ingestion
		// works. Losing it is worth a line, not a failed sync.
		s.log.Warn("could not prepare the Gmail labels", "error", err)
	}

	walked, cursor, err := s.walk(ctx, client, account)
	if err != nil {
		if recordErr := s.tokens.RecordFailure(ctx, err); recordErr != nil {
			s.log.Error("record the sync failure", "error", recordErr)
		}
		return walked.Result, err
	}
	result := walked.Result

	for _, id := range walked.messageIDs {
		stored, disposition, err := s.ingest(ctx, client, id, labels)
		if err != nil {
			// One message that will not come down does not stop the batch. Its
			// id is not committed to the cursor either, because the cursor only
			// moves at the end -- so the next run tries it again.
			s.log.Error("ingest message", "gmail_message_id", id, "error", err)
			continue
		}
		result.Fetched++
		switch {
		case disposition == "ignored":
			result.Ignored++
		case disposition == "failed":
			result.Failed++
		case stored:
			result.Stored++
		}
	}

	if cursor > 0 {
		if err := s.tokens.RecordSync(ctx, cursor, int64(result.Stored)); err != nil {
			return result, err
		}
	}
	return result, nil
}

// walkResult carries the ids a walk found alongside the counts.
type walkResult struct {
	Result
	messageIDs []string
}

// walk collects the message ids to fetch and the cursor to store afterward.
func (s *Syncer) walk(ctx context.Context, client *Client, account Account) (walkResult, uint64, error) {
	var out walkResult

	// No cursor means the account was just connected and Connect seeded one
	// from the profile, or the row predates that. Either way there is nothing
	// to walk forward from, so the first sync lists recent mail instead.
	if account.HistoryID == 0 {
		out.Resynced = true
		ids, err := s.listSince(ctx, client, s.fallbackSince(account))
		if err != nil {
			return out, 0, err
		}
		out.messageIDs = ids
		return out, s.currentHistoryID(ctx, client), nil
	}

	cursor := account.HistoryID
	pageToken := ""
	for page := range maxHistoryPages {
		history, err := client.ListHistory(ctx, account.HistoryID, pageToken)
		if errors.Is(err, ErrHistoryTooOld) {
			// The cursor predates what Gmail keeps, which happens after any
			// multi-day outage (§4.3). Fall back to a timestamp walk and say so
			// -- this is a condition the operator should see, not a silent
			// recovery.
			s.log.Warn("the stored historyId is too old; resyncing by timestamp",
				"history_id", account.HistoryID)
			out.Resynced = true
			ids, listErr := s.listSince(ctx, client, s.fallbackSince(account))
			if listErr != nil {
				return out, 0, listErr
			}
			out.messageIDs = ids
			return out, s.currentHistoryID(ctx, client), nil
		}
		if err != nil {
			return out, 0, err
		}

		out.messageIDs = append(out.messageIDs, history.MessageIDs...)
		if history.HistoryID > cursor {
			cursor = history.HistoryID
		}
		if history.NextPageToken == "" {
			break
		}
		pageToken = history.NextPageToken
		if page == maxHistoryPages-1 {
			s.log.Warn("stopping the history walk at the page cap; the next sync will continue",
				"pages", maxHistoryPages)
		}
	}
	return out, cursor, nil
}

// listSince is the timestamp fallback, paged.
func (s *Syncer) listSince(ctx context.Context, client *Client, since time.Time) ([]string, error) {
	var (
		ids   []string
		token string
	)
	for range maxHistoryPages {
		page, err := client.ListMessagesSince(ctx, since, token)
		if err != nil {
			return nil, err
		}
		ids = append(ids, page.MessageIDs...)
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
	}
	return ids, nil
}

// fallbackSince bounds a timestamp resync.
//
// The last successful sync when there was one; otherwise the day the account
// was connected, because mail that predates the grant is not this system's
// record to make.
func (s *Syncer) fallbackSince(account Account) time.Time {
	if account.LastSyncAt != nil {
		// A day of overlap, because Gmail's `after:` is coarse and re-listing a
		// message already on file costs one skipped insert.
		return account.LastSyncAt.Add(-24 * time.Hour)
	}
	if !account.ConnectedAt.IsZero() {
		return account.ConnectedAt
	}
	return s.now().Add(-7 * 24 * time.Hour)
}

// currentHistoryID reads the mailbox's cursor, for after a timestamp resync.
//
// A failure here is not fatal: the walk already has its message ids, and a
// cursor that does not advance means the next sync resyncs by timestamp again,
// which is slow rather than wrong.
func (s *Syncer) currentHistoryID(ctx context.Context, client *Client) uint64 {
	profile, err := client.Profile(ctx)
	if err != nil {
		s.log.Warn("could not read the profile for a new cursor", "error", err)
		return 0
	}
	return profile.HistoryID
}

// labelSet is the two label ids this process applies.
type labelSet struct {
	processed string
	ignored   string
}

func (s *Syncer) ensureLabels(ctx context.Context, client *Client) (labelSet, error) {
	var out labelSet
	processed, err := client.EnsureLabel(ctx, s.cfg.ProcessedLabel)
	if err != nil {
		return out, err
	}
	out.processed = processed

	ignored, err := client.EnsureLabel(ctx, s.cfg.IgnoredLabel)
	if err != nil {
		return out, err
	}
	out.ignored = ignored
	return out, nil
}

// ingest handles one message end to end.
//
// The order is deliberate: fetch, archive, then insert. The archive exists so
// that a parser fix can be replayed against what actually arrived, and a
// message that fails to parse is exactly the one whose bytes are wanted — so
// the write to disk happens before anything that can reject the message.
func (s *Syncer) ingest(ctx context.Context, client *Client, gmailID string, labels labelSet) (stored bool, disposition string, err error) {
	// Already on file. gmail_message_id is UNIQUE and this read is the cheap
	// half of that guarantee: the poller re-walks history the webhook already
	// delivered, and this is what makes the overlap free.
	if _, err := s.repo.Read().GetEmailMessageByGmailID(ctx, gmailID); err == nil {
		return false, "", nil
	} else if !store.NotFound(err) {
		return false, "", err
	}

	raw, err := client.GetRaw(ctx, gmailID, s.messageCap())
	if errors.Is(err, ErrTooLarge) {
		// It will be exactly as large next time, so retrying is pointless and
		// dropping it silently is worse: the operator forwarded something and
		// deserves to be told it did not fit. The row carries the reason and no
		// archive, because there is nothing on disk to point at.
		return s.recordUningestible(ctx, gmailID, err)
	}
	if err != nil {
		return false, "", err
	}

	receivedAt := raw.InternalDate
	if receivedAt.IsZero() {
		receivedAt = s.now()
	}
	rawPath, err := s.archive.Put(gmailID, receivedAt, raw.Raw)
	if err != nil {
		return false, "", err
	}

	parsed, parseErr := Parse(raw.Raw)
	sender := SenderAddress(parsed.From)

	// The allowlist, before anything is filed. A public-ish inbox will receive
	// spam, and this is the first and cheapest defense (§4.2 step 4).
	switch {
	case parseErr != nil:
		disposition = "failed"
	case !s.allowed(sender):
		disposition = "ignored"
	default:
		disposition = "received"
	}

	detail := ""
	if parseErr != nil {
		detail = parseErr.Error()
	}

	msg, err := s.repo.Write().CreateEmailMessage(ctx, sqlc.CreateEmailMessageParams{
		GmailMessageID: gmailID,
		ThreadID:       raw.ThreadID,
		FromAddr:       sender,
		ToAddr:         SenderAddress(parsed.To),
		Subject:        domain.Clip(parsed.Subject, 500),
		ReceivedAt:     domain.Stamp(receivedAt),
		Snippet:        domain.Clip(snippet(raw.Snippet, parsed.Text), 500),
		RawPath:        rawPath,
		Status:         disposition,
		Error:          domain.Clip(detail, 500),
		CreatedAt:      s.stamp(),
		UpdatedAt:      s.stamp(),
	})
	if store.Conflict(err) {
		// Two syncs raced on one message. The other one filed it.
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}

	if disposition == "received" {
		if err := s.fileAttachments(ctx, msg, parsed.Attachments); err != nil {
			return true, disposition, err
		}
	}

	label := labels.processed
	if disposition != "received" {
		label = labels.ignored
	}
	if label != "" {
		if err := client.ModifyLabels(ctx, gmailID, []string{label}, nil); err != nil {
			s.log.Warn("could not label the message in Gmail",
				"gmail_message_id", gmailID, "error", err)
		}
	}
	return true, disposition, nil
}

// recordUningestible files a message that could not be brought down at all.
//
// It gets a row so the register shows it and the next sync skips it, and it
// gets no raw_path because nothing was archived. The distinction matters when
// someone later asks why a forwarded email never appeared.
func (s *Syncer) recordUningestible(ctx context.Context, gmailID string, cause error) (bool, string, error) {
	_, err := s.repo.Write().CreateEmailMessage(ctx, sqlc.CreateEmailMessageParams{
		GmailMessageID: gmailID,
		ReceivedAt:     s.stamp(),
		Status:         "failed",
		Error:          domain.Clip(cause.Error(), 500),
		CreatedAt:      s.stamp(),
		UpdatedAt:      s.stamp(),
	})
	if store.Conflict(err) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, "failed", nil
}

// fileAttachments stores each attachment's bytes and records the row.
//
// An attachment past the cap is recorded with the reason and not stored. "There
// was a 40 MB PDF and we did not take it" is a fact worth keeping; a silently
// missing attachment is a bug the operator finds three months later (§4.3).
func (s *Syncer) fileAttachments(ctx context.Context, msg sqlc.EmailMessage, attachments []Attachment) error {
	for _, att := range attachments {
		row := sqlc.CreateEmailAttachmentParams{
			EmailMessageID: msg.ID,
			PartID:         att.PartID,
			Filename:       domain.Clip(att.Filename, 300),
			Mime:           att.MIME,
			SizeBytes:      int64(len(att.Content)),
			CreatedAt:      s.stamp(),
			UpdatedAt:      s.stamp(),
		}

		if int64(len(att.Content)) > s.cfg.MaxAttachmentBytes {
			row.SkippedReason = fmt.Sprintf("%d bytes, past the %d byte cap",
				len(att.Content), s.cfg.MaxAttachmentBytes)
			if _, err := s.repo.Write().CreateEmailAttachment(ctx, row); err != nil && !store.Conflict(err) {
				return err
			}
			continue
		}

		doc, err := s.fileDocument(ctx, msg, att)
		if err != nil {
			return err
		}
		row.DocumentID = &doc.ID
		if _, err := s.repo.Write().CreateEmailAttachment(ctx, row); err != nil && !store.Conflict(err) {
			return err
		}
	}
	return nil
}

// fileDocument puts one attachment in the blob store and gives it a row.
//
// The property is deliberately unset. Matching an address to a property is
// deterministic Go over an extracted string (§5.3), and the extraction that
// produces that string is M4's. Filing the bytes now and the association later
// is the whole point of the content-addressed store.
func (s *Syncer) fileDocument(ctx context.Context, msg sqlc.EmailMessage, att Attachment) (sqlc.Document, error) {
	ref, err := s.blobs.Put(ctx, bytes.NewReader(att.Content))
	if err != nil {
		return sqlc.Document{}, err
	}

	// The digest is the identity. The same receipt forwarded twice, from two
	// addresses, is one document with two messages pointing at it.
	if existing, err := s.repo.Read().GetDocumentBySHA(ctx, ref.SHA256); err == nil {
		return existing, nil
	} else if !store.NotFound(err) {
		return sqlc.Document{}, err
	}

	return s.repo.Write().CreateDocument(ctx, sqlc.CreateDocumentParams{
		Kind:             documentKind(att),
		Title:            domain.Clip(att.Filename, 200),
		OriginalFilename: domain.Clip(att.Filename, 300),
		Mime:             contentType(att),
		SizeBytes:        ref.Size,
		Sha256:           ref.SHA256,
		StoragePath:      ref.Path,
		SourceMessageID:  &msg.ID,
		CreatedAt:        s.stamp(),
		UpdatedAt:        s.stamp(),
	})
}

// documentKind guesses what an attachment is from its type alone.
//
// A guess, and a deliberately weak one: a photo is a photo and a PDF could be
// anything, so everything else is "other" until M4's classifier says otherwise.
// Guessing "lease" from a filename would put a wrong word on a card, and a
// wrong word is worse than no word.
func documentKind(att Attachment) string {
	if strings.HasPrefix(att.MIME, "image/") {
		return "photo"
	}
	return "other"
}

// allowed reports whether a sender is on the list.
func (s *Syncer) allowed(sender string) bool {
	if sender == "" {
		return false
	}
	for _, candidate := range s.cfg.AllowedSenders {
		if strings.EqualFold(strings.TrimSpace(candidate), sender) {
			return true
		}
	}
	return false
}

// messageCap bounds one downloaded message.
//
// A message is its attachments plus base64's third again, so the cap on the
// whole is deliberately looser than the cap on one attachment: a two-attachment
// email should not be refused for being the sum of two acceptable parts.
func (s *Syncer) messageCap() int64 {
	return s.cfg.MaxAttachmentBytes * 4
}

// stamp is this syncer's clock, spelled the way every column holds a
// timestamp. The clock is injectable; the format is not.
func (s *Syncer) stamp() string { return domain.Stamp(s.now()) }

// snippet prefers Gmail's own, falling back to the parsed body.
func snippet(gmailSnippet, body string) string {
	if strings.TrimSpace(gmailSnippet) != "" {
		return gmailSnippet
	}
	return strings.TrimSpace(strings.Join(strings.Fields(body), " "))
}

// contentType settles on a type for a stored attachment.
//
// The declared type is what the sending client claimed and is the first
// choice; a bare application/octet-stream is a client that did not bother, and
// the extension is a better guess than nothing. Neither decides anything
// dangerous — the document handler's allowlist decides what a browser may do
// with the bytes, and it is not consulted here.
func contentType(att Attachment) string {
	if att.MIME != "" && att.MIME != "application/octet-stream" {
		return att.MIME
	}
	if ext := filepath.Ext(att.Filename); ext != "" {
		if t := mime.TypeByExtension(ext); t != "" {
			if base, _, err := mime.ParseMediaType(t); err == nil {
				return base
			}
		}
	}
	return "application/octet-stream"
}
