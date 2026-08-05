package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// transactionCategories mirrors the CHECK constraint in migration 0002.
var transactionCategories = []string{
	"rent_income", "other_income", "mortgage_payment", "insurance",
	"property_tax", "hoa", "mgmt_fee", "repair", "capex",
	"utilities", "legal", "other",
}

// transactionResponse is one ledger entry on the wire.
//
// AmountCents is signed: income positive, expense negative. The sign is the
// only thing separating the two, here as in the database.
type transactionResponse struct {
	ID            int64        `json:"id"`
	PropertyID    int64        `json:"property_id"`
	OccurredOn    string       `json:"occurred_on"`
	AmountCents   domain.Money `json:"amount_cents"`
	Category      string       `json:"category"`
	Description   string       `json:"description"`
	Counterparty  string       `json:"counterparty"`
	PaymentMethod string       `json:"payment_method"`
	UnitID        *int64       `json:"unit_id"`
	LeaseID       *int64       `json:"lease_id"`
	RepairID      *int64       `json:"repair_id"`
	VendorID      *int64       `json:"vendor_id"`
	DocumentID    *int64       `json:"document_id"`
	Source        string       `json:"source"`
	NeedsReview   bool         `json:"needs_review"`
	CreatedAt     string       `json:"created_at"`
	UpdatedAt     string       `json:"updated_at"`
}

// ledgerTotals is the foot of the sheet.
//
// The server does this arithmetic, not the browser: the totals have to
// describe exactly the rows the filter selected, including the ones past the
// end of the page the client is holding.
type ledgerTotals struct {
	IncomeCents  domain.Money `json:"income_cents"`
	ExpenseCents domain.Money `json:"expense_cents"`
	NetCents     domain.Money `json:"net_cents"`
	EntryCount   int64        `json:"entry_count"`
}

type transactionList struct {
	Items      []transactionResponse `json:"items"`
	Totals     ledgerTotals          `json:"totals"`
	NextCursor string                `json:"next_cursor,omitempty"`
}

func newTransactionResponse(t sqlc.Transaction) transactionResponse {
	return transactionResponse{
		ID: t.ID, PropertyID: t.PropertyID, OccurredOn: t.OccurredOn,
		AmountCents: t.AmountCents, Category: t.Category,
		Description: t.Description, Counterparty: t.Counterparty,
		PaymentMethod: t.PaymentMethod,
		UnitID:        t.UnitID, LeaseID: t.LeaseID, RepairID: t.RepairID,
		VendorID: t.VendorID, DocumentID: t.DocumentID,
		Source: t.Source, NeedsReview: t.NeedsReview == 1,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

func (s *server) routeTransactions(mux *http.ServeMux) {
	route(mux, "/api/v1/properties/{id}/transactions", methods{
		http.MethodGet:  s.guarded(s.handleListTransactions),
		http.MethodPost: s.guarded(s.handleCreateTransaction),
	})
	route(mux, "/api/v1/transactions/{id}", methods{
		http.MethodGet:    s.guarded(s.handleGetTransaction),
		http.MethodPatch:  s.guarded(s.handleUpdateTransaction),
		http.MethodDelete: s.guarded(s.handleDeleteTransaction),
	})
}

// ledgerFilter is the from/to/category selection, shared by the page query and
// the totals query so the two can never describe different rows.
type ledgerFilter struct {
	From     *string
	To       *string
	Category *string
}

func readLedgerFilter(w http.ResponseWriter, r *http.Request) (ledgerFilter, bool) {
	var f ledgerFilter
	q := r.URL.Query()

	for _, param := range []struct {
		name string
		dst  **string
	}{{"from", &f.From}, {"to", &f.To}} {
		raw := strings.TrimSpace(q.Get(param.name))
		if raw == "" {
			continue
		}
		if !isCalendarDate(raw) {
			WriteProblem(w, r, http.StatusBadRequest,
				param.name+" has to be written as YYYY-MM-DD.")
			return f, false
		}
		value := raw
		*param.dst = &value
	}

	if raw := strings.TrimSpace(q.Get("category")); raw != "" {
		if !slicesContains(transactionCategories, raw) {
			WriteProblem(w, r, http.StatusBadRequest,
				"category has to be one of "+strings.Join(transactionCategories, ", ")+".")
			return f, false
		}
		value := raw
		f.Category = &value
	}
	return f, true
}

func (s *server) handleListTransactions(w http.ResponseWriter, r *http.Request) {
	propertyID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	size, ok := pageSize(w, r)
	if !ok {
		return
	}
	filter, ok := readLedgerFilter(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	if _, err := s.repo.Read().GetProperty(ctx, propertyID); err != nil {
		s.propertyReadError(w, r, err)
		return
	}

	rows, err := s.ledgerPage(ctx, propertyID, filter, r.URL.Query().Get("cursor"), size+1)
	if err != nil {
		if errors.Is(err, errBadCursor) {
			WriteProblem(w, r, http.StatusBadRequest, "The cursor is not one this endpoint issued.")
			return
		}
		loggerFrom(ctx).Error("list transactions", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not read the ledger.")
		return
	}

	// The same predicate as the page, so the foot describes the rows above it
	// -- including the ones past the end of this page.
	totals, err := s.repo.Read().SumTransactions(ctx, sqlc.SumTransactionsParams{
		PropertyID: propertyID,
		FromDate:   filter.From,
		ToDate:     filter.To,
		Category:   filter.Category,
	})
	if err != nil {
		loggerFrom(ctx).Error("sum transactions", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not total the ledger.")
		return
	}

	out := transactionList{
		Items: make([]transactionResponse, 0, size),
		Totals: ledgerTotals{
			IncomeCents:  domain.Money(totals.IncomeCents),
			ExpenseCents: domain.Money(totals.ExpenseCents),
			NetCents:     domain.Money(totals.NetCents),
			EntryCount:   totals.EntryCount,
		},
	}
	for i, t := range rows {
		if i == size {
			last := rows[i-1]
			out.NextCursor = encodeCursor(last.OccurredOn, last.ID)
			break
		}
		out.Items = append(out.Items, newTransactionResponse(t))
	}
	writeJSON(w, r, http.StatusOK, out)
}

func (s *server) ledgerPage(ctx context.Context, propertyID int64, filter ledgerFilter,
	cursor string, limit int) ([]sqlc.Transaction, error) {

	if cursor == "" {
		return s.repo.Read().ListTransactionsFirstPage(ctx, sqlc.ListTransactionsFirstPageParams{
			PropertyID: propertyID,
			FromDate:   filter.From,
			ToDate:     filter.To,
			Category:   filter.Category,
			PageSize:   int64(limit),
		})
	}
	occurredOn, id, err := decodeCursor(cursor)
	if err != nil {
		return nil, err
	}
	return s.repo.Read().ListTransactionsAfter(ctx, sqlc.ListTransactionsAfterParams{
		PropertyID: propertyID,
		FromDate:   filter.From,
		ToDate:     filter.To,
		Category:   filter.Category,
		AfterDate:  occurredOn,
		AfterID:    id,
		PageSize:   int64(limit),
	})
}

func (s *server) handleGetTransaction(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	entry, err := s.repo.Read().GetTransaction(r.Context(), id)
	if err != nil {
		s.transactionReadError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, newTransactionResponse(entry))
}

type createTransactionRequest struct {
	OccurredOn    string       `json:"occurred_on"`
	AmountCents   domain.Money `json:"amount_cents"`
	Category      string       `json:"category"`
	Description   string       `json:"description"`
	Counterparty  string       `json:"counterparty"`
	PaymentMethod string       `json:"payment_method"`
	UnitID        *int64       `json:"unit_id"`
	LeaseID       *int64       `json:"lease_id"`
	RepairID      *int64       `json:"repair_id"`
	VendorID      *int64       `json:"vendor_id"`
	DocumentID    *int64       `json:"document_id"`
	NeedsReview   bool         `json:"needs_review"`
}

func (s *server) handleCreateTransaction(w http.ResponseWriter, r *http.Request) {
	propertyID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var req createTransactionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Category == "" {
		req.Category = "other"
	}
	if detail := validateTransaction(req.OccurredOn, req.Category); detail != "" {
		WriteProblem(w, r, http.StatusUnprocessableEntity, detail)
		return
	}

	ctx := r.Context()
	if _, err := s.repo.Read().GetProperty(ctx, propertyID); err != nil {
		s.propertyReadError(w, r, err)
		return
	}

	now := timestamp()
	created, err := s.repo.Write().CreateTransaction(ctx, sqlc.CreateTransactionParams{
		PropertyID:    propertyID,
		OccurredOn:    req.OccurredOn,
		AmountCents:   req.AmountCents,
		Category:      req.Category,
		Description:   strings.TrimSpace(req.Description),
		Counterparty:  strings.TrimSpace(req.Counterparty),
		PaymentMethod: strings.TrimSpace(req.PaymentMethod),
		UnitID:        req.UnitID,
		LeaseID:       req.LeaseID,
		RepairID:      req.RepairID,
		VendorID:      req.VendorID,
		DocumentID:    req.DocumentID,
		// Everything this endpoint writes was typed by a person. M4's proposal
		// gate is the only thing that writes 'email'.
		Source:      "manual",
		NeedsReview: boolToInt(req.NeedsReview),
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	if err != nil {
		s.transactionWriteError(w, r, err)
		return
	}

	w.Header().Set("Location", "/api/v1/transactions/"+strconv.FormatInt(created.ID, 10))
	writeJSON(w, r, http.StatusCreated, newTransactionResponse(created))
}

// transactionPatchFields leaves out source, confidence, and property_id.
// Provenance is a fact about how a row arrived and is not the operator's to
// rewrite; moving an entry between properties is a delete and a re-entry, not
// an amendment.
var transactionPatchFields = []string{
	"occurred_on", "amount_cents", "category", "description", "counterparty",
	"payment_method", "unit_id", "lease_id", "repair_id", "vendor_id",
	"document_id", "needs_review",
}

func (s *server) handleUpdateTransaction(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	p, ok := decodePatch(w, r, transactionPatchFields...)
	if !ok {
		return
	}
	ctx := r.Context()

	var updated sqlc.Transaction
	err := s.repo.Tx(ctx, func(q *sqlc.Queries) error {
		current, err := q.GetTransaction(ctx, id)
		if err != nil {
			return err
		}

		needsReview := current.NeedsReview == 1
		for _, apply := range []func() error{
			func() error { return p.required("occurred_on", &current.OccurredOn) },
			func() error { return p.required("amount_cents", &current.AmountCents) },
			func() error { return p.required("category", &current.Category) },
			func() error { return p.required("description", &current.Description) },
			func() error { return p.required("counterparty", &current.Counterparty) },
			func() error { return p.required("payment_method", &current.PaymentMethod) },
			func() error { return p.required("needs_review", &needsReview) },
			func() error { return p.nullable("unit_id", &current.UnitID) },
			func() error { return p.nullable("lease_id", &current.LeaseID) },
			func() error { return p.nullable("repair_id", &current.RepairID) },
			func() error { return p.nullable("vendor_id", &current.VendorID) },
			func() error { return p.nullable("document_id", &current.DocumentID) },
		} {
			if err := apply(); err != nil {
				return validationError{err.Error()}
			}
		}

		if detail := validateTransaction(current.OccurredOn, current.Category); detail != "" {
			return validationError{detail}
		}

		updated, err = q.UpdateTransaction(ctx, sqlc.UpdateTransactionParams{
			OccurredOn:    current.OccurredOn,
			AmountCents:   current.AmountCents,
			Category:      current.Category,
			Description:   strings.TrimSpace(current.Description),
			Counterparty:  strings.TrimSpace(current.Counterparty),
			PaymentMethod: strings.TrimSpace(current.PaymentMethod),
			UnitID:        current.UnitID,
			LeaseID:       current.LeaseID,
			RepairID:      current.RepairID,
			VendorID:      current.VendorID,
			DocumentID:    current.DocumentID,
			NeedsReview:   boolToInt(needsReview),
			UpdatedAt:     timestamp(),
			ID:            id,
		})
		return err
	})
	if err != nil {
		s.transactionWriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, newTransactionResponse(updated))
}

func (s *server) handleDeleteTransaction(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	rows, err := s.repo.Write().DeleteTransaction(r.Context(), id)
	if err != nil {
		loggerFrom(r.Context()).Error("delete transaction", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not remove the entry.")
		return
	}
	if rows == 0 {
		WriteProblem(w, r, http.StatusNotFound, "No such entry.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validateTransaction checks what the database cannot state for itself.
//
// The amount is not range-checked and zero is allowed: a zero-value entry is a
// real thing to record (a waived fee, a credit that cancelled), and refusing
// it would be the application deciding what happened.
func validateTransaction(occurredOn, category string) string {
	if !isCalendarDate(occurredOn) {
		return "The date has to be written as YYYY-MM-DD."
	}
	if !slicesContains(transactionCategories, category) {
		return "Category has to be one of " + strings.Join(transactionCategories, ", ") + "."
	}
	return ""
}

func (s *server) transactionReadError(w http.ResponseWriter, r *http.Request, err error) {
	if store.NotFound(err) {
		WriteProblem(w, r, http.StatusNotFound, "No such entry.")
		return
	}
	loggerFrom(r.Context()).Error("read transaction", "error", err)
	WriteProblem(w, r, http.StatusInternalServerError, "Could not read the entry.")
}

func (s *server) transactionWriteError(w http.ResponseWriter, r *http.Request, err error) {
	var invalid validationError
	switch {
	case errors.As(err, &invalid):
		WriteProblem(w, r, http.StatusUnprocessableEntity, invalid.detail)
	case store.NotFound(err):
		WriteProblem(w, r, http.StatusNotFound, "No such entry.")
	case isForeignKeyError(err):
		WriteProblem(w, r, http.StatusUnprocessableEntity,
			"One of the records this entry points at does not exist.")
	default:
		loggerFrom(r.Context()).Error("write transaction", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not save the entry.")
	}
}

// isForeignKeyError reports a reference to a row that is not there.
//
// modernc.org/sqlite reports constraint failures as a message rather than a
// typed error, the same way store.Conflict has to match on text.
func isForeignKeyError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}

// boolToInt spells a Go bool the way a STRICT table stores one.
func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
