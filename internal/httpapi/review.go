package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/ingest"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// The review queue: what a model claimed, waiting for somebody to agree.
//
// This is the screen the whole email pipeline exists to feed (docs/DESIGN.md
// §7.2), and the endpoint behind it is deliberately plain: a keyset page, one
// proposal with its enclosures, a PATCH that corrects what was read, and two
// verbs. The enclosure itself is served by the existing document handler --
// adding a second way to serve bytes would step outside the inline-type
// allowlist that handler exists to enforce.

// proposalStatuses is the column's own CHECK, in the column's own order.
var proposalStatuses = []string{"pending", "approved", "rejected", "auto_applied"}

type proposalResponse struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	Kind   string `json:"kind"`
	// Payload is the extraction, verbatim. Its shape depends on the kind, and
	// the screen renders it field by field rather than the API flattening four
	// shapes into one.
	Payload json.RawMessage `json:"payload"`
	// Confidence is how sure the model was. It is a margin mark on the screen,
	// not a stamp: the stamp says where a thing stands, and this is a property
	// of the reading.
	Confidence *float64 `json:"confidence"`
	// PropertyHint is what the document said, verbatim, and PropertyID is what
	// the folding matched it to. Both, so the screen can say why a proposal is
	// against the property it is against -- or why it is against none.
	PropertyHint     string  `json:"property_hint"`
	PropertyID       *int64  `json:"property_id"`
	PropertyNickname *string `json:"property_nickname"`
	Reasoning        string  `json:"reasoning"`
	// Error is why an apply was refused, when one was.
	Error string `json:"error"`

	LLMModel         string `json:"llm_model"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`

	EmailMessageID    int64   `json:"email_message_id"`
	ReviewedBy        *int64  `json:"reviewed_by"`
	ReviewedAt        *string `json:"reviewed_at"`
	AppliedEntityType *string `json:"applied_entity_type"`
	AppliedEntityID   *int64  `json:"applied_entity_id"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// proposalListItem is one line of the register: enough to scan, and the
// message facts that put it in a day.
type proposalListItem struct {
	proposalResponse
	Subject    string `json:"subject"`
	FromAddr   string `json:"from_addr"`
	ReceivedAt string `json:"received_at"`
	Enclosures int    `json:"enclosures"`
}

type proposalList struct {
	Items []proposalListItem `json:"items"`
	// NextCursor is absent on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
	// Counts is the register's tally, by status.
	Counts map[string]int64 `json:"counts"`
}

// proposalDetail is one slip: the proposal, the message it came off, and every
// enclosure the screen can render beside the fields.
type proposalDetail struct {
	proposalResponse
	Subject    string                 `json:"subject"`
	FromAddr   string                 `json:"from_addr"`
	ReceivedAt string                 `json:"received_at"`
	Snippet    string                 `json:"snippet"`
	Enclosures []proposalEnclosure    `json:"enclosures"`
	Property   *propertyResponse      `json:"property"`
	Properties []proposalPropertyName `json:"properties"`
}

// proposalEnclosure is one attachment, as something to look at.
type proposalEnclosure struct {
	ID         int64  `json:"id"`
	DocumentID *int64 `json:"document_id"`
	Filename   string `json:"filename"`
	Mime       string `json:"mime"`
	SizeBytes  int64  `json:"size_bytes"`
	// SkippedReason is why the bytes were not taken, when they were not.
	SkippedReason string `json:"skipped_reason"`
}

// proposalPropertyName is the portfolio, for the picker that corrects a match.
//
// The matcher is deterministic and it is sometimes wrong; when it is, the
// operator has to be able to say which building this is, and a list of names
// is all that takes.
type proposalPropertyName struct {
	ID       int64  `json:"id"`
	Nickname string `json:"nickname"`
	Address  string `json:"address"`
}

func newProposalResponse(p sqlc.IngestProposal) proposalResponse {
	payload := json.RawMessage(p.Payload)
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	return proposalResponse{
		ID: p.ID, Status: p.Status, Kind: p.Kind, Payload: payload,
		Confidence:   p.Confidence,
		PropertyHint: p.PropertyHint, PropertyID: p.PropertyID,
		Reasoning: p.Reasoning, Error: p.Error,
		LLMModel: p.LlmModel, PromptTokens: p.PromptTokens, CompletionTokens: p.CompletionTokens,
		EmailMessageID: p.EmailMessageID,
		ReviewedBy:     p.ReviewedBy, ReviewedAt: p.ReviewedAt,
		AppliedEntityType: p.AppliedEntityType, AppliedEntityID: p.AppliedEntityID,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

// recProposal names a proposal for the shared error answers. The operator's
// word and the table's differ, so both readers get their own.
var recProposal = record{noun: "proposal", table: "ingest proposal"}

func (s *server) routeReview(mux *http.ServeMux) {
	route(mux, "/api/v1/review", methods{
		http.MethodGet: s.guarded(s.handleListProposals),
	})
	route(mux, "/api/v1/review/{id}", methods{
		http.MethodGet:   s.guarded(s.handleGetProposal),
		http.MethodPatch: s.guarded(s.handleUpdateProposal),
	})
	route(mux, "/api/v1/review/{id}/approve", methods{
		http.MethodPost: s.guarded(s.handleApproveProposal),
	})
	route(mux, "/api/v1/review/{id}/reject", methods{
		http.MethodPost: s.guarded(s.handleRejectProposal),
	})
}

func (s *server) handleListProposals(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	size, ok := pageSize(w, r)
	if !ok {
		return
	}

	// The queue defaults to what is waiting, because that is what the screen
	// is for. `status=all` is how a reader looks at what has already been
	// decided.
	var status *string
	switch raw := strings.TrimSpace(r.URL.Query().Get("status")); raw {
	case "", "pending":
		pending := "pending"
		status = &pending
	case "all":
	default:
		if !slices.Contains(proposalStatuses, raw) {
			WriteProblem(w, r, http.StatusBadRequest,
				"status has to be all or one of "+strings.Join(proposalStatuses, ", ")+".")
			return
		}
		status = &raw
	}

	rows, err := s.proposalPage(ctx, r.URL.Query().Get("cursor"), status, size+1)
	if err != nil {
		if errors.Is(err, errBadCursor) {
			WriteProblem(w, r, http.StatusBadRequest, "The cursor is not one this endpoint issued.")
			return
		}
		loggerFrom(ctx).Error("list ingest proposals", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not read the review queue.")
		return
	}

	counts, err := s.repo.Read().CountProposalsByStatus(ctx)
	if err != nil {
		loggerFrom(ctx).Error("count ingest proposals", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not read the review queue.")
		return
	}
	tally := make(map[string]int64, len(counts))
	for _, c := range counts {
		tally[c.Status] = c.Count
	}

	out := proposalList{Items: make([]proposalListItem, 0, size), Counts: tally}
	for i, row := range rows {
		if i == size {
			last := rows[i-1]
			out.NextCursor = encodeCursor(last.CreatedAt, last.ID)
			break
		}
		out.Items = append(out.Items, s.proposalLine(ctx, row))
	}

	writeJSON(w, r, http.StatusOK, out)
}

// proposalPage fetches one page of the register.
func (s *server) proposalPage(ctx context.Context, cursor string, status *string, size int) ([]sqlc.IngestProposal, error) {
	if cursor == "" {
		return s.repo.Read().ListProposalsFirstPage(ctx, sqlc.ListProposalsFirstPageParams{
			Status:   status,
			PageSize: int64(size),
		})
	}
	createdAt, id, err := decodeCursor(cursor)
	if err != nil {
		return nil, err
	}
	return s.repo.Read().ListProposalsAfter(ctx, sqlc.ListProposalsAfterParams{
		Status:         status,
		AfterCreatedAt: createdAt,
		AfterID:        id,
		PageSize:       int64(size),
	})
}

// proposalLine dresses one row with the message facts the register shows.
//
// Two reads per line rather than a join: the page is fifty rows at most, the
// reads are by primary key, and a join here would fork the row type every
// other query in this file returns.
func (s *server) proposalLine(ctx context.Context, p sqlc.IngestProposal) proposalListItem {
	item := proposalListItem{proposalResponse: newProposalResponse(p)}

	if msg, err := s.repo.Read().GetEmailMessage(ctx, p.EmailMessageID); err == nil {
		item.Subject = msg.Subject
		item.FromAddr = msg.FromAddr
		item.ReceivedAt = msg.ReceivedAt
	} else {
		// The register is kept by day, and a line with no day has nowhere to
		// sit. The proposal's own timestamp is the honest fallback.
		item.ReceivedAt = p.CreatedAt
	}
	if attachments, err := s.repo.Read().ListEmailAttachments(ctx, p.EmailMessageID); err == nil {
		item.Enclosures = len(attachments)
	}
	if p.PropertyID != nil {
		if property, err := s.repo.Read().GetProperty(ctx, *p.PropertyID); err == nil {
			item.PropertyNickname = &property.Nickname
		}
	}
	return item
}

func (s *server) handleGetProposal(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ctx := r.Context()

	proposal, err := s.repo.Read().GetProposal(ctx, id)
	if err != nil {
		s.readError(w, r, recProposal, err)
		return
	}

	out := proposalDetail{proposalResponse: newProposalResponse(proposal)}

	if msg, err := s.repo.Read().GetEmailMessage(ctx, proposal.EmailMessageID); err == nil {
		out.Subject = msg.Subject
		out.FromAddr = msg.FromAddr
		out.ReceivedAt = msg.ReceivedAt
		out.Snippet = msg.Snippet
	}

	attachments, err := s.repo.Read().ListEmailAttachments(ctx, proposal.EmailMessageID)
	if err != nil {
		loggerFrom(ctx).Error("read the enclosures", "error", err, "proposal_id", id)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not read the proposal.")
		return
	}
	out.Enclosures = make([]proposalEnclosure, 0, len(attachments))
	for _, att := range attachments {
		out.Enclosures = append(out.Enclosures, proposalEnclosure{
			ID: att.ID, DocumentID: att.DocumentID, Filename: att.Filename,
			Mime: att.Mime, SizeBytes: att.SizeBytes, SkippedReason: att.SkippedReason,
		})
	}

	if proposal.PropertyID != nil {
		if property, err := s.repo.Read().GetProperty(ctx, *proposal.PropertyID); err == nil {
			response := newPropertyResponse(property)
			out.Property = &response
			out.PropertyNickname = &property.Nickname
		}
	}

	// The portfolio rides along so the screen can offer the picker that
	// corrects a match without a second round trip. It is tens of rows.
	keys, err := s.repo.Read().ListPropertyMatchKeys(ctx)
	if err != nil {
		loggerFrom(ctx).Error("read the properties", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not read the proposal.")
		return
	}
	out.Properties = make([]proposalPropertyName, 0, len(keys))
	for _, k := range keys {
		out.Properties = append(out.Properties, proposalPropertyName{
			ID: k.ID, Nickname: k.Nickname, Address: k.AddressLine1,
		})
	}

	writeJSON(w, r, http.StatusOK, out)
}

var proposalPatchFields = []string{"kind", "payload", "property_id"}

// handleUpdateProposal corrects what was read, before anybody agrees to it.
//
// Only a pending proposal can be corrected. Once it has been filed the record
// it produced is the thing to amend, and editing the proposal afterwards would
// leave two accounts of the same fact disagreeing.
func (s *server) handleUpdateProposal(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	p, ok := decodePatch(w, r, proposalPatchFields...)
	if !ok {
		return
	}
	ctx := r.Context()

	var updated sqlc.IngestProposal
	err := s.repo.Tx(ctx, func(q *sqlc.Queries) error {
		current, err := q.GetProposal(ctx, id)
		if err != nil {
			return err
		}
		if current.Status != "pending" {
			return validationError{"This proposal has already been settled, so its record is the thing to amend."}
		}

		if err := p.apply(
			required("kind", &current.Kind),
			nullable("property_id", &current.PropertyID),
		); err != nil {
			return err
		}

		// payload is not bound above because the column is TEXT and the field
		// is an object: unmarshalling it into a string would fail on every
		// well-formed body. It is taken verbatim instead, which is also what
		// keeps the shape the kind's own -- this endpoint does not know what a
		// receipt looks like, and should not have to.
		if raw, sent := p["payload"]; sent {
			if isNull(raw) || !json.Valid(raw) {
				return validationError{"payload has to be an object holding the corrected fields."}
			}
			current.Payload = string(raw)
		}

		if !slices.Contains(proposalKinds, current.Kind) {
			return validationError{"kind has to be one of " + strings.Join(proposalKinds, ", ") + "."}
		}

		updated, err = q.UpdateProposal(ctx, sqlc.UpdateProposalParams{
			Kind:       current.Kind,
			Payload:    current.Payload,
			PropertyID: current.PropertyID,
			UpdatedAt:  timestamp(),
			ID:         id,
		})
		return err
	})
	if err != nil {
		if store.ForeignKey(err) {
			s.danglingReference(w, r, recProposal)
			return
		}
		s.writeError(w, r, recProposal, err)
		return
	}
	writeJSON(w, r, http.StatusOK, newProposalResponse(updated))
}

// proposalKinds is the column's own CHECK, in the column's own order.
var proposalKinds = []string{
	"receipt", "lease", "insurance", "mortgage_statement",
	"repair", "valuation", "note", "unknown",
}

func (s *server) handleApproveProposal(w http.ResponseWriter, r *http.Request) {
	s.settleProposal(w, r, func(id int64) (sqlc.IngestProposal, error) {
		return s.ingest.Apply(r.Context(), id, ingest.ActorWeb, userID(r))
	})
}

func (s *server) handleRejectProposal(w http.ResponseWriter, r *http.Request) {
	s.settleProposal(w, r, func(id int64) (sqlc.IngestProposal, error) {
		return s.ingest.Reject(r.Context(), id, userID(r))
	})
}

// settleProposal is the half approve and reject share: the id, the pipeline's
// absence, and the three ways a settlement can fail.
//
// A refusal is a 409. It is not the caller's request being malformed and it is
// not a fault in the server: the proposal cannot be filed as things stand, and
// the reason says what to change -- end the current lease, pick a property,
// look at the one that is already on file.
func (s *server) settleProposal(w http.ResponseWriter, r *http.Request, settle func(int64) (sqlc.IngestProposal, error)) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	// Deciding does not need a model. A proposal already on file can be
	// approved on a host whose llm.provider was since blanked -- but the
	// pipeline is what owns the apply path, so without it there is nothing to
	// call.
	if s.ingest == nil {
		WriteProblem(w, r, http.StatusServiceUnavailable, "Reading forwarded mail is not configured.")
		return
	}

	settled, err := settle(id)
	var refusal ingest.Refusal
	switch {
	case err == nil:
		writeJSON(w, r, http.StatusOK, newProposalResponse(settled))
	case errors.As(err, &refusal):
		WriteProblem(w, r, http.StatusConflict, capitalize(refusal.Reason)+".")
	default:
		s.writeError(w, r, recProposal, err)
	}
}

// capitalize makes a refusal read as a sentence. The reasons are written as
// clauses so they can also be shown in the margin of a register line.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + strings.TrimSuffix(s[1:], ".")
}

// Insurance and mortgages ---------------------------------------------------

// An applied insurance or mortgage proposal writes a row, and without these
// nothing can show it. They are read-only: the ingestion pipeline is what
// creates them at M4, and manual entry arrives with the screens that need it.

type insurancePolicyResponse struct {
	ID                     int64         `json:"id"`
	PropertyID             int64         `json:"property_id"`
	Carrier                string        `json:"carrier"`
	Type                   string        `json:"type"`
	AgentName              string        `json:"agent_name"`
	AgentPhone             string        `json:"agent_phone"`
	AgentEmail             string        `json:"agent_email"`
	EffectiveDate          *string       `json:"effective_date"`
	ExpirationDate         *string       `json:"expiration_date"`
	AnnualPremiumCents     *domain.Money `json:"annual_premium_cents"`
	DwellingCoverageCents  *domain.Money `json:"dwelling_coverage_cents"`
	LiabilityCoverageCents *domain.Money `json:"liability_coverage_cents"`
	DeductibleCents        *domain.Money `json:"deductible_cents"`
	DocumentID             *int64        `json:"document_id"`
	Notes                  string        `json:"notes"`
	CreatedAt              string        `json:"created_at"`
	UpdatedAt              string        `json:"updated_at"`
}

type insuranceList struct {
	Items []insurancePolicyResponse `json:"items"`
}

// The policy number is never on the wire. It is encrypted at rest under §9.2,
// and a screen that lists policies has no use for it that is worth decrypting
// it for.
func newInsurancePolicyResponse(p sqlc.InsurancePolicy) insurancePolicyResponse {
	return insurancePolicyResponse{
		ID: p.ID, PropertyID: p.PropertyID, Carrier: p.Carrier, Type: p.Type,
		AgentName: p.AgentName, AgentPhone: p.AgentPhone, AgentEmail: p.AgentEmail,
		EffectiveDate: p.EffectiveDate, ExpirationDate: p.ExpirationDate,
		AnnualPremiumCents:     p.AnnualPremiumCents,
		DwellingCoverageCents:  p.DwellingCoverageCents,
		LiabilityCoverageCents: p.LiabilityCoverageCents,
		DeductibleCents:        p.DeductibleCents,
		DocumentID:             p.DocumentID, Notes: p.Notes,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

type mortgageStatementResponse struct {
	ID                    int64         `json:"id"`
	StatementDate         string        `json:"statement_date"`
	PrincipalBalanceCents *domain.Money `json:"principal_balance_cents"`
	PaymentCents          *domain.Money `json:"payment_cents"`
	PrincipalPaidCents    *domain.Money `json:"principal_paid_cents"`
	InterestPaidCents     *domain.Money `json:"interest_paid_cents"`
	EscrowPaidCents       *domain.Money `json:"escrow_paid_cents"`
	DocumentID            *int64        `json:"document_id"`
	CreatedAt             string        `json:"created_at"`
}

type mortgageResponse struct {
	ID                     int64                       `json:"id"`
	PropertyID             int64                       `json:"property_id"`
	Lender                 string                      `json:"lender"`
	OriginalPrincipalCents *domain.Money               `json:"original_principal_cents"`
	InterestRateBps        *int64                      `json:"interest_rate_bps"`
	TermMonths             *int64                      `json:"term_months"`
	OriginationDate        *string                     `json:"origination_date"`
	MonthlyPiCents         *domain.Money               `json:"monthly_pi_cents"`
	EscrowMonthlyCents     *domain.Money               `json:"escrow_monthly_cents"`
	CurrentBalanceCents    *domain.Money               `json:"current_balance_cents"`
	BalanceAsOf            *string                     `json:"balance_as_of"`
	PayoffDate             *string                     `json:"payoff_date"`
	Notes                  string                      `json:"notes"`
	Statements             []mortgageStatementResponse `json:"statements"`
	CreatedAt              string                      `json:"created_at"`
	UpdatedAt              string                      `json:"updated_at"`
}

type mortgageList struct {
	Items []mortgageResponse `json:"items"`
}

func newMortgageResponse(m sqlc.Mortgage) mortgageResponse {
	return mortgageResponse{
		ID: m.ID, PropertyID: m.PropertyID, Lender: m.Lender,
		OriginalPrincipalCents: m.OriginalPrincipalCents,
		InterestRateBps:        m.InterestRateBps, TermMonths: m.TermMonths,
		OriginationDate: m.OriginationDate,
		MonthlyPiCents:  m.MonthlyPiCents, EscrowMonthlyCents: m.EscrowMonthlyCents,
		CurrentBalanceCents: m.CurrentBalanceCents, BalanceAsOf: m.BalanceAsOf,
		PayoffDate: m.PayoffDate, Notes: m.Notes,
		Statements: []mortgageStatementResponse{},
		CreatedAt:  m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}
}

func (s *server) handleListInsurance(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ctx := r.Context()

	policies, err := s.repo.Read().ListInsurancePoliciesByProperty(ctx, id)
	if err != nil {
		loggerFrom(ctx).Error("list insurance policies", "error", err, "property_id", id)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not read the policies.")
		return
	}

	out := insuranceList{Items: make([]insurancePolicyResponse, 0, len(policies))}
	for _, p := range policies {
		out.Items = append(out.Items, newInsurancePolicyResponse(p))
	}
	writeJSON(w, r, http.StatusOK, out)
}

// handleListMortgages carries each mortgage's statements inline.
//
// A mortgage is only interesting alongside the statements that moved its
// balance, and a property holds one or two of them. A second endpoint per
// mortgage would be a round trip for something that is always wanted together.
func (s *server) handleListMortgages(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ctx := r.Context()

	mortgages, err := s.repo.Read().ListMortgagesByProperty(ctx, id)
	if err != nil {
		loggerFrom(ctx).Error("list mortgages", "error", err, "property_id", id)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not read the mortgages.")
		return
	}

	out := mortgageList{Items: make([]mortgageResponse, 0, len(mortgages))}
	for _, m := range mortgages {
		response := newMortgageResponse(m)
		statements, err := s.repo.Read().ListMortgageStatements(ctx, m.ID)
		if err != nil {
			loggerFrom(ctx).Error("list mortgage statements", "error", err, "mortgage_id", m.ID)
			WriteProblem(w, r, http.StatusInternalServerError, "Could not read the statements.")
			return
		}
		for _, st := range statements {
			response.Statements = append(response.Statements, mortgageStatementResponse{
				ID: st.ID, StatementDate: st.StatementDate,
				PrincipalBalanceCents: st.PrincipalBalanceCents,
				PaymentCents:          st.PaymentCents,
				PrincipalPaidCents:    st.PrincipalPaidCents,
				InterestPaidCents:     st.InterestPaidCents,
				EscrowPaidCents:       st.EscrowPaidCents,
				DocumentID:            st.DocumentID,
				CreatedAt:             st.CreatedAt,
			})
		}
		out.Items = append(out.Items, response)
	}
	writeJSON(w, r, http.StatusOK, out)
}
