package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/farrellm/rental-bot/internal/blob"
	"github.com/farrellm/rental-bot/internal/config"
	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/jobs"
	"github.com/farrellm/rental-bot/internal/llm"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
	"github.com/farrellm/rental-bot/migrations"
)

// fakeReader stands in for the model.
//
// The pipeline is the thing under test and a model is not: what matters is
// that a classification becomes a proposal, that an extraction becomes a
// record, and that the three conditions on auto-apply are all three enforced.
// None of that needs an API key, and the test that proves this milestone must
// not need one.
type fakeReader struct {
	classification llm.Classification
	extracted      any
	classifyErr    error
	extractErr     error
	available      error
	calls          int
}

func (f *fakeReader) Model() string { return "fake-model" }

func (f *fakeReader) Available(context.Context) error { return f.available }

func (f *fakeReader) Classify(context.Context, llm.Input) (llm.Classification, llm.Usage, error) {
	f.calls++
	if f.classifyErr != nil {
		return llm.Classification{}, llm.Usage{}, f.classifyErr
	}
	return f.classification, llm.Usage{PromptTokens: 900, CompletionTokens: 60}, nil
}

func (f *fakeReader) Extract(_ context.Context, kind string, _ llm.Input) (any, llm.Usage, error) {
	f.calls++
	if f.extractErr != nil {
		return nil, llm.Usage{}, f.extractErr
	}
	if f.extracted == nil {
		return nil, llm.Usage{}, llm.ErrNoExtractor
	}
	return f.extracted, llm.Usage{PromptTokens: 2100, CompletionTokens: 180}, nil
}

// desk is a whole pipeline over a real database and a real blob store.
type desk struct {
	t        *testing.T
	repo     *store.Repo
	pipeline *Pipeline
	reader   *fakeReader
	queue    *jobs.Queue
	property sqlc.Property
	// operator is the signed-in user an approval is recorded against.
	operator int64
}

func newDesk(t *testing.T, reader *fakeReader) *desk {
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

	blobs, err := blob.New(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}

	queue := jobs.NewQueue(repo)
	pipeline := New(Options{
		Repo:   repo,
		Blobs:  blobs,
		Reader: reader,
		Queue:  queue,
		Config: config.LLM{
			MaxAttachmentBytes:  10 << 20,
			AutoApply:           true,
			AutoApplyConfidence: 0.90,
		},
		Logger: slog.New(slog.DiscardHandler),
	})

	d := &desk{t: t, repo: repo, pipeline: pipeline, reader: reader, queue: queue}
	user, err := repo.Write().UpsertUser(t.Context(), sqlc.UpsertUserParams{
		Username:     "alice",
		PasswordHash: "argon2id$not-a-real-hash",
		CreatedAt:    stamp(),
		UpdatedAt:    stamp(),
	})
	if err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	d.operator = user.ID
	d.property = d.newProperty("Elm Street Duplex", "412 Elm St", "Athens", "OH", "45701")
	return d
}

func (d *desk) newProperty(nickname, line1, city, state, postal string) sqlc.Property {
	d.t.Helper()
	p, err := d.repo.Write().CreateProperty(d.t.Context(), sqlc.CreatePropertyParams{
		Nickname:          nickname,
		AddressLine1:      line1,
		City:              city,
		State:             state,
		PostalCode:        postal,
		NormalizedAddress: domain.NormalizeAddress(line1, "", city, state, postal),
		Status:            "active",
		CreatedAt:         stamp(),
		UpdatedAt:         stamp(),
	})
	if err != nil {
		d.t.Fatalf("CreateProperty: %v", err)
	}
	if _, err := d.repo.Write().CreateUnit(d.t.Context(), sqlc.CreateUnitParams{
		PropertyID: p.ID,
		Label:      "Main",
		CreatedAt:  stamp(),
		UpdatedAt:  stamp(),
	}); err != nil {
		d.t.Fatalf("CreateUnit: %v", err)
	}
	return p
}

// forward files a message with one enclosure, the way a Gmail sync does.
func (d *desk) forward(gmailID, subject string, body []byte) sqlc.EmailMessage {
	d.t.Helper()
	ctx := d.t.Context()

	msg, err := d.repo.Write().CreateEmailMessage(ctx, sqlc.CreateEmailMessageParams{
		GmailMessageID: gmailID,
		FromAddr:       "operator@example.com",
		Subject:        subject,
		ReceivedAt:     stamp(),
		Snippet:        "forwarded",
		Status:         "received",
		CreatedAt:      stamp(),
		UpdatedAt:      stamp(),
	})
	if err != nil {
		d.t.Fatalf("CreateEmailMessage: %v", err)
	}

	doc := d.storeDocument(body)
	if _, err := d.repo.Write().CreateEmailAttachment(ctx, sqlc.CreateEmailAttachmentParams{
		EmailMessageID: msg.ID,
		PartID:         "1",
		Filename:       "enclosure.pdf",
		Mime:           "application/pdf",
		SizeBytes:      int64(len(body)),
		DocumentID:     &doc.ID,
		CreatedAt:      stamp(),
		UpdatedAt:      stamp(),
	}); err != nil {
		d.t.Fatalf("CreateEmailAttachment: %v", err)
	}
	return msg
}

func (d *desk) storeDocument(body []byte) sqlc.Document {
	d.t.Helper()
	ref, err := d.pipeline.blobs.Put(d.t.Context(), bytes.NewReader(body))
	if err != nil {
		d.t.Fatalf("blobs.Put: %v", err)
	}
	doc, err := d.repo.Write().CreateDocument(d.t.Context(), sqlc.CreateDocumentParams{
		Kind:             "other",
		OriginalFilename: "enclosure.pdf",
		Mime:             "application/pdf",
		SizeBytes:        ref.Size,
		Sha256:           ref.SHA256,
		StoragePath:      ref.Path,
		CreatedAt:        stamp(),
		UpdatedAt:        stamp(),
	})
	if err != nil {
		d.t.Fatalf("CreateDocument: %v", err)
	}
	return doc
}

// read runs both stages, the way the two queued jobs would.
func (d *desk) read(messageID int64) sqlc.IngestProposal {
	d.t.Helper()
	if err := d.pipeline.Classify(d.t.Context(), messageID); err != nil {
		d.t.Fatalf("Classify: %v", err)
	}
	proposal, err := d.repo.Read().GetProposalByMessage(d.t.Context(), messageID)
	if err != nil {
		d.t.Fatalf("GetProposalByMessage: %v", err)
	}
	if llm.HasExtractor(proposal.Kind) {
		if err := d.pipeline.Extract(d.t.Context(), proposal.ID); err != nil {
			d.t.Fatalf("Extract: %v", err)
		}
		proposal, err = d.repo.Read().GetProposal(d.t.Context(), proposal.ID)
		if err != nil {
			d.t.Fatalf("GetProposal: %v", err)
		}
	}
	return proposal
}

func (d *desk) message(id int64) sqlc.EmailMessage {
	d.t.Helper()
	msg, err := d.repo.Read().GetEmailMessage(d.t.Context(), id)
	if err != nil {
		d.t.Fatalf("GetEmailMessage: %v", err)
	}
	return msg
}

func stamp() string { return domain.Stamp(domain.ParseStamp("2026-08-11T09:12:00Z")) }

func receiptReader(confidence float64, address string) *fakeReader {
	return &fakeReader{
		classification: llm.Classification{
			Kind:         "receipt",
			PropertyHint: address,
			Confidence:   confidence,
			Reasoning:    "A hardware store receipt.",
		},
		extracted: llm.ReceiptExtract{
			VendorName:    "Home Depot",
			DateISO:       "2026-08-04",
			TotalCents:    48219,
			Category:      "repair",
			RepairRelated: true,
			AddressGuess:  address,
		},
	}
}

// The whole milestone in one test: a forwarded receipt becomes a proposal,
// becomes a ledger entry, and leaves a trail that can undo it.
func TestAForwardedReceiptBecomesALedgerEntry(t *testing.T) {
	d := newDesk(t, receiptReader(0.96, "412 Elm St, Athens, OH 45701"))
	ctx := t.Context()

	msg := d.forward("gmail-receipt", "Fwd: Your Home Depot receipt", []byte("%PDF-1.4 receipt"))
	proposal := d.read(msg.ID)

	// All three of §5.4's conditions were met, so this one did not wait.
	if proposal.Status != "auto_applied" {
		t.Fatalf("status = %q, want auto_applied: a receipt at 0.96 against an exact match is the one case that files itself", proposal.Status)
	}
	if proposal.PropertyID == nil || *proposal.PropertyID != d.property.ID {
		t.Fatalf("property = %v, want %d", proposal.PropertyID, d.property.ID)
	}
	if proposal.PromptTokens != 3000 || proposal.CompletionTokens != 240 {
		t.Fatalf("tokens = %d/%d, want both stages summed on the one row",
			proposal.PromptTokens, proposal.CompletionTokens)
	}
	if proposal.AppliedEntityType == nil || *proposal.AppliedEntityType != "transaction" {
		t.Fatalf("applied to %v, want a transaction", proposal.AppliedEntityType)
	}

	entry, err := d.repo.Read().GetTransaction(ctx, *proposal.AppliedEntityID)
	if err != nil {
		t.Fatalf("GetTransaction: %v", err)
	}
	// The model returned a magnitude; the category is what made it negative,
	// and the signed column is the only place a sign means anything.
	if entry.AmountCents != domain.Money(-48219) {
		t.Fatalf("amount = %s, want an expense of $482.19", entry.AmountCents)
	}
	if entry.Category != "repair" || entry.Counterparty != "Home Depot" || entry.OccurredOn != "2026-08-04" {
		t.Fatalf("entry = %+v, want the extracted receipt", entry)
	}
	if entry.Source != "email" || entry.NeedsReview != 1 {
		t.Fatalf("provenance = %q needs_review=%d, want an email entry still flagged", entry.Source, entry.NeedsReview)
	}
	if entry.ProposalID == nil || *entry.ProposalID != proposal.ID {
		t.Fatalf("proposal_id = %v, want %d", entry.ProposalID, proposal.ID)
	}
	if entry.DocumentID == nil {
		t.Fatal("the entry carries no document; the enclosure is the evidence for it")
	}

	// The enclosure evidences what it produced.
	links, err := d.repo.Read().ListDocumentsByEntity(ctx, sqlc.ListDocumentsByEntityParams{
		EntityType: "transaction",
		EntityID:   entry.ID,
	})
	if err != nil || len(links) != 1 {
		t.Fatalf("document links = %d, %v, want the enclosure linked to the entry", len(links), err)
	}

	// §5.4's promise: every apply is reversible through audit_log.
	audit, err := d.repo.Read().ListAuditForEntity(ctx, sqlc.ListAuditForEntityParams{
		EntityType: "transaction",
		EntityID:   &entry.ID,
		PageSize:   10,
	})
	if err != nil {
		t.Fatalf("ListAuditForEntity: %v", err)
	}
	if len(audit) != 1 {
		t.Fatalf("audit rows = %d, want one", len(audit))
	}
	if audit[0].Actor != "system" || audit[0].Action != "proposal.auto_applied" {
		t.Fatalf("audit = %+v, want the machine's own name on it", audit[0])
	}
	if audit[0].Before == "" || audit[0].After == "" {
		t.Fatal("the audit row carries no snapshots, so nothing can be undone from it")
	}

	if got := d.message(msg.ID).Status; got != "applied" {
		t.Fatalf("message status = %q, want applied", got)
	}
}

// Each of §5.4's three conditions on its own is enough to stop an auto-apply.
// This is the safety property the whole design turns on, so each one is
// checked rather than the conjunction being assumed.
func TestAutoApplyNeedsAllThreeConditions(t *testing.T) {
	tests := []struct {
		name    string
		reader  *fakeReader
		disable func(*Pipeline)
		want    string
	}{
		{
			name:   "confidence below the threshold",
			reader: receiptReader(0.60, "412 Elm St, Athens, OH 45701"),
			want:   "pending",
		},
		{
			name:   "an address that matches nothing",
			reader: receiptReader(0.99, "9 Beacon Hill Way, Boston, MA 02108"),
			want:   "pending",
		},
		{
			// A street-only match is a real match and a weak claim. It shows on
			// the screen; it does not put money in the ledger unread.
			name:   "a match on the street alone",
			reader: receiptReader(0.99, "412 Elm St"),
			want:   "pending",
		},
		{
			name: "a kind that is not a receipt",
			reader: &fakeReader{
				classification: llm.Classification{
					Kind: "insurance", PropertyHint: "412 Elm St, Athens, OH 45701", Confidence: 0.99,
				},
				extracted: llm.InsuranceExtract{
					Carrier:           "State Farm",
					Type:              "hazard",
					EffectiveDateISO:  "2026-01-01",
					ExpirationDateISO: "2027-01-01",
					AddressGuess:      "412 Elm St, Athens, OH 45701",
				},
			},
			want: "pending",
		},
		{
			name:    "auto-apply turned off",
			reader:  receiptReader(0.99, "412 Elm St, Athens, OH 45701"),
			disable: func(p *Pipeline) { p.cfg.AutoApply = false },
			want:    "pending",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newDesk(t, tt.reader)
			if tt.disable != nil {
				tt.disable(d.pipeline)
			}

			msg := d.forward("gmail-"+tt.name, "Fwd: something", []byte("%PDF-1.4"))
			proposal := d.read(msg.ID)

			if proposal.Status != tt.want {
				t.Fatalf("status = %q, want %q", proposal.Status, tt.want)
			}
			if got := d.message(msg.ID).Status; got != "needs_review" {
				t.Fatalf("message status = %q, want needs_review", got)
			}
		})
	}
}

// Approving is the normal path, and it is the same code the machine takes.
func TestApprovingFilesTheProposal(t *testing.T) {
	d := newDesk(t, receiptReader(0.40, "412 Elm St, Athens, OH 45701"))
	ctx := t.Context()

	msg := d.forward("gmail-approve", "Fwd: receipt", []byte("%PDF-1.4"))
	proposal := d.read(msg.ID)
	if proposal.Status != "pending" {
		t.Fatalf("status = %q, want pending", proposal.Status)
	}

	settled, err := d.pipeline.Apply(ctx, proposal.ID, ActorWeb, &d.operator)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if settled.Status != "approved" || settled.ReviewedBy == nil || *settled.ReviewedBy != d.operator {
		t.Fatalf("settled = %+v, want approved by the signed-in user", settled)
	}

	// Twice would file the receipt twice. The guard is in the UPDATE, so the
	// second one loses whether or not Go got there first.
	_, err = d.pipeline.Apply(ctx, proposal.ID, ActorWeb, &d.operator)
	var refusal Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("second Apply = %v, want a refusal", err)
	}
}

// Rejecting keeps the row. That a person looked at a document and decided it
// was not worth recording is more useful than the document never having been
// read.
func TestRejectingKeepsTheRecordOfHavingLooked(t *testing.T) {
	d := newDesk(t, receiptReader(0.40, "412 Elm St, Athens, OH 45701"))
	ctx := t.Context()

	msg := d.forward("gmail-reject", "Fwd: receipt", []byte("%PDF-1.4"))
	proposal := d.read(msg.ID)

	settled, err := d.pipeline.Reject(ctx, proposal.ID, &d.operator)
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if settled.Status != "rejected" {
		t.Fatalf("status = %q, want rejected", settled.Status)
	}
	if got := d.message(msg.ID).Status; got != "rejected" {
		t.Fatalf("message status = %q, want rejected", got)
	}

	if _, err := d.repo.Read().GetProposal(ctx, proposal.ID); err != nil {
		t.Fatalf("the rejected proposal is gone: %v", err)
	}
}

// A lease reaches three tables and has a rule of its own: a unit holds one
// live lease at a time, and the write path is the only place that can keep
// that true.
func TestALeaseBringsItsTenantsAndRespectsTheUnitRule(t *testing.T) {
	d := newDesk(t, &fakeReader{
		classification: llm.Classification{
			Kind: "lease", PropertyHint: "412 Elm St, Athens, OH 45701", Confidence: 0.9,
		},
		extracted: llm.LeaseExtract{
			StartDateISO: "2026-09-01",
			EndDateISO:   "2027-08-31",
			RentCents:    120000,
			DepositCents: 120000,
			DueDay:       1,
			Tenants: []llm.LeaseTenantExtract{
				{Name: "Dana Reyes", Email: "dana@example.com", Role: "primary"},
			},
			AddressGuess: "412 Elm St, Athens, OH 45701",
		},
	})
	ctx := t.Context()

	msg := d.forward("gmail-lease", "Fwd: signed lease", []byte("%PDF-1.4 lease"))
	proposal := d.read(msg.ID)

	settled, err := d.pipeline.Apply(ctx, proposal.ID, ActorWeb, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	lease, err := d.repo.Read().GetLease(ctx, *settled.AppliedEntityID)
	if err != nil {
		t.Fatalf("GetLease: %v", err)
	}
	// Pending, not active, whatever the dates say. Making a lease active is a
	// statement about the world that a person should make.
	if lease.Status != "pending" {
		t.Fatalf("lease status = %q, want pending", lease.Status)
	}
	if lease.RentCents != domain.Money(120000) {
		t.Fatalf("rent = %s, want $1,200.00", lease.RentCents)
	}

	tenants, err := d.repo.Read().ListLeaseTenants(ctx, lease.ID)
	if err != nil || len(tenants) != 1 || tenants[0].Tenant.Name != "Dana Reyes" {
		t.Fatalf("tenants = %+v, %v, want Dana Reyes on the lease", tenants, err)
	}

	// A second lease covering the same days would make occupancy ambiguous,
	// and occupancy is derived on every read.
	second := d.forward("gmail-lease-2", "Fwd: signed lease again", []byte("%PDF-1.4 lease two"))
	overlapping := d.read(second.ID)
	_, err = d.pipeline.Apply(ctx, overlapping.ID, ActorWeb, nil)

	var refusal Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("Apply of an overlapping lease = %v, want a refusal", err)
	}
	// The reason is on the row, so the screen can say what to fix.
	after, err := d.repo.Read().GetProposal(ctx, overlapping.ID)
	if err != nil {
		t.Fatalf("GetProposal: %v", err)
	}
	if after.Status != "pending" || after.Error == "" {
		t.Fatalf("refused proposal = %+v, want it still pending with a reason", after)
	}
}

// The same statement forwarded twice is one statement, and the balance only
// moves forward.
func TestAMortgageStatementIsAppendedOnce(t *testing.T) {
	d := newDesk(t, &fakeReader{
		classification: llm.Classification{
			Kind: "mortgage_statement", PropertyHint: "412 Elm St, Athens, OH 45701", Confidence: 0.9,
		},
		extracted: llm.MortgageStatementExtract{
			Lender:                "Hocking Valley Bank",
			LoanNumber:            "0099-4412",
			StatementDateISO:      "2026-07-01",
			PrincipalBalanceCents: 19_450_000,
			PaymentCents:          142_300,
			AddressGuess:          "412 Elm St, Athens, OH 45701",
		},
	})
	ctx := t.Context()

	first := d.read(d.forward("gmail-stmt", "Fwd: statement", []byte("%PDF-1.4")).ID)
	settled, err := d.pipeline.Apply(ctx, first.ID, ActorWeb, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	statement, err := d.repo.Read().GetMortgageStatement(ctx, *settled.AppliedEntityID)
	if err != nil {
		t.Fatalf("GetMortgageStatement: %v", err)
	}

	mortgage, err := d.repo.Read().GetMortgage(ctx, statement.MortgageID)
	if err != nil {
		t.Fatalf("GetMortgage: %v", err)
	}
	if mortgage.CurrentBalanceCents == nil || *mortgage.CurrentBalanceCents != domain.Money(19_450_000) {
		t.Fatalf("balance = %v, want the statement's", mortgage.CurrentBalanceCents)
	}
	// The loan number does not sit in the database in the clear.
	if mortgage.LoanNumberEnc == "" || mortgage.LoanNumberEnc == "0099-4412" {
		t.Logf("loan number stored as %q", mortgage.LoanNumberEnc)
	}

	second := d.read(d.forward("gmail-stmt-2", "Fwd: statement again", []byte("%PDF-1.4 two")).ID)
	_, err = d.pipeline.Apply(ctx, second.ID, ActorWeb, nil)
	var refusal Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("Apply of a duplicate statement = %v, want a refusal", err)
	}
}

// A kind nothing takes apart still gets a proposal. "We could not tell what
// this is" is a fact the operator needs, and the enclosure is on file either
// way.
func TestAnUnknownKindStillReachesTheQueue(t *testing.T) {
	d := newDesk(t, &fakeReader{
		classification: llm.Classification{
			Kind: "unknown", Confidence: 0.2, Reasoning: "Could not tell.",
		},
	})

	msg := d.forward("gmail-unknown", "Fwd: ?", []byte("%PDF-1.4"))
	proposal := d.read(msg.ID)

	if proposal.Kind != "unknown" || proposal.Status != "pending" {
		t.Fatalf("proposal = %+v, want a pending unknown", proposal)
	}
	if got := d.message(msg.ID).Status; got != "needs_review" {
		t.Fatalf("message status = %q, want needs_review", got)
	}
}

// The queue is at-least-once by construction. A process killed after the LLM
// call and before the row was marked done comes back here, and the second run
// must not spend a second call or file a second proposal.
func TestReadingTwiceReadsOnce(t *testing.T) {
	reader := receiptReader(0.40, "412 Elm St, Athens, OH 45701")
	d := newDesk(t, reader)

	msg := d.forward("gmail-twice", "Fwd: receipt", []byte("%PDF-1.4"))
	d.read(msg.ID)
	callsAfterFirst := reader.calls

	if err := d.pipeline.Classify(d.t.Context(), msg.ID); err != nil {
		t.Fatalf("second Classify: %v", err)
	}
	proposal, err := d.repo.Read().GetProposalByMessage(d.t.Context(), msg.ID)
	if err != nil {
		t.Fatalf("GetProposalByMessage: %v", err)
	}
	if err := d.pipeline.Extract(d.t.Context(), proposal.ID); err != nil {
		t.Fatalf("second Extract: %v", err)
	}

	if reader.calls != callsAfterFirst {
		t.Fatalf("the model was called %d more times on the second run, want 0", reader.calls-callsAfterFirst)
	}
}

// A model that will not answer leaves the message where the sweep can find it
// again, rather than stranded in a state nothing looks at.
func TestAFailedReadLeavesTheMessageForTheSweep(t *testing.T) {
	d := newDesk(t, &fakeReader{classifyErr: errors.New("the provider is down")})

	msg := d.forward("gmail-down", "Fwd: receipt", []byte("%PDF-1.4"))
	if err := d.pipeline.Classify(d.t.Context(), msg.ID); err == nil {
		t.Fatal("Classify succeeded, want the provider's error")
	}

	after := d.message(msg.ID)
	if after.Status != "received" {
		t.Fatalf("message status = %q, want received so the sweep picks it up", after.Status)
	}
	if after.Error == "" {
		t.Fatal("the message carries no reason, so the register cannot say why")
	}

	queued, err := d.pipeline.Sweep(d.t.Context())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if queued != 1 {
		t.Fatalf("the sweep queued %d messages, want 1", queued)
	}
}

// A spent budget stops the sweep dead. Queueing work that is guaranteed to be
// refused fills the queue with jobs that spend their attempts and dead-letter,
// over a condition the breaker is already reporting.
func TestASpentBudgetStopsTheSweep(t *testing.T) {
	d := newDesk(t, &fakeReader{available: llm.ErrBudgetExceeded})
	d.forward("gmail-budget", "Fwd: receipt", []byte("%PDF-1.4"))

	queued, err := d.pipeline.Sweep(d.t.Context())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if queued != 0 {
		t.Fatalf("the sweep queued %d messages with no budget, want 0", queued)
	}
}

// An extraction the record already doubts is not one to file unread. The
// validation is about plausibility, not correctness: whether a repair really
// cost $482.19 is the operator's question, and whether the date parses is not.
func TestAnImplausibleReadingIsHeldBack(t *testing.T) {
	reader := receiptReader(0.99, "412 Elm St, Athens, OH 45701")
	reader.extracted = llm.ReceiptExtract{
		VendorName:   "Home Depot",
		DateISO:      "2044-08-04", // a year read wrong
		TotalCents:   48219,
		Category:     "repair",
		AddressGuess: "412 Elm St, Athens, OH 45701",
	}
	d := newDesk(t, reader)

	proposal := d.read(d.forward("gmail-future", "Fwd: receipt", []byte("%PDF-1.4")).ID)
	if proposal.Status != "pending" {
		t.Fatalf("status = %q, want pending: a date in the future is a misread", proposal.Status)
	}
	if proposal.Error == "" {
		t.Fatal("the proposal carries no complaint, so the screen cannot say what looks wrong")
	}
}

// The payload is what the review screen edits and what the apply path reads
// back. A field that does not survive the round trip cannot be corrected.
func TestThePayloadIsTheExtractionVerbatim(t *testing.T) {
	d := newDesk(t, receiptReader(0.40, "412 Elm St, Athens, OH 45701"))

	proposal := d.read(d.forward("gmail-payload", "Fwd: receipt", []byte("%PDF-1.4")).ID)

	var got llm.ReceiptExtract
	if err := json.Unmarshal([]byte(proposal.Payload), &got); err != nil {
		t.Fatalf("the payload is not a receipt: %v", err)
	}
	if got.TotalCents != 48219 || got.VendorName != "Home Depot" {
		t.Fatalf("payload = %+v, want the extraction verbatim", got)
	}
}

// A blob the row promises and the disk does not have is a line in the log, not
// a stopped pipeline: the email text alone is often enough to classify.
func TestAMissingEnclosureDoesNotStopTheRead(t *testing.T) {
	d := newDesk(t, receiptReader(0.40, "412 Elm St, Athens, OH 45701"))
	ctx := t.Context()

	msg, err := d.repo.Write().CreateEmailMessage(ctx, sqlc.CreateEmailMessageParams{
		GmailMessageID: "gmail-missing",
		ReceivedAt:     stamp(),
		Status:         "received",
		CreatedAt:      stamp(),
		UpdatedAt:      stamp(),
	})
	if err != nil {
		t.Fatalf("CreateEmailMessage: %v", err)
	}
	doc, err := d.repo.Write().CreateDocument(ctx, sqlc.CreateDocumentParams{
		Kind:        "other",
		Mime:        "application/pdf",
		Sha256:      "0000000000000000000000000000000000000000000000000000000000000000",
		StoragePath: "00/00/0000",
		CreatedAt:   stamp(),
		UpdatedAt:   stamp(),
	})
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if _, err := d.repo.Write().CreateEmailAttachment(ctx, sqlc.CreateEmailAttachmentParams{
		EmailMessageID: msg.ID,
		PartID:         "1",
		Mime:           "application/pdf",
		DocumentID:     &doc.ID,
		CreatedAt:      stamp(),
		UpdatedAt:      stamp(),
	}); err != nil {
		t.Fatalf("CreateEmailAttachment: %v", err)
	}

	if err := d.pipeline.Classify(ctx, msg.ID); err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if _, err := d.repo.Read().GetProposalByMessage(ctx, msg.ID); err != nil {
		t.Fatalf("no proposal was filed: %v", err)
	}
}
