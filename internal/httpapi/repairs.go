package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// repairStatuses mirrors the CHECK constraint in migration 0002.
var repairStatuses = []string{"open", "scheduled", "in_progress", "done", "wontfix"}

// closedStatuses are the ones that mean the job is over. A repair reaching one
// of these gets a closing date, and a repair leaving one loses it, so
// closed_on and status can never tell different stories.
var closedStatuses = map[string]bool{"done": true, "wontfix": true}

type repairResponse struct {
	ID            int64         `json:"id"`
	PropertyID    int64         `json:"property_id"`
	UnitID        *int64        `json:"unit_id"`
	OpenedOn      string        `json:"opened_on"`
	ClosedOn      *string       `json:"closed_on"`
	Status        string        `json:"status"`
	Category      string        `json:"category"`
	VendorID      *int64        `json:"vendor_id"`
	Description   string        `json:"description"`
	EstimateCents *domain.Money `json:"estimate_cents"`
	ActualCents   *domain.Money `json:"actual_cents"`
	IsCapex       bool          `json:"is_capex"`
	WarrantyUntil *string       `json:"warranty_until"`
	Notes         string        `json:"notes"`
	CreatedAt     string        `json:"created_at"`
	UpdatedAt     string        `json:"updated_at"`
	// Events is present on the single-repair response and absent from a list.
	Events []repairEventResponse `json:"events,omitempty"`
}

type repairEventResponse struct {
	ID         int64  `json:"id"`
	RepairID   int64  `json:"repair_id"`
	At         string `json:"at"`
	Note       string `json:"note"`
	DocumentID *int64 `json:"document_id"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

type repairList struct {
	Items []repairResponse `json:"items"`
}

func newRepairResponse(r sqlc.Repair) repairResponse {
	return repairResponse{
		ID: r.ID, PropertyID: r.PropertyID, UnitID: r.UnitID,
		OpenedOn: r.OpenedOn, ClosedOn: r.ClosedOn, Status: r.Status,
		Category: r.Category, VendorID: r.VendorID, Description: r.Description,
		EstimateCents: r.EstimateCents, ActualCents: r.ActualCents,
		IsCapex: r.IsCapex == 1, WarrantyUntil: r.WarrantyUntil, Notes: r.Notes,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func newRepairEventResponse(e sqlc.RepairEvent) repairEventResponse {
	return repairEventResponse{
		ID: e.ID, RepairID: e.RepairID, At: e.At, Note: e.Note,
		DocumentID: e.DocumentID, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt,
	}
}

func (s *server) routeRepairs(mux *http.ServeMux) {
	route(mux, "/api/v1/properties/{id}/repairs", methods{
		http.MethodGet:  s.guarded(s.handleListRepairs),
		http.MethodPost: s.guarded(s.handleCreateRepair),
	})
	route(mux, "/api/v1/repairs/{id}", methods{
		http.MethodGet:    s.guarded(s.handleGetRepair),
		http.MethodPatch:  s.guarded(s.handleUpdateRepair),
		http.MethodDelete: s.guarded(s.handleDeleteRepair),
	})
	route(mux, "/api/v1/repairs/{id}/events", methods{
		http.MethodPost: s.guarded(s.handleCreateRepairEvent),
	})
	route(mux, "/api/v1/repair-events/{id}", methods{
		http.MethodDelete: s.guarded(s.handleDeleteRepairEvent),
	})
}

func (s *server) handleListRepairs(w http.ResponseWriter, r *http.Request) {
	propertyID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	size, ok := pageSize(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	var status *string
	if raw := strings.TrimSpace(r.URL.Query().Get("status")); raw != "" {
		if !slicesContains(repairStatuses, raw) {
			WriteProblem(w, r, http.StatusBadRequest,
				"status has to be one of "+strings.Join(repairStatuses, ", ")+".")
			return
		}
		status = &raw
	}

	if _, err := s.repo.Read().GetProperty(ctx, propertyID); err != nil {
		s.propertyReadError(w, r, err)
		return
	}

	repairs, err := s.repo.Read().ListRepairsByProperty(ctx, sqlc.ListRepairsByPropertyParams{
		PropertyID: propertyID, Status: status, PageSize: int64(size),
	})
	if err != nil {
		loggerFrom(ctx).Error("list repairs", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not read the repairs.")
		return
	}

	out := repairList{Items: make([]repairResponse, 0, len(repairs))}
	for _, repair := range repairs {
		out.Items = append(out.Items, newRepairResponse(repair))
	}
	writeJSON(w, r, http.StatusOK, out)
}

// handleGetRepair returns one repair with its timeline.
//
// The events come inline because a docket without its history is half a
// record: what you want to know about a repair is what has happened to it.
func (s *server) handleGetRepair(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ctx := r.Context()

	repair, err := s.repo.Read().GetRepair(ctx, id)
	if err != nil {
		s.repairReadError(w, r, err)
		return
	}
	events, err := s.repo.Read().ListRepairEvents(ctx, id)
	if err != nil {
		loggerFrom(ctx).Error("list repair events", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not read the repair's history.")
		return
	}

	out := newRepairResponse(repair)
	out.Events = make([]repairEventResponse, 0, len(events))
	for _, e := range events {
		out.Events = append(out.Events, newRepairEventResponse(e))
	}
	writeJSON(w, r, http.StatusOK, out)
}

type createRepairRequest struct {
	UnitID        *int64        `json:"unit_id"`
	OpenedOn      string        `json:"opened_on"`
	ClosedOn      *string       `json:"closed_on"`
	Status        string        `json:"status"`
	Category      string        `json:"category"`
	VendorID      *int64        `json:"vendor_id"`
	Description   string        `json:"description"`
	EstimateCents *domain.Money `json:"estimate_cents"`
	ActualCents   *domain.Money `json:"actual_cents"`
	IsCapex       bool          `json:"is_capex"`
	WarrantyUntil *string       `json:"warranty_until"`
	Notes         string        `json:"notes"`
}

func (s *server) handleCreateRepair(w http.ResponseWriter, r *http.Request) {
	propertyID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var req createRepairRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Status == "" {
		req.Status = "open"
	}
	if req.OpenedOn == "" {
		req.OpenedOn = time.Now().UTC().Format(time.DateOnly)
	}
	req.Description = strings.TrimSpace(req.Description)

	closedOn := reconcileClosing(req.Status, req.ClosedOn)
	if detail := validateRepair(req.OpenedOn, closedOn, req.Status,
		req.WarrantyUntil, req.Description); detail != "" {
		WriteProblem(w, r, http.StatusUnprocessableEntity, detail)
		return
	}

	ctx := r.Context()
	if _, err := s.repo.Read().GetProperty(ctx, propertyID); err != nil {
		s.propertyReadError(w, r, err)
		return
	}

	now := timestamp()
	created, err := s.repo.Write().CreateRepair(ctx, sqlc.CreateRepairParams{
		PropertyID:    propertyID,
		UnitID:        req.UnitID,
		OpenedOn:      req.OpenedOn,
		ClosedOn:      closedOn,
		Status:        req.Status,
		Category:      strings.TrimSpace(req.Category),
		VendorID:      req.VendorID,
		Description:   req.Description,
		EstimateCents: req.EstimateCents,
		ActualCents:   req.ActualCents,
		IsCapex:       boolToInt(req.IsCapex),
		WarrantyUntil: req.WarrantyUntil,
		Notes:         req.Notes,
		CreatedAt:     now,
		UpdatedAt:     now,
	})
	if err != nil {
		s.repairWriteError(w, r, err)
		return
	}

	w.Header().Set("Location", "/api/v1/repairs/"+strconv.FormatInt(created.ID, 10))
	writeJSON(w, r, http.StatusCreated, newRepairResponse(created))
}

var repairPatchFields = []string{
	"unit_id", "opened_on", "closed_on", "status", "category", "vendor_id",
	"description", "estimate_cents", "actual_cents", "is_capex",
	"warranty_until", "notes",
}

func (s *server) handleUpdateRepair(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	p, ok := decodePatch(w, r, repairPatchFields...)
	if !ok {
		return
	}
	ctx := r.Context()

	var updated sqlc.Repair
	err := s.repo.Tx(ctx, func(q *sqlc.Queries) error {
		current, err := q.GetRepair(ctx, id)
		if err != nil {
			return err
		}

		wasClosed := closedStatuses[current.Status]
		isCapex := current.IsCapex == 1
		for _, apply := range []func() error{
			func() error { return p.required("opened_on", &current.OpenedOn) },
			func() error { return p.required("status", &current.Status) },
			func() error { return p.required("category", &current.Category) },
			func() error { return p.required("description", &current.Description) },
			func() error { return p.required("notes", &current.Notes) },
			func() error { return p.required("is_capex", &isCapex) },
			func() error { return p.nullable("unit_id", &current.UnitID) },
			func() error { return p.nullable("closed_on", &current.ClosedOn) },
			func() error { return p.nullable("vendor_id", &current.VendorID) },
			func() error { return p.nullable("estimate_cents", &current.EstimateCents) },
			func() error { return p.nullable("actual_cents", &current.ActualCents) },
			func() error { return p.nullable("warranty_until", &current.WarrantyUntil) },
		} {
			if err := apply(); err != nil {
				return validationError{err.Error()}
			}
		}

		// Closing and reopening carry their date with them, unless the caller
		// named one explicitly in this same patch.
		if _, named := p["closed_on"]; !named {
			current.ClosedOn = reconcileClosingTransition(current.Status, wasClosed, current.ClosedOn)
		}

		current.Description = strings.TrimSpace(current.Description)
		if detail := validateRepair(current.OpenedOn, current.ClosedOn, current.Status,
			current.WarrantyUntil, current.Description); detail != "" {
			return validationError{detail}
		}

		updated, err = q.UpdateRepair(ctx, sqlc.UpdateRepairParams{
			UnitID:        current.UnitID,
			OpenedOn:      current.OpenedOn,
			ClosedOn:      current.ClosedOn,
			Status:        current.Status,
			Category:      strings.TrimSpace(current.Category),
			VendorID:      current.VendorID,
			Description:   current.Description,
			EstimateCents: current.EstimateCents,
			ActualCents:   current.ActualCents,
			IsCapex:       boolToInt(isCapex),
			WarrantyUntil: current.WarrantyUntil,
			Notes:         current.Notes,
			UpdatedAt:     timestamp(),
			ID:            id,
		})
		return err
	})
	if err != nil {
		s.repairWriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, newRepairResponse(updated))
}

func (s *server) handleDeleteRepair(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	rows, err := s.repo.Write().DeleteRepair(r.Context(), id)
	if err != nil {
		loggerFrom(r.Context()).Error("delete repair", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not remove the repair.")
		return
	}
	if rows == 0 {
		WriteProblem(w, r, http.StatusNotFound, "No such repair.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type createRepairEventRequest struct {
	At         string `json:"at"`
	Note       string `json:"note"`
	DocumentID *int64 `json:"document_id"`
}

// handleCreateRepairEvent adds a line to the timeline.
//
// Events are append-only in practice: quoted, scheduled, completed, paid. The
// only way to take one back is to delete it, which is deliberate — amending
// history in place would make the timeline a summary rather than a record.
func (s *server) handleCreateRepairEvent(w http.ResponseWriter, r *http.Request) {
	repairID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var req createRepairEventRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.At == "" {
		req.At = timestamp()
	}
	req.Note = strings.TrimSpace(req.Note)

	if _, err := time.Parse(time.RFC3339, req.At); err != nil {
		WriteProblem(w, r, http.StatusUnprocessableEntity,
			"The time has to be an RFC3339 timestamp, like 2026-07-03T14:05:00Z.")
		return
	}
	if req.Note == "" {
		WriteProblem(w, r, http.StatusUnprocessableEntity, "An event needs a note saying what happened.")
		return
	}
	if len(req.Note) > 2000 {
		WriteProblem(w, r, http.StatusUnprocessableEntity, "The note is longer than 2000 characters.")
		return
	}

	ctx := r.Context()
	if _, err := s.repo.Read().GetRepair(ctx, repairID); err != nil {
		s.repairReadError(w, r, err)
		return
	}

	now := timestamp()
	event, err := s.repo.Write().CreateRepairEvent(ctx, sqlc.CreateRepairEventParams{
		RepairID: repairID, At: req.At, Note: req.Note, DocumentID: req.DocumentID,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		if isForeignKeyError(err) {
			WriteProblem(w, r, http.StatusUnprocessableEntity, "No document has that id.")
			return
		}
		loggerFrom(ctx).Error("create repair event", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not record the event.")
		return
	}

	writeJSON(w, r, http.StatusCreated, newRepairEventResponse(event))
}

func (s *server) handleDeleteRepairEvent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	rows, err := s.repo.Write().DeleteRepairEvent(r.Context(), id)
	if err != nil {
		loggerFrom(r.Context()).Error("delete repair event", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not remove the event.")
		return
	}
	if rows == 0 {
		WriteProblem(w, r, http.StatusNotFound, "No such event.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// reconcileClosing supplies a closing date for a repair created as finished.
func reconcileClosing(status string, closedOn *string) *string {
	if closedStatuses[status] && closedOn == nil {
		today := time.Now().UTC().Format(time.DateOnly)
		return &today
	}
	if !closedStatuses[status] {
		return nil
	}
	return closedOn
}

// reconcileClosingTransition keeps closed_on agreeing with status across an
// amendment: a repair that just closed gets today's date, and one that just
// reopened loses the date it had. A job cannot be both finished and open.
func reconcileClosingTransition(status string, wasClosed bool, closedOn *string) *string {
	isClosed := closedStatuses[status]
	switch {
	case isClosed && !wasClosed:
		today := time.Now().UTC().Format(time.DateOnly)
		return &today
	case !isClosed:
		return nil
	default:
		return closedOn
	}
}

func validateRepair(openedOn string, closedOn *string, status string,
	warrantyUntil *string, description string) string {

	if !isCalendarDate(openedOn) {
		return "The opening date has to be written as YYYY-MM-DD."
	}
	if !slicesContains(repairStatuses, status) {
		return "Status has to be one of " + strings.Join(repairStatuses, ", ") + "."
	}
	if closedOn != nil {
		if !isCalendarDate(*closedOn) {
			return "The closing date has to be written as YYYY-MM-DD."
		}
		if *closedOn < openedOn {
			return "A repair cannot close before it opened."
		}
	}
	if warrantyUntil != nil && !isCalendarDate(*warrantyUntil) {
		return "The warranty date has to be written as YYYY-MM-DD."
	}
	if description == "" {
		return "A repair needs a description of the work."
	}
	if len(description) > 2000 {
		return "The description is longer than 2000 characters."
	}
	return ""
}

func (s *server) repairReadError(w http.ResponseWriter, r *http.Request, err error) {
	if store.NotFound(err) {
		WriteProblem(w, r, http.StatusNotFound, "No such repair.")
		return
	}
	loggerFrom(r.Context()).Error("read repair", "error", err)
	WriteProblem(w, r, http.StatusInternalServerError, "Could not read the repair.")
}

func (s *server) repairWriteError(w http.ResponseWriter, r *http.Request, err error) {
	var invalid validationError
	switch {
	case errors.As(err, &invalid):
		WriteProblem(w, r, http.StatusUnprocessableEntity, invalid.detail)
	case store.NotFound(err):
		WriteProblem(w, r, http.StatusNotFound, "No such repair.")
	case isForeignKeyError(err):
		WriteProblem(w, r, http.StatusUnprocessableEntity,
			"One of the records this repair points at does not exist.")
	default:
		loggerFrom(r.Context()).Error("write repair", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not save the repair.")
	}
}
