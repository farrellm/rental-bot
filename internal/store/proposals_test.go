package store

import (
	"testing"

	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// newMessage files an arrived email so a proposal has something to hang off.
func newMessage(t *testing.T, repo *Repo, gmailID string) sqlc.EmailMessage {
	t.Helper()
	msg, err := repo.Write().CreateEmailMessage(t.Context(), sqlc.CreateEmailMessageParams{
		GmailMessageID: gmailID,
		FromAddr:       "operator@example.com",
		Subject:        "Fwd: your receipt",
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

func newProposal(t *testing.T, repo *Repo, messageID int64, kind string) sqlc.IngestProposal {
	t.Helper()
	confidence := 0.94
	p, err := repo.Write().CreateProposal(t.Context(), sqlc.CreateProposalParams{
		EmailMessageID: messageID,
		Kind:           kind,
		Payload:        `{}`,
		LlmModel:       "claude-sonnet-5",
		PromptTokens:   1200,
		Confidence:     &confidence,
		Status:         "pending",
		CreatedAt:      now(),
		UpdatedAt:      now(),
	})
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	return p
}

// A proposal leaves 'pending' exactly once. The guard is in the UPDATE rather
// than in Go, because a read-then-write lets two approvals arriving together
// both pass the read -- and the second one files the receipt a second time.
func TestAProposalIsSettledOnlyOnce(t *testing.T) {
	repo := openRepo(t)
	ctx := t.Context()

	msg := newMessage(t, repo, "gmail-1")
	proposal := newProposal(t, repo, msg.ID, "receipt")

	entity := "transaction"
	entityID := int64(7)
	settle := sqlc.SettleProposalParams{
		Status:            "approved",
		ReviewedAt:        strptr(now()),
		AppliedEntityType: &entity,
		AppliedEntityID:   &entityID,
		UpdatedAt:         now(),
		ID:                proposal.ID,
	}

	settled, err := repo.Write().SettleProposal(ctx, settle)
	if err != nil {
		t.Fatalf("SettleProposal: %v", err)
	}
	if settled.Status != "approved" || settled.AppliedEntityID == nil || *settled.AppliedEntityID != 7 {
		t.Fatalf("settled = %+v, want approved against transaction 7", settled)
	}

	// The second one matches nothing, which is how the loser of the race finds
	// out it lost.
	if _, err := repo.Write().SettleProposal(ctx, settle); !NotFound(err) {
		t.Fatalf("second settle = %v, want no rows", err)
	}
}

// Every status the screen can show has to be a status the column accepts, and
// nothing else can get in.
func TestProposalStatusIsClosed(t *testing.T) {
	repo := openRepo(t)
	ctx := t.Context()

	msg := newMessage(t, repo, "gmail-status")
	for _, status := range []string{"pending", "approved", "rejected", "auto_applied"} {
		if _, err := repo.Write().CreateProposal(ctx, sqlc.CreateProposalParams{
			EmailMessageID: msg.ID,
			Kind:           "receipt",
			Status:         status,
			CreatedAt:      now(),
			UpdatedAt:      now(),
		}); err != nil {
			t.Fatalf("CreateProposal(%q): %v", status, err)
		}
	}

	if _, err := repo.Write().CreateProposal(ctx, sqlc.CreateProposalParams{
		EmailMessageID: msg.ID,
		Kind:           "receipt",
		Status:         "maybe",
		CreatedAt:      now(),
		UpdatedAt:      now(),
	}); err == nil {
		t.Fatal("CreateProposal accepted an unlisted status, want the CHECK to refuse it")
	}
}

// The sweep is what makes ingestion reliable when the direct enqueue at sync
// time is lost. It has to find exactly the messages that arrived and have no
// proposal, and stop finding one the moment a proposal exists.
func TestTheSweepFindsMessagesWithNoProposal(t *testing.T) {
	repo := openRepo(t)
	ctx := t.Context()

	waiting := newMessage(t, repo, "gmail-waiting")
	done := newMessage(t, repo, "gmail-done")
	newProposal(t, repo, done.ID, "receipt")

	// Something the allowlist turned away is not waiting on anything.
	if _, err := repo.Write().CreateEmailMessage(ctx, sqlc.CreateEmailMessageParams{
		GmailMessageID: "gmail-spam",
		ReceivedAt:     now(),
		Status:         "ignored",
		CreatedAt:      now(),
		UpdatedAt:      now(),
	}); err != nil {
		t.Fatalf("CreateEmailMessage: %v", err)
	}

	rows, err := repo.Read().ListMessagesAwaitingProposal(ctx, 50)
	if err != nil {
		t.Fatalf("ListMessagesAwaitingProposal: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != waiting.ID {
		t.Fatalf("awaiting = %d rows %v, want only the one with no proposal", len(rows), rows)
	}
}

// The budget breaker sums one column across a month. Classify and extract both
// spend against the same row, so the extract update adds rather than replaces.
func TestTokenSpendAccumulatesOnOneRow(t *testing.T) {
	repo := openRepo(t)
	ctx := t.Context()

	msg := newMessage(t, repo, "gmail-tokens")
	proposal := newProposal(t, repo, msg.ID, "receipt")

	if _, err := repo.Write().RecordProposalExtract(ctx, sqlc.RecordProposalExtractParams{
		Payload:          `{"total_cents":48219}`,
		LlmModel:         "claude-sonnet-5",
		PromptTokens:     800,
		CompletionTokens: 150,
		UpdatedAt:        now(),
		ID:               proposal.ID,
	}); err != nil {
		t.Fatalf("RecordProposalExtract: %v", err)
	}

	spent, err := repo.Read().SumProposalTokensSince(ctx, "1970-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("SumProposalTokensSince: %v", err)
	}
	// 1200 from classify, plus 800 and 150 from extract.
	if spent != 2150 {
		t.Fatalf("spend = %d, want 2150", spent)
	}

	// A window that starts after the row was written sees nothing, which is
	// what makes the breaker reset at the turn of the month.
	if spent, err := repo.Read().SumProposalTokensSince(ctx, "2999-01-01T00:00:00Z"); err != nil || spent != 0 {
		t.Fatalf("spend in an empty window = %d, %v, want 0", spent, err)
	}
}

// The same statement forwarded twice is one statement, and the constraint is
// what makes that true rather than a check somebody remembers to write.
func TestAMortgageStatementIsFiledOnce(t *testing.T) {
	repo := openRepo(t)
	ctx := t.Context()

	property := newProperty(t, repo, "Elm Street Duplex")
	mortgage, err := repo.Write().CreateMortgage(ctx, sqlc.CreateMortgageParams{
		PropertyID: property.ID,
		Lender:     "Hocking Valley Bank",
		CreatedAt:  now(),
		UpdatedAt:  now(),
	})
	if err != nil {
		t.Fatalf("CreateMortgage: %v", err)
	}

	balance := domain.Money(19_450_000)
	statement := sqlc.CreateMortgageStatementParams{
		MortgageID:            mortgage.ID,
		StatementDate:         "2026-07-01",
		PrincipalBalanceCents: &balance,
		CreatedAt:             now(),
		UpdatedAt:             now(),
	}
	if _, err := repo.Write().CreateMortgageStatement(ctx, statement); err != nil {
		t.Fatalf("CreateMortgageStatement: %v", err)
	}
	if _, err := repo.Write().CreateMortgageStatement(ctx, statement); !Conflict(err) {
		t.Fatalf("second statement for the same month = %v, want a uniqueness conflict", err)
	}

	// A later statement moves the running balance; an earlier one arriving out
	// of order does not walk it backwards.
	if err := repo.Write().SetMortgageBalance(ctx, sqlc.SetMortgageBalanceParams{
		CurrentBalanceCents: &balance,
		BalanceAsOf:         strptr("2026-07-01"),
		UpdatedAt:           now(),
		ID:                  mortgage.ID,
	}); err != nil {
		t.Fatalf("SetMortgageBalance: %v", err)
	}
	stale := domain.Money(19_900_000)
	if err := repo.Write().SetMortgageBalance(ctx, sqlc.SetMortgageBalanceParams{
		CurrentBalanceCents: &stale,
		BalanceAsOf:         strptr("2026-06-01"),
		UpdatedAt:           now(),
		ID:                  mortgage.ID,
	}); err != nil {
		t.Fatalf("SetMortgageBalance with an older statement: %v", err)
	}

	got, err := repo.Read().GetMortgage(ctx, mortgage.ID)
	if err != nil {
		t.Fatalf("GetMortgage: %v", err)
	}
	if got.CurrentBalanceCents == nil || *got.CurrentBalanceCents != balance {
		t.Fatalf("balance = %v, want the one from the newer statement", got.CurrentBalanceCents)
	}
}

// Widening document_links.entity_type is a table rebuild, and the rebuild has
// to keep the rows it already held as well as accept the new types.
func TestDocumentLinksReachTheEntitiesM4Writes(t *testing.T) {
	repo := openRepo(t)
	ctx := t.Context()

	property := newProperty(t, repo, "Elm Street Duplex")
	doc, err := repo.Write().CreateDocument(ctx, sqlc.CreateDocumentParams{
		PropertyID:  &property.ID,
		Kind:        "insurance",
		Sha256:      "a1b2c3",
		StoragePath: "a1/b2/a1b2c3",
		CreatedAt:   now(),
		UpdatedAt:   now(),
	})
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}

	for _, entity := range []string{"property", "insurance_policy", "mortgage", "mortgage_statement"} {
		if _, err := repo.Write().CreateDocumentLink(ctx, sqlc.CreateDocumentLinkParams{
			DocumentID: doc.ID,
			EntityType: entity,
			EntityID:   1,
			CreatedAt:  now(),
			UpdatedAt:  now(),
		}); err != nil {
			t.Fatalf("CreateDocumentLink(%q): %v", entity, err)
		}
	}

	if _, err := repo.Write().CreateDocumentLink(ctx, sqlc.CreateDocumentLinkParams{
		DocumentID: doc.ID,
		EntityType: "valuation",
		EntityID:   1,
		CreatedAt:  now(),
		UpdatedAt:  now(),
	}); err == nil {
		t.Fatal("CreateDocumentLink accepted an entity type that does not exist yet")
	}
}
