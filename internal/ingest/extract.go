package ingest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/llm"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// Extract fills in a proposal's payload from the document behind it.
//
// Everything the model returns is checked in Go before it is stored. §5.3 asks
// for exactly that -- a lease starting in 1970 or a receipt dated next year is
// a misread, and catching it here is what keeps the review screen a place to
// confirm rather than a place to find mistakes.
func (p *Pipeline) Extract(ctx context.Context, proposalID int64) error {
	if !p.Reads() {
		return ErrNoReader
	}
	proposal, err := p.repo.Read().GetProposal(ctx, proposalID)
	if store.NotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ingest: read proposal %d: %w", proposalID, err)
	}

	// Already settled, or already read. Both make a second run a no-op, which
	// matters because an extraction is the most expensive thing this process
	// does and the queue is at-least-once.
	if proposal.Status != "pending" || proposal.Payload != "{}" {
		return nil
	}

	files, err := p.enclosures(ctx, proposal.EmailMessageID)
	if err != nil {
		return err
	}
	msg, err := p.repo.Read().GetEmailMessage(ctx, proposal.EmailMessageID)
	if err != nil {
		return fmt.Errorf("ingest: read message %d: %w", proposal.EmailMessageID, err)
	}

	extracted, usage, err := p.reader.Extract(ctx, proposal.Kind, llm.Input{Text: messageText(msg), Files: files})
	if errors.Is(err, llm.ErrNoExtractor) {
		// The classifier named a kind this milestone does not take apart. The
		// proposal stands, the document is filed, and a person decides.
		p.setMessage(ctx, proposal.EmailMessageID, "needs_review", "")
		return nil
	}
	if err != nil {
		return fmt.Errorf("ingest: extract proposal %d: %w", proposalID, err)
	}

	problem := validate(extracted, p.now())
	payload, err := payloadJSON(extracted)
	if err != nil {
		return err
	}

	// The model's own address guess is better evidence than the classifier's:
	// it read the document rather than the covering email. Re-matching on it
	// can turn an unmatched proposal into a matched one, and it is also where
	// the outcome auto-apply needs comes from -- which is why it is recomputed
	// here rather than carried on the row. A stored verdict would be a second
	// copy of something the addresses already say.
	propertyID, outcome := proposal.PropertyID, NoMatch
	hint := addressGuess(extracted)
	if hint == "" {
		hint = proposal.PropertyHint
	}
	if matched, o := p.match(ctx, hint); o.Matched() {
		propertyID, outcome = matched, o
	}

	updated, err := p.repo.Write().RecordProposalExtract(ctx, sqlc.RecordProposalExtractParams{
		Payload:          payload,
		LlmModel:         p.reader.Model(),
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		Confidence:       proposal.Confidence,
		PropertyID:       propertyID,
		Error:            domain.Clip(problem, 500),
		UpdatedAt:        p.stamp(),
		ID:               proposalID,
	})
	if err != nil {
		return fmt.Errorf("ingest: record the extraction for proposal %d: %w", proposalID, err)
	}

	if p.autoApplies(updated, problem, outcome) {
		if _, err := p.Apply(ctx, proposalID, ActorSystem, nil); err != nil {
			// An auto-apply that will not go through is not an error to retry:
			// the proposal is intact, the reason is on the row, and it is now
			// what it would have been anyway -- something for a person.
			p.log.Warn("could not file a proposal automatically",
				"proposal_id", proposalID, "error", err)
			p.setMessage(ctx, proposal.EmailMessageID, "needs_review", "")
			return nil
		}
		return nil
	}

	p.setMessage(ctx, proposal.EmailMessageID, "needs_review", "")
	return nil
}

// autoApplies is §5.4's one exception to the review gate, and all of it.
//
// Three conditions, together: a receipt, a confidence at or above the
// threshold, and a property matched without ambiguity. Anything the validation
// complained about disqualifies it too -- a figure the record already doubts
// is not a figure to file unread.
//
// A row filed this way still carries needs_review on the transaction, so the
// ledger says out loud that nobody has checked it.
func (p *Pipeline) autoApplies(proposal sqlc.IngestProposal, problem string, outcome Outcome) bool {
	if !p.cfg.AutoApply || problem != "" {
		return false
	}
	if proposal.Kind != "receipt" {
		return false
	}
	if proposal.Confidence == nil || *proposal.Confidence < p.cfg.AutoApplyConfidence {
		return false
	}
	// Unambiguous means the folded addresses agreed in full. A street-only
	// match is a fine thing to show an operator and a weak thing to file
	// unread.
	return proposal.PropertyID != nil && outcome.Unambiguous()
}

// addressGuess reads whichever address field an extract carries.
func addressGuess(extracted any) string {
	switch v := extracted.(type) {
	case llm.ReceiptExtract:
		return v.AddressGuess
	case llm.LeaseExtract:
		return v.AddressGuess
	case llm.InsuranceExtract:
		return v.AddressGuess
	case llm.MortgageStatementExtract:
		return v.AddressGuess
	}
	return ""
}

// validate checks an extraction against what is possible, and returns the
// first complaint or an empty string.
//
// It is deliberately about plausibility rather than correctness. Whether a
// repair really cost $482.19 is the operator's question; whether a date parses
// and falls in this century is not.
func validate(extracted any, now time.Time) string {
	switch v := extracted.(type) {
	case llm.ReceiptExtract:
		if problem := checkDate("the transaction date", v.DateISO, true, now); problem != "" {
			return problem
		}
		if v.TotalCents <= 0 {
			return "the total came back as zero or negative; a receipt is a magnitude"
		}
		if problem := checkAmount("the total", v.TotalCents); problem != "" {
			return problem
		}
	case llm.LeaseExtract:
		if problem := checkDate("the start date", v.StartDateISO, false, now); problem != "" {
			return problem
		}
		if v.EndDateISO != "" {
			if problem := checkDate("the end date", v.EndDateISO, false, now); problem != "" {
				return problem
			}
			if v.EndDateISO < v.StartDateISO {
				return "the lease ends before it starts"
			}
		}
		if v.RentCents <= 0 {
			return "the rent came back as zero"
		}
		if v.DueDay < 0 || v.DueDay > 31 {
			return "the rent due day is not a day of the month"
		}
	case llm.InsuranceExtract:
		if problem := checkDate("the effective date", v.EffectiveDateISO, false, now); problem != "" {
			return problem
		}
		if problem := checkDate("the expiration date", v.ExpirationDateISO, false, now); problem != "" {
			return problem
		}
		if v.Carrier == "" {
			return "no carrier was named"
		}
	case llm.MortgageStatementExtract:
		if problem := checkDate("the statement date", v.StatementDateISO, true, now); problem != "" {
			return problem
		}
		if v.Lender == "" {
			return "no lender was named"
		}
	}
	return ""
}

// checkDate rejects a date that does not parse or that could not have been on
// the document.
//
// `past` marks a date that describes something that already happened. A
// receipt dated next year is a misread of the year; a lease that starts next
// year is a renewal, and refusing it would refuse the most useful thing anyone
// forwards.
func checkDate(what, value string, past bool, now time.Time) string {
	if value == "" {
		if past {
			return "no " + strings.TrimPrefix(what, "the ") + " was found"
		}
		return ""
	}
	on, err := time.Parse(time.DateOnly, value)
	if err != nil {
		return what + " is not a date: " + value
	}
	// Nothing this application holds predates the modern mortgage market, and
	// 1970 is what a misread epoch looks like.
	if on.Year() < 1980 || on.Year() > now.Year()+30 {
		return what + " is not plausible: " + value
	}
	if past && on.After(now.AddDate(0, 0, 2)) {
		// Two days of slack: a timezone at the far side of the world, not a
		// year read wrong.
		return what + " is in the future: " + value
	}
	return ""
}

// checkAmount rejects a figure large enough to be a decimal point in the wrong
// place. Ten million dollars is not a receipt.
func checkAmount(what string, cents int64) string {
	const ceiling = 1_000_000_00
	if cents > ceiling {
		return what + " is implausibly large: " + domain.Money(cents).String()
	}
	return ""
}
