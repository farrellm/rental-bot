// Package ingest is the proposal gate: classify, extract, propose, apply.
//
// It is the one path between a language model and this application's records,
// and it is deliberately narrow. Nothing a model produced reaches the ledger
// except as an `ingest_proposals` row that a person has agreed to — with one
// exception, spelled out in §5.4 and implemented in autoApplies below, that
// requires three conditions at once and still leaves the row it writes flagged
// for review.
//
// docs/DESIGN.md §5.4 calls that gate the design's single most important
// safety property. A misextracted receipt that silently enters the ledger is
// the most likely real-world bug in this system and the one that damages trust
// in every number the dashboard shows.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"time"

	"github.com/farrellm/rental-bot/internal/alert"
	"github.com/farrellm/rental-bot/internal/blob"
	"github.com/farrellm/rental-bot/internal/config"
	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/jobs"
	"github.com/farrellm/rental-bot/internal/llm"
	"github.com/farrellm/rental-bot/internal/secret"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// The job kinds this package registers.
const (
	// KindClassify reads one message and files a proposal for it.
	KindClassify = "ingest.classify"
	// KindExtract fills in that proposal's payload.
	KindExtract = "ingest.extract"
	// KindSweep looks for messages that arrived and were never read.
	//
	// This is to the enqueue at sync time what the Gmail poller is to the
	// webhook (§4.2 step 6): the enqueue makes ingestion fast, and this makes
	// it reliable. Both are free because every step reads before it writes.
	KindSweep = "ingest.sweep"
)

// sweepPage bounds one sweep. A backlog larger than this is drained over
// several sweeps rather than enqueued in one burst that fills the queue.
const sweepPage = 50

// Reader is the half of llm.Client this package uses.
//
// It is an interface so a test can drive the whole pipeline end to end without
// an API key — which is the test that actually proves this milestone, and it
// must not need one.
type Reader interface {
	// Model names the model in use, for the provenance column.
	Model() string
	// Available reports whether a call would be allowed, without making one.
	Available(ctx context.Context) error
	Classify(ctx context.Context, in llm.Input) (llm.Classification, llm.Usage, error)
	Extract(ctx context.Context, kind string, in llm.Input) (any, llm.Usage, error)
}

// Pipeline runs the three stages.
type Pipeline struct {
	repo   *store.Repo
	blobs  *blob.Store
	reader Reader
	queue  *jobs.Queue
	notify func()
	box    *secret.Box
	cfg    config.LLM
	alerts alert.Publisher
	log    *slog.Logger
	// now is injectable so a test can assert a timestamp without racing one.
	now func() time.Time
}

// Options carry the pieces the pipeline cannot build for itself.
type Options struct {
	Repo  *store.Repo
	Blobs *blob.Store
	// Reader is nil when no model is configured. The pipeline still applies
	// and rejects proposals in that state -- deciding about a reading that is
	// already on file does not need the thing that made it, and a host that
	// turned the model off should still be able to clear its queue.
	Reader Reader
	Queue  *jobs.Queue
	// Notify wakes the runner, so an extract enqueued by a classify starts now
	// rather than at the next tick.
	Notify func()
	// Box encrypts the policy and loan numbers an extract carries (§9.2).
	Box    *secret.Box
	Config config.LLM
	Alerts alert.Publisher
	Logger *slog.Logger
}

// New builds the pipeline.
func New(opts Options) *Pipeline {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Notify == nil {
		opts.Notify = func() {}
	}
	return &Pipeline{
		repo:   opts.Repo,
		blobs:  opts.Blobs,
		reader: opts.Reader,
		queue:  opts.Queue,
		notify: opts.Notify,
		box:    opts.Box,
		cfg:    opts.Config,
		alerts: opts.Alerts,
		log:    opts.Logger,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// Reads reports whether there is a model behind this pipeline.
//
// Everything that talks to one is guarded by it; everything that settles a
// proposal is not.
func (p *Pipeline) Reads() bool { return p != nil && p.reader != nil }

// EnqueueClassify queues the reading of one message.
//
// It is exported because the Gmail syncer calls it the moment a message's
// attachments are filed, which is what makes a forwarded receipt appear on the
// review screen in seconds rather than at the next sweep. The dedupe key is
// per message, and the index behind it is partial over pending jobs, so the
// sweep re-enqueuing the same message costs one refused insert.
func (p *Pipeline) EnqueueClassify(ctx context.Context, messageID int64) error {
	if !p.Reads() {
		// Nothing would handle the job. The message is filed either way.
		return nil
	}
	added, err := p.queue.EnqueueOnce(ctx, KindClassify, classifyKey(messageID), classifyPayload{EmailMessageID: messageID})
	if err != nil {
		return fmt.Errorf("ingest: queue a classify for message %d: %w", messageID, err)
	}
	if added {
		p.notify()
	}
	return nil
}

func classifyKey(messageID int64) string {
	return KindClassify + ":" + strconv.FormatInt(messageID, 10)
}

func extractKey(proposalID int64) string {
	return KindExtract + ":" + strconv.FormatInt(proposalID, 10)
}

type classifyPayload struct {
	EmailMessageID int64 `json:"email_message_id"`
}

type extractPayload struct {
	ProposalID int64 `json:"proposal_id"`
}

// Sweep enqueues a classify for every message that arrived and was never read.
//
// It reads nothing and writes no proposal itself: it only finds work the
// direct enqueue missed, which is what makes running it every fifteen minutes
// cost one query against an index.
//
// A tripped budget stops it dead. Enqueueing work that is guaranteed to be
// refused would fill the queue with jobs that spend their attempts and then
// dead-letter, and the condition is already being reported by the breaker.
func (p *Pipeline) Sweep(ctx context.Context) (int, error) {
	if !p.Reads() {
		return 0, nil
	}
	if err := p.budgetOpen(ctx); err != nil {
		p.log.Warn("skipping the ingest sweep", "reason", err)
		return 0, nil
	}

	waiting, err := p.repo.Read().ListMessagesAwaitingProposal(ctx, sweepPage)
	if err != nil {
		return 0, fmt.Errorf("ingest: look for unread messages: %w", err)
	}

	queued := 0
	for _, msg := range waiting {
		if err := p.EnqueueClassify(ctx, msg.ID); err != nil {
			return queued, err
		}
		queued++
	}
	if queued > 0 {
		p.log.Info("queued messages the sync did not", "count", queued)
	}
	return queued, nil
}

// budgetOpen reports whether there is budget left, without spending any.
func (p *Pipeline) budgetOpen(ctx context.Context) error {
	if !p.Reads() {
		return ErrNoReader
	}
	return p.reader.Available(ctx)
}

// ErrNoReader reports that no model is configured, so nothing can be read.
//
// It is not a failure of the work: the mail is archived and filed, and the
// sweep will read it if a model is ever configured.
var ErrNoReader = errors.New("ingest: no model is configured")

// enclosures loads what came attached to a message, as far as the caps allow.
//
// An attachment past llm.max_attachment_bytes is skipped rather than truncated:
// half a PDF is not a smaller PDF, and a model reading one produces a
// confident answer about a document it did not see.
func (p *Pipeline) enclosures(ctx context.Context, messageID int64) ([]llm.File, error) {
	attachments, err := p.repo.Read().ListEmailAttachments(ctx, messageID)
	if err != nil {
		return nil, fmt.Errorf("ingest: read the attachments of message %d: %w", messageID, err)
	}

	files := make([]llm.File, 0, len(attachments))
	for _, att := range attachments {
		if att.DocumentID == nil {
			continue
		}
		doc, err := p.repo.Read().GetDocument(ctx, *att.DocumentID)
		if err != nil {
			return nil, fmt.Errorf("ingest: read document %d: %w", *att.DocumentID, err)
		}
		if doc.SizeBytes > p.cfg.MaxAttachmentBytes {
			p.log.Info("enclosure too large to read",
				"document_id", doc.ID, "size_bytes", doc.SizeBytes, "cap", p.cfg.MaxAttachmentBytes)
			continue
		}
		if !readable(doc.Mime) {
			continue
		}

		bytes, err := p.readBlob(doc.Sha256)
		if err != nil {
			// The row says the bytes are there and they are not. That is worth
			// a line in the log and not worth stopping the read: the email
			// text alone is often enough to classify.
			p.log.Error("could not read an enclosure", "document_id", doc.ID, "error", err)
			continue
		}
		files = append(files, llm.File{
			Filename:  doc.OriginalFilename,
			MediaType: doc.Mime,
			Bytes:     bytes,
		})
	}
	return files, nil
}

// readable reports whether a media type is one a model can be shown.
//
// The list is short on purpose. A .docx or a .zip sent as base64 is tokens
// spent on something no provider will read, and it comes back as a confident
// answer about nothing.
func readable(mime string) bool {
	switch mime {
	case "application/pdf", "image/png", "image/jpeg", "image/gif", "image/webp", "text/plain":
		return true
	}
	return false
}

func (p *Pipeline) readBlob(digest string) ([]byte, error) {
	f, err := p.blobs.Open(digest)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// stamp is the one spelling of "now" in this package.
func (p *Pipeline) stamp() string { return domain.Stamp(p.now()) }

// setMessage records where a message stands, and why when there is a why.
func (p *Pipeline) setMessage(ctx context.Context, id int64, status, detail string) {
	if err := p.repo.Write().SetEmailMessageStatus(ctx, sqlc.SetEmailMessageStatusParams{
		Status:    status,
		Error:     domain.Clip(detail, 500),
		UpdatedAt: p.stamp(),
		ID:        id,
	}); err != nil {
		p.log.Error("could not record a message's standing",
			"email_message_id", id, "status", status, "error", err)
	}
}
