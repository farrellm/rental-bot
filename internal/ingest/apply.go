package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/llm"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// Actor is who is doing the applying, in audit_log's own vocabulary.
type Actor = string

const (
	// ActorWeb is a person at the review screen.
	ActorWeb Actor = "web"
	// ActorSystem is this process filing a receipt on its own, under §5.4's
	// three conditions. The value is what makes those findable later, which is
	// the whole reason auto-apply is allowed at all.
	ActorSystem Actor = "system"
)

// Refusal is an apply that cannot go through, stated so the operator can fix
// it.
//
// It is a distinct type because the caller has to tell it from a failure: a
// refusal leaves the proposal pending with the reason on the row and nothing
// to retry, while a failure is worth another attempt.
type Refusal struct{ Reason string }

func (r Refusal) Error() string { return r.Reason }

// refuse is shorthand at the call sites below, of which there are many.
func refuse(format string, args ...any) error {
	return Refusal{Reason: fmt.Sprintf(format, args...)}
}

// Apply turns a proposal into a record.
//
// Everything happens in one write transaction: the entity, the audit row, the
// document link, the proposal's settlement, and the message's standing. A log
// entry without its effect -- or an effect without its entry -- is not a state
// this database is allowed to be in, and §5.4's promise that every apply is
// reversible depends on that.
//
// The settlement is guarded in SQL rather than in Go. Two approvals arriving
// together would both pass a read-then-write, and the second would file the
// receipt a second time.
func (p *Pipeline) Apply(ctx context.Context, proposalID int64, actor Actor, userID *int64) (sqlc.IngestProposal, error) {
	var settled sqlc.IngestProposal

	err := p.repo.Tx(ctx, func(q *sqlc.Queries) error {
		proposal, err := q.GetProposal(ctx, proposalID)
		if err != nil {
			return err
		}
		if proposal.Status != "pending" {
			return refuse("this proposal has already been %s", said(proposal.Status))
		}
		if proposal.PropertyID == nil {
			return refuse("no property is matched, so there is nothing to file this against")
		}
		if proposal.Payload == "{}" {
			return refuse("nothing has been read off this document yet")
		}

		documentID := p.enclosureID(ctx, q, proposal.EmailMessageID)

		entityType, entityID, err := p.file(ctx, q, proposal, documentID)
		if err != nil {
			return err
		}

		if documentID != nil {
			// The enclosure evidences what it produced. A failure here is a
			// missing cross-reference, not a reason to unwind the record.
			if _, err := q.CreateDocumentLink(ctx, sqlc.CreateDocumentLinkParams{
				DocumentID: *documentID,
				EntityType: entityType,
				EntityID:   entityID,
				CreatedAt:  p.stamp(),
				UpdatedAt:  p.stamp(),
			}); err != nil && !store.Conflict(err) {
				return err
			}
		}

		status := "approved"
		if actor == ActorSystem {
			status = "auto_applied"
		}
		reviewedAt := p.stamp()
		settled, err = q.SettleProposal(ctx, sqlc.SettleProposalParams{
			Status:            status,
			ReviewedBy:        userID,
			ReviewedAt:        &reviewedAt,
			AppliedEntityType: &entityType,
			AppliedEntityID:   &entityID,
			UpdatedAt:         p.stamp(),
			ID:                proposalID,
		})
		if store.NotFound(err) {
			// Somebody else settled it between the read above and here.
			return refuse("this proposal has already been settled")
		}
		if err != nil {
			return err
		}

		if err := p.audit(ctx, q, actor, userID, "proposal."+status, entityType, entityID, proposal, settled); err != nil {
			return err
		}

		return q.SetEmailMessageStatus(ctx, sqlc.SetEmailMessageStatusParams{
			Status:    "applied",
			UpdatedAt: p.stamp(),
			ID:        proposal.EmailMessageID,
		})
	})

	var refusal Refusal
	if errors.As(err, &refusal) {
		// The reason belongs on the row, so the screen can say what to fix
		// rather than only that it did not work. Outside the transaction that
		// carried the refusal, because that one rolled back.
		if err := p.repo.Write().SetProposalError(ctx, sqlc.SetProposalErrorParams{
			Error:     domain.Clip(refusal.Reason, 500),
			UpdatedAt: p.stamp(),
			ID:        proposalID,
		}); err != nil {
			p.log.Error("could not record why an apply was refused", "proposal_id", proposalID, "error", err)
		}
	}
	if err != nil {
		return sqlc.IngestProposal{}, err
	}

	p.log.Info("filed a proposal",
		"proposal_id", proposalID, "actor", actor, "status", settled.Status,
		"entity", derefString(settled.AppliedEntityType), "entity_id", derefInt(settled.AppliedEntityID))
	return settled, nil
}

// Reject records that a proposal was read and refused.
//
// The row stays. A rejected proposal is the evidence that somebody looked at a
// document and decided it was not a record worth keeping, which is a different
// and more useful thing than the document never having been read.
func (p *Pipeline) Reject(ctx context.Context, proposalID int64, userID *int64) (sqlc.IngestProposal, error) {
	var settled sqlc.IngestProposal

	err := p.repo.Tx(ctx, func(q *sqlc.Queries) error {
		before, err := q.GetProposal(ctx, proposalID)
		if err != nil {
			return err
		}
		if before.Status != "pending" {
			return refuse("this proposal has already been %s", said(before.Status))
		}

		reviewedAt := p.stamp()
		settled, err = q.SettleProposal(ctx, sqlc.SettleProposalParams{
			Status:     "rejected",
			ReviewedBy: userID,
			ReviewedAt: &reviewedAt,
			UpdatedAt:  p.stamp(),
			ID:         proposalID,
		})
		if store.NotFound(err) {
			return refuse("this proposal has already been settled")
		}
		if err != nil {
			return err
		}

		if err := p.audit(ctx, q, ActorWeb, userID, "proposal.rejected", "ingest_proposal", proposalID, before, settled); err != nil {
			return err
		}

		return q.SetEmailMessageStatus(ctx, sqlc.SetEmailMessageStatusParams{
			Status:    "rejected",
			UpdatedAt: p.stamp(),
			ID:        before.EmailMessageID,
		})
	})
	if err != nil {
		return sqlc.IngestProposal{}, err
	}
	return settled, nil
}

// file writes the record a proposal describes, and names what it wrote.
func (p *Pipeline) file(ctx context.Context, q *sqlc.Queries, proposal sqlc.IngestProposal, documentID *int64) (string, int64, error) {
	switch proposal.Kind {
	case "receipt":
		return p.fileReceipt(ctx, q, proposal, documentID)
	case "lease":
		return p.fileLease(ctx, q, proposal, documentID)
	case "insurance":
		return p.fileInsurance(ctx, q, proposal, documentID)
	case "mortgage_statement":
		return p.fileMortgageStatement(ctx, q, proposal, documentID)
	default:
		return "", 0, refuse("there is nothing to file for a %s yet", strings.ReplaceAll(proposal.Kind, "_", " "))
	}
}

// fileReceipt writes a ledger entry.
//
// The sign is applied here and only here. The model returns a magnitude,
// because "is this income or expense" is not a question a reading of a
// document answers; the category is what decides it, and the signed column is
// the one place in the schema where a sign carries meaning.
func (p *Pipeline) fileReceipt(ctx context.Context, q *sqlc.Queries, proposal sqlc.IngestProposal, documentID *int64) (string, int64, error) {
	var receipt llm.ReceiptExtract
	if err := json.Unmarshal([]byte(proposal.Payload), &receipt); err != nil {
		return "", 0, refuse("the extracted receipt could not be read back")
	}
	if receipt.TotalCents <= 0 {
		return "", 0, refuse("the total is zero, so there is nothing to file")
	}
	if !isCalendarDate(receipt.DateISO) {
		return "", 0, refuse("the transaction date is not a date this can file")
	}

	// Every category a receipt can carry is money going out.
	amount := domain.Money(-receipt.TotalCents)
	description := receipt.Notes
	if description == "" {
		description = summarize(receipt)
	}

	entry, err := q.CreateTransaction(ctx, sqlc.CreateTransactionParams{
		PropertyID:    *proposal.PropertyID,
		OccurredOn:    receipt.DateISO,
		AmountCents:   amount,
		Category:      category(receipt.Category),
		Description:   domain.Clip(description, 500),
		Counterparty:  domain.Clip(receipt.VendorName, 200),
		PaymentMethod: domain.Clip(receipt.PaymentMethod, 100),
		DocumentID:    documentID,
		Source:        "email",
		Confidence:    proposal.Confidence,
		// Flagged whoever filed it. An auto-applied entry says out loud that
		// nobody has checked it, and an approved one still came off a model.
		NeedsReview: 1,
		ProposalID:  &proposal.ID,
		CreatedAt:   p.stamp(),
		UpdatedAt:   p.stamp(),
	})
	if err != nil {
		return "", 0, err
	}
	return "transaction", entry.ID, nil
}

// fileLease writes a tenancy, its tenants, and the link between them.
//
// It is the heaviest of the four, because a lease is the only extract that
// implies rows in three tables and has a rule of its own to respect: a unit
// holds one live lease at a time, and occupancy is derived from exactly these
// dates. The write path is the only place that can keep that unambiguous, so
// an overlap is a refusal rather than a second live lease.
func (p *Pipeline) fileLease(ctx context.Context, q *sqlc.Queries, proposal sqlc.IngestProposal, documentID *int64) (string, int64, error) {
	var extract llm.LeaseExtract
	if err := json.Unmarshal([]byte(proposal.Payload), &extract); err != nil {
		return "", 0, refuse("the extracted lease could not be read back")
	}
	if !isCalendarDate(extract.StartDateISO) {
		return "", 0, refuse("the lease start date is not a date this can file")
	}
	if extract.RentCents <= 0 {
		return "", 0, refuse("the rent is zero, so there is nothing to file")
	}

	unitID, err := p.unitFor(ctx, q, *proposal.PropertyID, extract.UnitLabel)
	if err != nil {
		return "", 0, err
	}

	var endDate *string
	if extract.EndDateISO != "" {
		if !isCalendarDate(extract.EndDateISO) {
			return "", 0, refuse("the lease end date is not a date this can file")
		}
		endDate = &extract.EndDateISO
	}

	overlapping, err := q.CountOverlappingLeases(ctx, sqlc.CountOverlappingLeasesParams{
		UnitID:    unitID,
		ExcludeID: 0,
		EndDate:   endDate,
		StartDate: &extract.StartDateISO,
	})
	if err != nil {
		return "", 0, err
	}
	if overlapping > 0 {
		return "", 0, refuse("that unit already has a lease covering those dates; end the current one first")
	}

	var dueDay *int64
	if extract.DueDay >= 1 && extract.DueDay <= 31 {
		dueDay = &extract.DueDay
	}
	deposit := domain.Money(extract.DepositCents)
	lateFee := domain.Money(extract.LateFeeCents)

	lease, err := q.CreateLease(ctx, sqlc.CreateLeaseParams{
		UnitID:       unitID,
		StartDate:    extract.StartDateISO,
		EndDate:      endDate,
		RentCents:    domain.Money(extract.RentCents),
		DepositCents: optionalMoney(deposit),
		DueDay:       dueDay,
		LateFeeCents: optionalMoney(lateFee),
		// Pending, not active, whatever the dates say. A lease reaches the
		// record because somebody forwarded a PDF; making it active is a
		// statement about the world that a person should make.
		Status:     "pending",
		DocumentID: documentID,
		Notes:      domain.Clip(extract.Notes, 500),
		CreatedAt:  p.stamp(),
		UpdatedAt:  p.stamp(),
	})
	if err != nil {
		return "", 0, err
	}

	for _, t := range extract.Tenants {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			continue
		}
		tenant, err := q.FindTenantByName(ctx, name)
		if store.NotFound(err) {
			tenant, err = q.CreateTenant(ctx, sqlc.CreateTenantParams{
				Name:      domain.Clip(name, 200),
				Email:     domain.Clip(t.Email, 200),
				Phone:     domain.Clip(t.Phone, 50),
				CreatedAt: p.stamp(),
				UpdatedAt: p.stamp(),
			})
		}
		if err != nil {
			return "", 0, err
		}
		if _, err := q.AddLeaseTenant(ctx, sqlc.AddLeaseTenantParams{
			LeaseID:   lease.ID,
			TenantID:  tenant.ID,
			Role:      role(t.Role),
			CreatedAt: p.stamp(),
			UpdatedAt: p.stamp(),
		}); err != nil && !store.Conflict(err) {
			return "", 0, err
		}
	}

	return "lease", lease.ID, nil
}

// fileInsurance writes or updates a policy.
//
// The same declaration page forwarded twice is one policy, so the carrier and
// the encrypted policy number are read before anything is written -- the
// discipline the whole ingestion pipeline follows on gmail_message_id.
func (p *Pipeline) fileInsurance(ctx context.Context, q *sqlc.Queries, proposal sqlc.IngestProposal, documentID *int64) (string, int64, error) {
	var extract llm.InsuranceExtract
	if err := json.Unmarshal([]byte(proposal.Payload), &extract); err != nil {
		return "", 0, refuse("the extracted policy could not be read back")
	}
	if extract.Carrier == "" {
		return "", 0, refuse("no carrier was found, so there is nothing to file this policy under")
	}

	number, err := p.seal(extract.PolicyNumber)
	if err != nil {
		return "", 0, err
	}

	existing, err := q.FindInsurancePolicy(ctx, sqlc.FindInsurancePolicyParams{
		PropertyID:      *proposal.PropertyID,
		Carrier:         extract.Carrier,
		PolicyNumberEnc: number,
	})
	switch {
	case err == nil:
		updated, err := q.UpdateInsurancePolicy(ctx, sqlc.UpdateInsurancePolicyParams{
			Carrier:                extract.Carrier,
			PolicyNumberEnc:        number,
			Type:                   policyType(extract.Type),
			AgentName:              domain.Clip(extract.AgentName, 200),
			AgentPhone:             domain.Clip(extract.AgentPhone, 50),
			AgentEmail:             domain.Clip(extract.AgentEmail, 200),
			EffectiveDate:          optionalDate(extract.EffectiveDateISO),
			ExpirationDate:         optionalDate(extract.ExpirationDateISO),
			AnnualPremiumCents:     optionalMoney(domain.Money(extract.AnnualPremiumCents)),
			DwellingCoverageCents:  optionalMoney(domain.Money(extract.DwellingCoverageCents)),
			LiabilityCoverageCents: optionalMoney(domain.Money(extract.LiabilityCoverageCents)),
			DeductibleCents:        optionalMoney(domain.Money(extract.DeductibleCents)),
			DocumentID:             documentID,
			Notes:                  domain.Clip(extract.Notes, 500),
			UpdatedAt:              p.stamp(),
			ID:                     existing.ID,
		})
		if err != nil {
			return "", 0, err
		}
		return "insurance_policy", updated.ID, nil
	case store.NotFound(err):
	default:
		return "", 0, err
	}

	policy, err := q.CreateInsurancePolicy(ctx, sqlc.CreateInsurancePolicyParams{
		PropertyID:             *proposal.PropertyID,
		Carrier:                extract.Carrier,
		PolicyNumberEnc:        number,
		Type:                   policyType(extract.Type),
		AgentName:              domain.Clip(extract.AgentName, 200),
		AgentPhone:             domain.Clip(extract.AgentPhone, 50),
		AgentEmail:             domain.Clip(extract.AgentEmail, 200),
		EffectiveDate:          optionalDate(extract.EffectiveDateISO),
		ExpirationDate:         optionalDate(extract.ExpirationDateISO),
		AnnualPremiumCents:     optionalMoney(domain.Money(extract.AnnualPremiumCents)),
		DwellingCoverageCents:  optionalMoney(domain.Money(extract.DwellingCoverageCents)),
		LiabilityCoverageCents: optionalMoney(domain.Money(extract.LiabilityCoverageCents)),
		DeductibleCents:        optionalMoney(domain.Money(extract.DeductibleCents)),
		DocumentID:             documentID,
		Notes:                  domain.Clip(extract.Notes, 500),
		CreatedAt:              p.stamp(),
		UpdatedAt:              p.stamp(),
	})
	if err != nil {
		return "", 0, err
	}
	return "insurance_policy", policy.ID, nil
}

// fileMortgageStatement appends a statement, creating the mortgage it belongs
// to if this is the first one to arrive.
//
// The statement table is append-only, which is what makes an amortization
// history a consequence of the write path rather than a feature to build. The
// running balance on the mortgage is a separate write, and one that refuses to
// go backwards when a statement arrives out of order.
func (p *Pipeline) fileMortgageStatement(ctx context.Context, q *sqlc.Queries, proposal sqlc.IngestProposal, documentID *int64) (string, int64, error) {
	var extract llm.MortgageStatementExtract
	if err := json.Unmarshal([]byte(proposal.Payload), &extract); err != nil {
		return "", 0, refuse("the extracted statement could not be read back")
	}
	if extract.Lender == "" {
		return "", 0, refuse("no lender was found, so there is nothing to file this statement under")
	}
	if !isCalendarDate(extract.StatementDateISO) {
		return "", 0, refuse("the statement date is not a date this can file")
	}

	mortgage, err := q.FindMortgageByLender(ctx, sqlc.FindMortgageByLenderParams{
		PropertyID: *proposal.PropertyID,
		Lender:     extract.Lender,
	})
	if store.NotFound(err) {
		number, sealErr := p.seal(extract.LoanNumber)
		if sealErr != nil {
			return "", 0, sealErr
		}
		mortgage, err = q.CreateMortgage(ctx, sqlc.CreateMortgageParams{
			PropertyID:    *proposal.PropertyID,
			Lender:        domain.Clip(extract.Lender, 200),
			LoanNumberEnc: number,
			CreatedAt:     p.stamp(),
			UpdatedAt:     p.stamp(),
		})
	}
	if err != nil {
		return "", 0, err
	}

	balance := optionalMoney(domain.Money(extract.PrincipalBalanceCents))
	statement, err := q.CreateMortgageStatement(ctx, sqlc.CreateMortgageStatementParams{
		MortgageID:            mortgage.ID,
		StatementDate:         extract.StatementDateISO,
		PrincipalBalanceCents: balance,
		PaymentCents:          optionalMoney(domain.Money(extract.PaymentCents)),
		PrincipalPaidCents:    optionalMoney(domain.Money(extract.PrincipalPaidCents)),
		InterestPaidCents:     optionalMoney(domain.Money(extract.InterestPaidCents)),
		EscrowPaidCents:       optionalMoney(domain.Money(extract.EscrowPaidCents)),
		DocumentID:            documentID,
		CreatedAt:             p.stamp(),
		UpdatedAt:             p.stamp(),
	})
	if store.Conflict(err) {
		return "", 0, refuse("that statement is already on file")
	}
	if err != nil {
		return "", 0, err
	}

	if balance != nil {
		if err := q.SetMortgageBalance(ctx, sqlc.SetMortgageBalanceParams{
			CurrentBalanceCents: balance,
			BalanceAsOf:         &extract.StatementDateISO,
			UpdatedAt:           p.stamp(),
			ID:                  mortgage.ID,
		}); err != nil {
			return "", 0, err
		}
	}

	return "mortgage_statement", statement.ID, nil
}

// unitFor picks the unit a lease hangs off.
//
// Every lease hangs off a unit and every property keeps at least one, so there
// is always something to choose from. A property with one unit takes it
// whatever the document calls it; a property with several needs the label to
// match, because putting a tenancy on the wrong apartment is worse than
// waiting for somebody to say which.
func (p *Pipeline) unitFor(ctx context.Context, q *sqlc.Queries, propertyID int64, label string) (int64, error) {
	units, err := q.ListUnitsByProperty(ctx, propertyID)
	if err != nil {
		return 0, err
	}
	switch len(units) {
	case 0:
		return 0, refuse("that property has no units, so a lease has nothing to hang off")
	case 1:
		return units[0].ID, nil
	}

	label = strings.TrimSpace(label)
	if label == "" {
		return 0, refuse("that property has %d units and the lease does not say which", len(units))
	}
	for _, u := range units {
		if strings.EqualFold(strings.TrimSpace(u.Label), label) {
			return u.ID, nil
		}
	}
	return 0, refuse("that property has no unit called %q", label)
}

// enclosureID is the document a proposal is about, where there is one.
//
// The first stored attachment: a forwarded receipt has one enclosure, and the
// screen shows it beside the fields. A message with several is unusual and the
// review screen lists all of them; the link recorded here is the one the
// record points back at.
func (p *Pipeline) enclosureID(ctx context.Context, q *sqlc.Queries, messageID int64) *int64 {
	attachments, err := q.ListEmailAttachments(ctx, messageID)
	if err != nil {
		p.log.Error("could not read the enclosures", "email_message_id", messageID, "error", err)
		return nil
	}
	for _, att := range attachments {
		if att.DocumentID != nil {
			return att.DocumentID
		}
	}
	return nil
}

// audit records what was done, inside the transaction that did it.
func (p *Pipeline) audit(ctx context.Context, q *sqlc.Queries, actor Actor, userID *int64,
	action, entityType string, entityID int64, before, after sqlc.IngestProposal) error {
	beforeJSON, err := json.Marshal(before)
	if err != nil {
		return fmt.Errorf("ingest: encode the audit snapshot: %w", err)
	}
	afterJSON, err := json.Marshal(after)
	if err != nil {
		return fmt.Errorf("ingest: encode the audit snapshot: %w", err)
	}

	_, err = q.RecordAudit(ctx, sqlc.RecordAuditParams{
		UserID:     userID,
		Actor:      actor,
		At:         p.stamp(),
		Action:     action,
		EntityType: entityType,
		EntityID:   &entityID,
		Before:     string(beforeJSON),
		After:      string(afterJSON),
		CreatedAt:  p.stamp(),
		UpdatedAt:  p.stamp(),
	})
	return err
}

// seal encrypts a policy or loan number for storage (§9.2).
//
// An empty value stays empty rather than becoming ciphertext of nothing:
// FindInsurancePolicy matches on this column, and every unnumbered policy
// hashing to a different value would file each one twice.
func (p *Pipeline) seal(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || p.box == nil {
		return "", nil
	}
	sealed, err := p.box.SealString(value)
	if err != nil {
		return "", fmt.Errorf("ingest: encrypt an account number: %w", err)
	}
	return sealed, nil
}

// summarize is the ledger description for a receipt with no note of its own.
func summarize(r llm.ReceiptExtract) string {
	if r.VendorName != "" {
		return r.VendorName + " receipt"
	}
	return "Forwarded receipt"
}

// category maps an extracted category onto the ledger's own set. Anything
// unrecognised lands in other rather than being refused: the entry is right
// and only its filing is uncertain, and a person is looking at it.
func category(name string) string {
	switch name {
	case "repair", "capex", "utilities", "insurance", "property_tax", "hoa", "mgmt_fee":
		return name
	default:
		return "other"
	}
}

func policyType(name string) string {
	switch name {
	case "hazard", "flood", "umbrella", "liability":
		return name
	default:
		return "hazard"
	}
}

func role(name string) string {
	switch name {
	case "primary", "cosigner", "occupant":
		return name
	default:
		return "occupant"
	}
}

// optionalMoney keeps "not stated" and "zero" apart, which is the distinction
// every nullable money column in this schema exists to hold.
func optionalMoney(m domain.Money) *domain.Money {
	if m == 0 {
		return nil
	}
	return &m
}

func optionalDate(s string) *string {
	if !isCalendarDate(s) {
		return nil
	}
	return &s
}

// isCalendarDate checks the shape and the calendar, which is all a date off a
// document has to satisfy before it is stored.
func isCalendarDate(s string) bool {
	if len(s) != len("2006-01-02") {
		return false
	}
	_, err := time.Parse(time.DateOnly, s)
	return err == nil
}

// said turns a status into the word an operator reads in a refusal.
func said(status string) string {
	if status == "auto_applied" {
		return "filed automatically"
	}
	return status
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
