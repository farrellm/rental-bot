package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/farrellm/rental-bot/internal/config"
	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/ingest"
	"github.com/farrellm/rental-bot/internal/jobs"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// desk is the review API over a real database, plus the repository a test
// seeds proposals through.
//
// The pipeline is real: approving is the one endpoint in this API that writes
// to the ledger, and a fake apply path would leave the interesting half
// untested. No model is wired -- nothing here classifies, and the proposals
// are seeded the way the pipeline would have left them.
type desk struct {
	*api
	repo     *store.Repo
	property propertyDetail
}

func newDesk(t *testing.T) *desk {
	t.Helper()
	opts, request := authed(t, Options{})
	opts.Ingest = ingest.New(ingest.Options{
		Repo:   opts.Repo,
		Blobs:  opts.Blobs,
		Queue:  jobs.NewQueue(opts.Repo),
		Config: config.LLM{AutoApply: false, AutoApplyConfidence: 0.9},
		Logger: slog.New(slog.DiscardHandler),
	})

	handler := New(opts)
	a := &api{
		t: t, handler: handler, blobs: opts.Blobs, raw: request,
		request: func(method, target string, body any) *http.Request {
			if body == nil {
				return request(method, target, nil)
			}
			return request(method, target, jsonBody(t, body))
		},
	}

	d := &desk{api: a, repo: opts.Repo}
	d.property = a.newProperty(elmStreet())
	return d
}

// seed files a message and a proposal against it, the way a read would have.
func (d *desk) seed(gmailID, kind, payload string, propertyID *int64) sqlc.IngestProposal {
	d.t.Helper()
	ctx := d.t.Context()
	now := time.Now().UTC().Format(time.RFC3339)

	msg, err := d.repo.Write().CreateEmailMessage(ctx, sqlc.CreateEmailMessageParams{
		GmailMessageID: gmailID,
		FromAddr:       "operator@example.com",
		Subject:        "Fwd: your receipt",
		ReceivedAt:     now,
		Status:         "needs_review",
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	if err != nil {
		d.t.Fatalf("CreateEmailMessage: %v", err)
	}

	confidence := 0.94
	proposal, err := d.repo.Write().CreateProposal(ctx, sqlc.CreateProposalParams{
		EmailMessageID: msg.ID,
		Kind:           kind,
		Payload:        payload,
		LlmModel:       "claude-sonnet-5",
		PromptTokens:   1200,
		Confidence:     &confidence,
		PropertyHint:   "412 Elm St, Athens, OH 45701",
		PropertyID:     propertyID,
		Reasoning:      "A hardware store receipt.",
		Status:         "pending",
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	if err != nil {
		d.t.Fatalf("CreateProposal: %v", err)
	}
	return proposal
}

const homeDepotReceipt = `{"vendor_name":"Home Depot","date_iso":"2026-08-04","total_cents":48219,"category":"repair"}`

func reviewPath(id int64) string { return "/api/v1/review/" + itoa(id) }

// The register defaults to what is waiting, because that is what the screen is
// for, and it carries the message facts each line is read by.
func TestTheReviewQueueShowsWhatIsWaiting(t *testing.T) {
	d := newDesk(t)
	property := d.property.ID
	d.seed("gmail-1", "receipt", homeDepotReceipt, &property)

	var out proposalList
	d.decode(d.do(http.MethodGet, "/api/v1/review", nil), http.StatusOK, &out)

	if len(out.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(out.Items))
	}
	item := out.Items[0]
	if item.Status != "pending" || item.Kind != "receipt" {
		t.Fatalf("item = %+v, want a pending receipt", item)
	}
	if item.Subject != "Fwd: your receipt" || item.FromAddr != "operator@example.com" {
		t.Fatalf("item = %+v, want the message facts the register is read by", item)
	}
	if item.PropertyNickname == nil || *item.PropertyNickname != "Elm Street Duplex" {
		t.Fatalf("property = %v, want the matched property named", item.PropertyNickname)
	}
	if out.Counts["pending"] != 1 {
		t.Fatalf("counts = %v, want one pending", out.Counts)
	}

	// The payload rides through verbatim, because its shape is the kind's and
	// the screen renders it field by field.
	var receipt map[string]any
	if err := json.Unmarshal(item.Payload, &receipt); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if receipt["total_cents"] != float64(48219) {
		t.Fatalf("payload = %v, want the extraction verbatim", receipt)
	}
}

// A settled proposal is off the queue by default and findable on request.
func TestTheReviewQueueCanBeAskedForWhatWasDecided(t *testing.T) {
	d := newDesk(t)
	property := d.property.ID
	proposal := d.seed("gmail-decided", "receipt", homeDepotReceipt, &property)

	d.decode(d.do(http.MethodPost, reviewPath(proposal.ID)+"/reject", nil), http.StatusOK, nil)

	var pending proposalList
	d.decode(d.do(http.MethodGet, "/api/v1/review", nil), http.StatusOK, &pending)
	if len(pending.Items) != 0 {
		t.Fatalf("pending = %d, want the rejected one gone from the queue", len(pending.Items))
	}

	var all proposalList
	d.decode(d.do(http.MethodGet, "/api/v1/review?status=all", nil), http.StatusOK, &all)
	if len(all.Items) != 1 || all.Items[0].Status != "rejected" {
		t.Fatalf("all = %+v, want the rejected proposal still on file", all.Items)
	}

	if rec := d.do(http.MethodGet, "/api/v1/review?status=maybe", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("an unlisted status = %d, want 400", rec.Code)
	}
}

// The slip carries the enclosure to render beside the fields, and the
// portfolio to correct the match with.
func TestAProposalCarriesItsEnclosureAndThePortfolio(t *testing.T) {
	d := newDesk(t)
	property := d.property.ID
	proposal := d.seed("gmail-slip", "receipt", homeDepotReceipt, &property)

	// One attachment, filed the way a sync files one.
	doc := d.newDocument(t)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := d.repo.Write().CreateEmailAttachment(t.Context(), sqlc.CreateEmailAttachmentParams{
		EmailMessageID: proposal.EmailMessageID,
		PartID:         "1",
		Filename:       "receipt.pdf",
		Mime:           "application/pdf",
		SizeBytes:      12,
		DocumentID:     &doc,
		CreatedAt:      now,
		UpdatedAt:      now,
	}); err != nil {
		t.Fatalf("CreateEmailAttachment: %v", err)
	}

	var out proposalDetail
	d.decode(d.do(http.MethodGet, reviewPath(proposal.ID), nil), http.StatusOK, &out)

	if len(out.Enclosures) != 1 || out.Enclosures[0].DocumentID == nil {
		t.Fatalf("enclosures = %+v, want the document the screen renders", out.Enclosures)
	}
	if out.Property == nil || out.Property.Nickname != "Elm Street Duplex" {
		t.Fatalf("property = %+v, want the matched one", out.Property)
	}
	if len(out.Properties) != 1 {
		t.Fatalf("portfolio = %d, want the picker's options", len(out.Properties))
	}
	if out.PropertyHint == "" || out.Reasoning == "" {
		t.Fatal("the slip cannot say why it matched what it matched")
	}
}

// newDocument files bytes and returns the document id.
func (d *desk) newDocument(t *testing.T) int64 {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	doc, err := d.repo.Write().CreateDocument(t.Context(), sqlc.CreateDocumentParams{
		Kind:             "receipt",
		OriginalFilename: "receipt.pdf",
		Mime:             "application/pdf",
		SizeBytes:        12,
		Sha256:           "a1b2c3d4e5f6",
		StoragePath:      "a1/b2/a1b2c3d4e5f6",
		CreatedAt:        now,
		UpdatedAt:        now,
	})
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	return doc.ID
}

// Correcting the reading is the whole point of the screen: the operator
// changes what the model got wrong, and only then agrees to it.
func TestAProposalCanBeCorrectedBeforeItIsFiled(t *testing.T) {
	d := newDesk(t)
	proposal := d.seed("gmail-correct", "receipt", homeDepotReceipt, nil)
	property := d.property.ID

	var corrected proposalResponse
	d.decode(d.do(http.MethodPatch, reviewPath(proposal.ID), map[string]any{
		"payload": map[string]any{
			"vendor_name": "Ace Hardware",
			"date_iso":    "2026-08-04",
			"total_cents": 51000,
			"category":    "repair",
		},
		"property_id": property,
	}), http.StatusOK, &corrected)

	if corrected.PropertyID == nil || *corrected.PropertyID != property {
		t.Fatalf("property = %v, want the one the operator chose", corrected.PropertyID)
	}

	// The kind was not sent, so it was not touched. Absent, null and a value
	// are three different instructions.
	if corrected.Kind != "receipt" {
		t.Fatalf("kind = %q, want it untouched by a patch that did not name it", corrected.Kind)
	}

	var receipt map[string]any
	if err := json.Unmarshal(corrected.Payload, &receipt); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if receipt["vendor_name"] != "Ace Hardware" || receipt["total_cents"] != float64(51000) {
		t.Fatalf("payload = %v, want the correction", receipt)
	}

	// Clearing the match is a thing an operator can mean.
	d.decode(d.do(http.MethodPatch, reviewPath(proposal.ID), map[string]any{
		"property_id": nil,
	}), http.StatusOK, &corrected)
	if corrected.PropertyID != nil {
		t.Fatalf("property = %v, want null to have cleared it", corrected.PropertyID)
	}
}

func TestAProposalRefusesACorrectionItCannotHold(t *testing.T) {
	d := newDesk(t)
	proposal := d.seed("gmail-bad", "receipt", homeDepotReceipt, nil)

	tests := []struct {
		name string
		body map[string]any
		want int
	}{
		{"a kind the column does not accept", map[string]any{"kind": "invoice"}, http.StatusUnprocessableEntity},
		{"a payload that is not an object", map[string]any{"payload": nil}, http.StatusUnprocessableEntity},
		{"a property that does not exist", map[string]any{"property_id": 9999}, http.StatusUnprocessableEntity},
		{"a field this endpoint does not accept", map[string]any{"confidence": 1.0}, http.StatusBadRequest},
		{"a body that changes nothing", map[string]any{}, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if rec := d.do(http.MethodPatch, reviewPath(proposal.ID), tt.body); rec.Code != tt.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

// Approving is what the whole pipeline exists to make one tap.
func TestApprovingAProposalFilesIt(t *testing.T) {
	d := newDesk(t)
	property := d.property.ID
	proposal := d.seed("gmail-approve", "receipt", homeDepotReceipt, &property)

	var filed proposalResponse
	d.decode(d.do(http.MethodPost, reviewPath(proposal.ID)+"/approve", nil), http.StatusOK, &filed)

	if filed.Status != "approved" {
		t.Fatalf("status = %q, want approved", filed.Status)
	}
	if filed.AppliedEntityType == nil || *filed.AppliedEntityType != "transaction" {
		t.Fatalf("applied to %v, want a transaction", filed.AppliedEntityType)
	}
	if filed.ReviewedBy == nil {
		t.Fatal("nobody is recorded as having approved it")
	}

	// The entry is on the property's ledger, as an expense.
	var ledger transactionList
	d.decode(d.do(http.MethodGet, "/api/v1/properties/"+itoa(property)+"/transactions", nil), http.StatusOK, &ledger)
	if len(ledger.Items) != 1 {
		t.Fatalf("ledger = %d entries, want 1", len(ledger.Items))
	}
	if got := ledger.Items[0].AmountCents; got != domain.Money(-48219) {
		t.Fatalf("amount = %s, want an expense of $482.19", got)
	}
	if !ledger.Items[0].NeedsReview {
		t.Fatal("the entry is not flagged; an entry that came off a model should say so")
	}
}

// Approving twice would file the receipt twice. The second one is a conflict:
// not a malformed request and not a fault, but a thing that cannot be done as
// matters stand.
func TestAProposalIsSettledOnlyOnce(t *testing.T) {
	d := newDesk(t)
	property := d.property.ID
	proposal := d.seed("gmail-twice", "receipt", homeDepotReceipt, &property)

	d.decode(d.do(http.MethodPost, reviewPath(proposal.ID)+"/approve", nil), http.StatusOK, nil)

	for _, verb := range []string{"approve", "reject"} {
		rec := d.do(http.MethodPost, reviewPath(proposal.ID)+"/"+verb, nil)
		if rec.Code != http.StatusConflict {
			t.Fatalf("second %s = %d, want 409 (body %s)", verb, rec.Code, rec.Body)
		}
	}
}

// An apply with nowhere to land is refused with the reason, and the proposal
// stays where the operator can fix it.
func TestApprovingWithNoPropertyIsRefusedWithAReason(t *testing.T) {
	d := newDesk(t)
	proposal := d.seed("gmail-nowhere", "receipt", homeDepotReceipt, nil)

	rec := d.do(http.MethodPost, reviewPath(proposal.ID)+"/approve", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body)
	}

	var problem Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Detail == "" {
		t.Fatal("the refusal says nothing, so the operator does not know what to fix")
	}

	var still proposalResponse
	d.decode(d.do(http.MethodGet, reviewPath(proposal.ID), nil), http.StatusOK, &still)
	if still.Status != "pending" {
		t.Fatalf("status = %q, want it still pending", still.Status)
	}
	if still.Error == "" {
		t.Fatal("the reason is not on the row, so the screen cannot show it")
	}
}

// Rejecting keeps the row and moves the message with it.
func TestRejectingAProposal(t *testing.T) {
	d := newDesk(t)
	property := d.property.ID
	proposal := d.seed("gmail-reject", "receipt", homeDepotReceipt, &property)

	var out proposalResponse
	d.decode(d.do(http.MethodPost, reviewPath(proposal.ID)+"/reject", nil), http.StatusOK, &out)
	if out.Status != "rejected" {
		t.Fatalf("status = %q, want rejected", out.Status)
	}
}

// Without a model there is no apply path to call, and saying so beats a 500.
func TestSettlingNeedsThePipeline(t *testing.T) {
	a := newAPI(t)
	if rec := a.do(http.MethodPost, "/api/v1/review/1/approve", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestReviewRoutesNeedASession(t *testing.T) {
	handler := New(Options{DB: healthyDB(), Guard: nil})

	tests := []struct{ method, target string }{
		{http.MethodGet, "/api/v1/review"},
		{http.MethodGet, "/api/v1/review/1"},
		{http.MethodPatch, "/api/v1/review/1"},
		{http.MethodPost, "/api/v1/review/1/approve"},
		{http.MethodPost, "/api/v1/review/1/reject"},
		{http.MethodGet, "/api/v1/properties/1/insurance"},
		{http.MethodGet, "/api/v1/properties/1/mortgage"},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.target, nil))
		// A nil guard fails closed rather than serving the queue to anyone.
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503", tt.method, tt.target, rec.Code)
		}
	}
}

func TestReviewMutationsNeedACSRFToken(t *testing.T) {
	d := newDesk(t)
	property := d.property.ID
	proposal := d.seed("gmail-csrf", "receipt", homeDepotReceipt, &property)

	for _, target := range []string{
		reviewPath(proposal.ID) + "/approve",
		reviewPath(proposal.ID) + "/reject",
	} {
		req := d.request(http.MethodPost, target, nil)
		req.Header.Del("X-CSRF-Token")
		rec := httptest.NewRecorder()
		d.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s without a token = %d, want 403", target, rec.Code)
		}
	}
}

// An applied insurance or mortgage proposal writes a row, and these are what
// show it.
func TestInsuranceAndMortgageAreReadable(t *testing.T) {
	d := newDesk(t)
	property := d.property.ID
	now := time.Now().UTC().Format(time.RFC3339)
	ctx := t.Context()

	premium := domain.Money(184000)
	if _, err := d.repo.Write().CreateInsurancePolicy(ctx, sqlc.CreateInsurancePolicyParams{
		PropertyID:         property,
		Carrier:            "State Farm",
		PolicyNumberEnc:    "sealed",
		Type:               "hazard",
		AnnualPremiumCents: &premium,
		CreatedAt:          now,
		UpdatedAt:          now,
	}); err != nil {
		t.Fatalf("CreateInsurancePolicy: %v", err)
	}

	mortgage, err := d.repo.Write().CreateMortgage(ctx, sqlc.CreateMortgageParams{
		PropertyID:    property,
		Lender:        "Hocking Valley Bank",
		LoanNumberEnc: "sealed",
		CreatedAt:     now,
		UpdatedAt:     now,
	})
	if err != nil {
		t.Fatalf("CreateMortgage: %v", err)
	}
	balance := domain.Money(19_450_000)
	if _, err := d.repo.Write().CreateMortgageStatement(ctx, sqlc.CreateMortgageStatementParams{
		MortgageID:            mortgage.ID,
		StatementDate:         "2026-07-01",
		PrincipalBalanceCents: &balance,
		CreatedAt:             now,
		UpdatedAt:             now,
	}); err != nil {
		t.Fatalf("CreateMortgageStatement: %v", err)
	}

	var policies insuranceList
	d.decode(d.do(http.MethodGet, "/api/v1/properties/"+itoa(property)+"/insurance", nil), http.StatusOK, &policies)
	if len(policies.Items) != 1 || policies.Items[0].Carrier != "State Farm" {
		t.Fatalf("policies = %+v, want the one on file", policies.Items)
	}
	// The policy number is encrypted at rest and has no business on the wire.
	if body := d.do(http.MethodGet, "/api/v1/properties/"+itoa(property)+"/insurance", nil).Body.String(); strings.Contains(body, "sealed") {
		t.Fatal("the response carries the stored policy number")
	}

	var mortgages mortgageList
	d.decode(d.do(http.MethodGet, "/api/v1/properties/"+itoa(property)+"/mortgage", nil), http.StatusOK, &mortgages)
	if len(mortgages.Items) != 1 || len(mortgages.Items[0].Statements) != 1 {
		t.Fatalf("mortgages = %+v, want one with its statement inline", mortgages.Items)
	}
}
