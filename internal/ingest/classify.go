package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/llm"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// Classify reads one message and files the proposal for it.
//
// The proposal is written here rather than after extraction, and an `unknown`
// classification gets one too. Two reasons: "we could not tell what this is"
// is a fact the operator needs and the enclosure is already on file either
// way, and every call this pipeline makes then lands on a row, which is what
// lets the budget breaker read one column instead of keeping a ledger of its
// own.
func (p *Pipeline) Classify(ctx context.Context, messageID int64) error {
	msg, err := p.repo.Read().GetEmailMessage(ctx, messageID)
	if store.NotFound(err) {
		// Deleted while this sat in the queue. Nothing to read.
		return nil
	}
	if err != nil {
		return fmt.Errorf("ingest: read message %d: %w", messageID, err)
	}

	// Both of these make a second run a no-op, which the queue needs: it is
	// at-least-once by construction, and a process killed after the LLM call
	// and before the row was marked done will come back here.
	if msg.Status != "received" && msg.Status != "parsing" {
		return nil
	}
	if _, err := p.repo.Read().GetProposalByMessage(ctx, messageID); err == nil {
		return nil
	} else if !store.NotFound(err) {
		return fmt.Errorf("ingest: look for an existing proposal: %w", err)
	}

	files, err := p.enclosures(ctx, messageID)
	if err != nil {
		return err
	}

	p.setMessage(ctx, messageID, "parsing", "")

	said, usage, err := p.reader.Classify(ctx, llm.Input{Text: messageText(msg), Files: files})
	if err != nil {
		// Back to received rather than left in parsing. A job that runs out of
		// attempts leaves the row as it found it, and the sweep is then what
		// picks the message up again -- the same reliability argument the
		// Gmail poller rests on.
		p.setMessage(ctx, messageID, "received", err.Error())
		return fmt.Errorf("ingest: classify message %d: %w", messageID, err)
	}

	kind := said.Kind
	if kind == "" {
		kind = "unknown"
	}
	propertyID, outcome := p.match(ctx, said.PropertyHint)
	confidence := said.Confidence

	proposal, err := p.repo.Write().CreateProposal(ctx, sqlc.CreateProposalParams{
		EmailMessageID:   messageID,
		Kind:             kind,
		Payload:          "{}",
		LlmModel:         p.reader.Model(),
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		Confidence:       &confidence,
		PropertyHint:     domain.Clip(said.PropertyHint, 300),
		PropertyID:       propertyID,
		Reasoning:        domain.Clip(matchNote(said.Reasoning, outcome), 500),
		Status:           "pending",
		CreatedAt:        p.stamp(),
		UpdatedAt:        p.stamp(),
	})
	if err != nil {
		p.setMessage(ctx, messageID, "received", err.Error())
		return fmt.Errorf("ingest: file a proposal for message %d: %w", messageID, err)
	}

	p.log.Info("classified a message",
		"email_message_id", messageID, "proposal_id", proposal.ID,
		"kind", kind, "confidence", confidence, "property_match", outcome)

	if !llm.HasExtractor(kind) {
		// Nothing to fill in, and that is a finished state rather than a
		// failure: the document is filed, the proposal names what it is, and a
		// person decides what to do with it.
		p.setMessage(ctx, messageID, "needs_review", "")
		return nil
	}

	added, err := p.queue.EnqueueOnce(ctx, KindExtract, extractKey(proposal.ID), extractPayload{ProposalID: proposal.ID})
	if err != nil {
		return fmt.Errorf("ingest: queue an extract for proposal %d: %w", proposal.ID, err)
	}
	if added {
		p.notify()
	}
	return nil
}

// messageText is what the model is shown besides the enclosures.
//
// Subject and body, and nothing about the sender. The allowlist has already
// decided the sender is trusted enough to process; telling the model who sent
// something invites it to classify by sender rather than by content, which is
// exactly what the system prompt tells it not to do.
func messageText(msg sqlc.EmailMessage) string {
	var b strings.Builder
	if msg.Subject != "" {
		b.WriteString("Subject: ")
		b.WriteString(msg.Subject)
		b.WriteString("\n\n")
	}
	b.WriteString(msg.Snippet)
	return b.String()
}

// match resolves the model's address string against the portfolio.
//
// A read that fails is not a match failure: it leaves the proposal unmatched,
// which routes it to a person, which is where an unmatched proposal was going
// anyway.
func (p *Pipeline) match(ctx context.Context, hint string) (*int64, Outcome) {
	if strings.TrimSpace(hint) == "" {
		return nil, NoAddress
	}
	candidates, err := p.repo.Read().ListPropertyMatchKeys(ctx)
	if err != nil {
		p.log.Error("could not read the properties to match against", "error", err)
		return nil, NoMatch
	}
	return MatchProperty(candidates, hint)
}

// matchNote puts the model's sentence and the matcher's verdict on one line,
// so the review screen can say why a proposal is against the property it is
// against -- or why it is against none.
func matchNote(reasoning string, outcome Outcome) string {
	switch outcome {
	case NoAddress:
		return reasoning
	case Ambiguous:
		return reasoning + " (the address fits more than one property, so none was chosen)"
	case NoMatch:
		return reasoning + " (no property matched the address)"
	case MatchedOnStreet:
		return reasoning + " (matched on the street line)"
	case MatchedApproximately:
		return reasoning + " (matched approximately)"
	default:
		return reasoning
	}
}

// payloadJSON encodes an extract for the payload column.
func payloadJSON(v any) (string, error) {
	encoded, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("ingest: encode the extraction: %w", err)
	}
	return string(encoded), nil
}
