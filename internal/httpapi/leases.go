package httpapi

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// leaseStatuses and tenantRoles mirror the CHECK constraints in migration 0002.
var (
	leaseStatuses = []string{"pending", "active", "ended", "terminated"}
	tenantRoles   = []string{"primary", "cosigner", "occupant"}
)

// liveStatuses are the ones that hold a unit. Two of these overlapping on one
// unit would make occupancy ambiguous, which is the whole reason the overlap
// check exists.
var liveStatuses = map[string]bool{"pending": true, "active": true}

// leaseResponse is a lease abstract on the wire.
//
// EndDate is nullable and a null one is a month-to-month tenancy, not a missing
// value. The screen draws the term from these two dates, so the distinction is
// the difference between an open-ended rule and a broken one.
type leaseResponse struct {
	ID               int64         `json:"id"`
	UnitID           int64         `json:"unit_id"`
	UnitLabel        string        `json:"unit_label"`
	StartDate        string        `json:"start_date"`
	EndDate          *string       `json:"end_date"`
	RentCents        domain.Money  `json:"rent_cents"`
	DepositCents     *domain.Money `json:"deposit_cents"`
	DueDay           *int64        `json:"due_day"`
	LateFeeCents     *domain.Money `json:"late_fee_cents"`
	Status           string        `json:"status"`
	RenewalOfLeaseID *int64        `json:"renewal_of_lease_id"`
	DocumentID       *int64        `json:"document_id"`
	Notes            string        `json:"notes"`
	CreatedAt        string        `json:"created_at"`
	UpdatedAt        string        `json:"updated_at"`
	// Tenants is present on the single-lease response and absent from a list.
	Tenants []leaseTenantResponse `json:"tenants,omitempty"`
}

type leaseTenantResponse struct {
	tenantResponse
	Role string `json:"role"`
}

type leaseList struct {
	Items []leaseResponse `json:"items"`
}

func newLeaseResponse(l sqlc.Lease, unitLabel string) leaseResponse {
	return leaseResponse{
		ID: l.ID, UnitID: l.UnitID, UnitLabel: unitLabel,
		StartDate: l.StartDate, EndDate: l.EndDate,
		RentCents: l.RentCents, DepositCents: l.DepositCents,
		DueDay: l.DueDay, LateFeeCents: l.LateFeeCents,
		Status: l.Status, RenewalOfLeaseID: l.RenewalOfLeaseID,
		DocumentID: l.DocumentID, Notes: l.Notes,
		CreatedAt: l.CreatedAt, UpdatedAt: l.UpdatedAt,
	}
}

func (s *server) routeLeases(mux *http.ServeMux) {
	route(mux, "/api/v1/properties/{id}/leases", methods{
		http.MethodGet:  s.guarded(s.handleListLeases),
		http.MethodPost: s.guarded(s.handleCreateLease),
	})
	route(mux, "/api/v1/leases/{id}", methods{
		http.MethodGet:    s.guarded(s.handleGetLease),
		http.MethodPatch:  s.guarded(s.handleUpdateLease),
		http.MethodDelete: s.guarded(s.handleDeleteLease),
	})
	route(mux, "/api/v1/leases/{id}/tenants", methods{
		http.MethodPost:   s.guarded(s.handleAddLeaseTenant),
		http.MethodDelete: s.guarded(s.handleRemoveLeaseTenant),
	})
}

func (s *server) handleListLeases(w http.ResponseWriter, r *http.Request) {
	propertyID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ctx := r.Context()

	if _, err := s.repo.Read().GetProperty(ctx, propertyID); err != nil {
		s.propertyReadError(w, r, err)
		return
	}

	rows, err := s.repo.Read().ListLeasesByProperty(ctx, propertyID)
	if err != nil {
		loggerFrom(ctx).Error("list leases", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not read the leases.")
		return
	}

	out := leaseList{Items: make([]leaseResponse, 0, len(rows))}
	for _, row := range rows {
		out.Items = append(out.Items, newLeaseResponse(row.Lease, row.UnitLabel))
	}
	writeJSON(w, r, http.StatusOK, out)
}

// handleGetLease returns one lease with who is on it.
//
// The tenants come inline because a lease abstract without its parties is not
// an abstract of anything.
func (s *server) handleGetLease(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ctx := r.Context()

	row, err := s.repo.Read().GetLeaseWithUnit(ctx, id)
	if err != nil {
		s.leaseReadError(w, r, err)
		return
	}
	tenants, err := s.repo.Read().ListLeaseTenants(ctx, id)
	if err != nil {
		loggerFrom(ctx).Error("list lease tenants", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not read who is on the lease.")
		return
	}

	out := newLeaseResponse(row.Lease, row.UnitLabel)
	out.Tenants = make([]leaseTenantResponse, 0, len(tenants))
	for _, t := range tenants {
		out.Tenants = append(out.Tenants, leaseTenantResponse{
			tenantResponse: newTenantResponse(t.Tenant), Role: t.Role,
		})
	}
	writeJSON(w, r, http.StatusOK, out)
}

type createLeaseRequest struct {
	UnitID           int64         `json:"unit_id"`
	StartDate        string        `json:"start_date"`
	EndDate          *string       `json:"end_date"`
	RentCents        domain.Money  `json:"rent_cents"`
	DepositCents     *domain.Money `json:"deposit_cents"`
	DueDay           *int64        `json:"due_day"`
	LateFeeCents     *domain.Money `json:"late_fee_cents"`
	Status           string        `json:"status"`
	RenewalOfLeaseID *int64        `json:"renewal_of_lease_id"`
	DocumentID       *int64        `json:"document_id"`
	Notes            string        `json:"notes"`
}

func (s *server) handleCreateLease(w http.ResponseWriter, r *http.Request) {
	propertyID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var req createLeaseRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Status == "" {
		req.Status = "pending"
	}
	if detail := validateLease(req.StartDate, req.EndDate, req.Status, req.DueDay, req.RentCents); detail != "" {
		WriteProblem(w, r, http.StatusUnprocessableEntity, detail)
		return
	}

	ctx := r.Context()
	var created sqlc.Lease
	var unitLabel string

	err := s.repo.Tx(ctx, func(q *sqlc.Queries) error {
		// The unit has to belong to the property in the path. Without this a
		// lease could be filed against someone else's unit by id.
		unit, err := q.GetUnit(ctx, req.UnitID)
		if err != nil {
			if store.NotFound(err) {
				return validationError{"No unit has that id."}
			}
			return err
		}
		if unit.PropertyID != propertyID {
			return validationError{"That unit belongs to another property."}
		}
		unitLabel = unit.Label

		if err := checkOverlap(ctx, q, req.UnitID, 0, req.StartDate, req.EndDate, req.Status); err != nil {
			return err
		}

		now := timestamp()
		created, err = q.CreateLease(ctx, sqlc.CreateLeaseParams{
			UnitID:           req.UnitID,
			StartDate:        req.StartDate,
			EndDate:          req.EndDate,
			RentCents:        req.RentCents,
			DepositCents:     req.DepositCents,
			DueDay:           req.DueDay,
			LateFeeCents:     req.LateFeeCents,
			Status:           req.Status,
			RenewalOfLeaseID: req.RenewalOfLeaseID,
			DocumentID:       req.DocumentID,
			Notes:            req.Notes,
			CreatedAt:        now,
			UpdatedAt:        now,
		})
		return err
	})
	if err != nil {
		s.leaseWriteError(w, r, err)
		return
	}

	w.Header().Set("Location", "/api/v1/leases/"+strconv.FormatInt(created.ID, 10))
	writeJSON(w, r, http.StatusCreated, newLeaseResponse(created, unitLabel))
}

var leasePatchFields = []string{
	"unit_id", "start_date", "end_date", "rent_cents", "deposit_cents",
	"due_day", "late_fee_cents", "status", "renewal_of_lease_id",
	"document_id", "notes",
}

func (s *server) handleUpdateLease(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	p, ok := decodePatch(w, r, leasePatchFields...)
	if !ok {
		return
	}
	ctx := r.Context()

	var updated sqlc.Lease
	var unitLabel string

	err := s.repo.Tx(ctx, func(q *sqlc.Queries) error {
		current, err := q.GetLease(ctx, id)
		if err != nil {
			return err
		}
		propertyID, err := propertyOfUnit(ctx, q, current.UnitID)
		if err != nil {
			return err
		}

		for _, apply := range []func() error{
			func() error { return p.required("unit_id", &current.UnitID) },
			func() error { return p.required("start_date", &current.StartDate) },
			func() error { return p.required("rent_cents", &current.RentCents) },
			func() error { return p.required("status", &current.Status) },
			func() error { return p.required("notes", &current.Notes) },
			func() error { return p.nullable("end_date", &current.EndDate) },
			func() error { return p.nullable("deposit_cents", &current.DepositCents) },
			func() error { return p.nullable("due_day", &current.DueDay) },
			func() error { return p.nullable("late_fee_cents", &current.LateFeeCents) },
			func() error { return p.nullable("renewal_of_lease_id", &current.RenewalOfLeaseID) },
			func() error { return p.nullable("document_id", &current.DocumentID) },
		} {
			if err := apply(); err != nil {
				return validationError{err.Error()}
			}
		}

		if detail := validateLease(current.StartDate, current.EndDate, current.Status,
			current.DueDay, current.RentCents); detail != "" {
			return validationError{detail}
		}

		// Moving a lease to another unit is allowed, but not off the property.
		unit, err := q.GetUnit(ctx, current.UnitID)
		if err != nil {
			if store.NotFound(err) {
				return validationError{"No unit has that id."}
			}
			return err
		}
		if unit.PropertyID != propertyID {
			return validationError{"That unit belongs to another property."}
		}
		unitLabel = unit.Label

		if err := checkOverlap(ctx, q, current.UnitID, id,
			current.StartDate, current.EndDate, current.Status); err != nil {
			return err
		}

		updated, err = q.UpdateLease(ctx, sqlc.UpdateLeaseParams{
			UnitID:           current.UnitID,
			StartDate:        current.StartDate,
			EndDate:          current.EndDate,
			RentCents:        current.RentCents,
			DepositCents:     current.DepositCents,
			DueDay:           current.DueDay,
			LateFeeCents:     current.LateFeeCents,
			Status:           current.Status,
			RenewalOfLeaseID: current.RenewalOfLeaseID,
			DocumentID:       current.DocumentID,
			Notes:            current.Notes,
			UpdatedAt:        timestamp(),
			ID:               id,
		})
		return err
	})
	if err != nil {
		s.leaseWriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, newLeaseResponse(updated, unitLabel))
}

func (s *server) handleDeleteLease(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	rows, err := s.repo.Write().DeleteLease(r.Context(), id)
	if err != nil {
		loggerFrom(r.Context()).Error("delete lease", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not remove the lease.")
		return
	}
	if rows == 0 {
		WriteProblem(w, r, http.StatusNotFound, "No such lease.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type leaseTenantRequest struct {
	TenantID int64  `json:"tenant_id"`
	Role     string `json:"role"`
}

func (s *server) handleAddLeaseTenant(w http.ResponseWriter, r *http.Request) {
	leaseID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var req leaseTenantRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Role == "" {
		req.Role = "primary"
	}
	if !slices.Contains(tenantRoles, req.Role) {
		WriteProblem(w, r, http.StatusUnprocessableEntity,
			"Role has to be one of "+strings.Join(tenantRoles, ", ")+".")
		return
	}
	ctx := r.Context()

	if _, err := s.repo.Read().GetLease(ctx, leaseID); err != nil {
		s.leaseReadError(w, r, err)
		return
	}

	now := timestamp()
	_, err := s.repo.Write().AddLeaseTenant(ctx, sqlc.AddLeaseTenantParams{
		LeaseID: leaseID, TenantID: req.TenantID, Role: req.Role,
		CreatedAt: now, UpdatedAt: now,
	})
	switch {
	case store.Conflict(err):
		WriteProblem(w, r, http.StatusConflict, "That tenant is already on this lease.")
		return
	case store.ForeignKey(err):
		WriteProblem(w, r, http.StatusUnprocessableEntity, "No tenant has that id.")
		return
	case err != nil:
		loggerFrom(ctx).Error("add lease tenant", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not add the tenant to the lease.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleRemoveLeaseTenant(w http.ResponseWriter, r *http.Request) {
	leaseID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var req leaseTenantRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	rows, err := s.repo.Write().RemoveLeaseTenant(r.Context(), sqlc.RemoveLeaseTenantParams{
		LeaseID: leaseID, TenantID: req.TenantID,
	})
	if err != nil {
		loggerFrom(r.Context()).Error("remove lease tenant", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not remove the tenant from the lease.")
		return
	}
	if rows == 0 {
		WriteProblem(w, r, http.StatusNotFound, "That tenant is not on this lease.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// errOverlappingLease reports a second live lease on one unit.
var errOverlappingLease = errors.New("httpapi: a unit holds one live lease at a time")

// checkOverlap refuses a second live lease covering the same days on one unit.
//
// Occupancy is derived from the lease dates rather than stored (§3.2), so the
// write path is the only place that can keep the answer unambiguous. Two active
// leases covering today would make "is this unit let" a question with two
// correct answers.
//
// Ended and terminated leases are history and overlap nothing: a renewal that
// starts the day the old lease was terminated early is a normal thing to record.
func checkOverlap(ctx context.Context, q *sqlc.Queries, unitID, excludeID int64,
	startDate string, endDate *string, status string) error {

	if !liveStatuses[status] {
		return nil
	}
	count, err := q.CountOverlappingLeases(ctx, sqlc.CountOverlappingLeasesParams{
		UnitID:    unitID,
		ExcludeID: excludeID,
		StartDate: &startDate,
		EndDate:   endDate,
	})
	if err != nil {
		return err
	}
	if count > 0 {
		return errOverlappingLease
	}
	return nil
}

// propertyOfUnit reports which property a unit belongs to.
func propertyOfUnit(ctx context.Context, q *sqlc.Queries, unitID int64) (int64, error) {
	unit, err := q.GetUnit(ctx, unitID)
	if err != nil {
		return 0, err
	}
	return unit.PropertyID, nil
}

func validateLease(startDate string, endDate *string, status string,
	dueDay *int64, rentCents domain.Money) string {

	if !isCalendarDate(startDate) {
		return "The start date has to be written as YYYY-MM-DD."
	}
	if endDate != nil {
		if !isCalendarDate(*endDate) {
			return "The end date has to be written as YYYY-MM-DD."
		}
		if *endDate < startDate {
			return "A lease cannot end before it starts."
		}
	}
	if !slices.Contains(leaseStatuses, status) {
		return "Status has to be one of " + strings.Join(leaseStatuses, ", ") + "."
	}
	if dueDay != nil && (*dueDay < 1 || *dueDay > 31) {
		return "The rent due day has to be between 1 and 31."
	}
	if rentCents < 0 {
		return "Rent cannot be negative."
	}
	return ""
}

func (s *server) leaseReadError(w http.ResponseWriter, r *http.Request, err error) {
	if store.NotFound(err) {
		WriteProblem(w, r, http.StatusNotFound, "No such lease.")
		return
	}
	loggerFrom(r.Context()).Error("read lease", "error", err)
	WriteProblem(w, r, http.StatusInternalServerError, "Could not read the lease.")
}

func (s *server) leaseWriteError(w http.ResponseWriter, r *http.Request, err error) {
	var invalid validationError
	switch {
	case errors.Is(err, errOverlappingLease):
		WriteProblem(w, r, http.StatusConflict,
			"This unit already has a lease covering those dates. End that one first.")
	case errors.As(err, &invalid):
		WriteProblem(w, r, http.StatusUnprocessableEntity, invalid.detail)
	case store.NotFound(err):
		WriteProblem(w, r, http.StatusNotFound, "No such lease.")
	case store.ForeignKey(err):
		WriteProblem(w, r, http.StatusUnprocessableEntity,
			"One of the records this lease points at does not exist.")
	default:
		loggerFrom(r.Context()).Error("write lease", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not save the lease.")
	}
}
