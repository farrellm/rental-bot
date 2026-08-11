package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

type unitResponse struct {
	ID         int64    `json:"id"`
	PropertyID int64    `json:"property_id"`
	Label      string   `json:"label"`
	Beds       *int64   `json:"beds"`
	Baths      *float64 `json:"baths"`
	Sqft       *int64   `json:"sqft"`
	CreatedAt  string   `json:"created_at"`
	UpdatedAt  string   `json:"updated_at"`

	// ActiveLeaseID names the lease that makes this unit occupied today, or is
	// null when nothing does. It is derived on every read from the lease dates
	// and never stored (docs/DESIGN.md §3.2) — there is no is_occupied column
	// to drift out of sync with the leases that are the actual evidence.
	//
	// It carries the lease id rather than a boolean so the screen can link to
	// the reason for the answer instead of just asserting it.
	ActiveLeaseID      *int64  `json:"active_lease_id"`
	ActiveLeaseEndDate *string `json:"active_lease_end_date"`
}

// unitInput is a unit as a client writes it, on create or nested in a new
// property.
type unitInput struct {
	Label string   `json:"label"`
	Beds  *int64   `json:"beds"`
	Baths *float64 `json:"baths"`
	Sqft  *int64   `json:"sqft"`
}

func newUnitResponse(u sqlc.Unit) unitResponse {
	return unitResponse{
		ID: u.ID, PropertyID: u.PropertyID, Label: u.Label,
		Beds: u.Beds, Baths: u.Baths, Sqft: u.Sqft,
		CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt,
	}
}

func newUnitResponses(units []sqlc.Unit) []unitResponse {
	out := make([]unitResponse, 0, len(units))
	for _, u := range units {
		out = append(out, newUnitResponse(u))
	}
	return out
}

// newOccupiedUnitResponses renders units with the lease that is holding them.
func newOccupiedUnitResponses(rows []sqlc.ListUnitsWithOccupancyRow) []unitResponse {
	out := make([]unitResponse, 0, len(rows))
	for _, row := range rows {
		u := newUnitResponse(row.Unit)
		u.ActiveLeaseID = row.ActiveLeaseID
		u.ActiveLeaseEndDate = row.ActiveLeaseEndDate
		out = append(out, u)
	}
	return out
}

type unitList struct {
	Items []unitResponse `json:"items"`
}

func (s *server) handleListUnits(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ctx := r.Context()

	// The property is read first so a missing one is a 404 rather than an
	// empty list, which would look like a property with no units.
	if _, err := s.repo.Read().GetProperty(ctx, id); err != nil {
		s.propertyReadError(w, r, err)
		return
	}

	units, err := s.repo.Read().ListUnitsByProperty(ctx, id)
	if err != nil {
		loggerFrom(ctx).Error("list units", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not read the units.")
		return
	}
	writeJSON(w, r, http.StatusOK, unitList{Items: newUnitResponses(units)})
}

func (s *server) handleCreateUnit(w http.ResponseWriter, r *http.Request) {
	propertyID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var req unitInput
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Label = strings.TrimSpace(req.Label)

	if detail := validateUnits([]unitInput{req}); detail != "" {
		WriteProblem(w, r, http.StatusUnprocessableEntity, detail)
		return
	}

	ctx := r.Context()
	if _, err := s.repo.Read().GetProperty(ctx, propertyID); err != nil {
		s.propertyReadError(w, r, err)
		return
	}

	now := timestamp()
	unit, err := s.repo.Write().CreateUnit(ctx, sqlc.CreateUnitParams{
		PropertyID: propertyID,
		Label:      req.Label,
		Beds:       req.Beds,
		Baths:      req.Baths,
		Sqft:       req.Sqft,
		CreatedAt:  now,
		UpdatedAt:  now,
	})
	if err != nil {
		if store.Conflict(err) {
			WriteProblem(w, r, http.StatusConflict,
				"This property already has a unit labelled "+strconv.Quote(req.Label)+".")
			return
		}
		loggerFrom(ctx).Error("create unit", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not save the unit.")
		return
	}

	w.Header().Set("Location", "/api/v1/units/"+strconv.FormatInt(unit.ID, 10))
	writeJSON(w, r, http.StatusCreated, newUnitResponse(unit))
}

var unitPatchFields = []string{"label", "beds", "baths", "sqft"}

func (s *server) handleUpdateUnit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	p, ok := decodePatch(w, r, unitPatchFields...)
	if !ok {
		return
	}

	ctx := r.Context()
	var updated sqlc.Unit

	err := s.repo.Tx(ctx, func(q *sqlc.Queries) error {
		current, err := q.GetUnit(ctx, id)
		if err != nil {
			return err
		}

		if err := p.required("label", &current.Label); err != nil {
			return validationError{err.Error()}
		}
		for _, apply := range []func() error{
			func() error { return p.nullable("beds", &current.Beds) },
			func() error { return p.nullable("baths", &current.Baths) },
			func() error { return p.nullable("sqft", &current.Sqft) },
		} {
			if err := apply(); err != nil {
				return validationError{err.Error()}
			}
		}

		current.Label = strings.TrimSpace(current.Label)
		if detail := validateUnits([]unitInput{{
			Label: current.Label, Beds: current.Beds, Baths: current.Baths, Sqft: current.Sqft,
		}}); detail != "" {
			return validationError{detail}
		}

		updated, err = q.UpdateUnit(ctx, sqlc.UpdateUnitParams{
			Label:     current.Label,
			Beds:      current.Beds,
			Baths:     current.Baths,
			Sqft:      current.Sqft,
			UpdatedAt: timestamp(),
			ID:        id,
		})
		return err
	})
	if err != nil {
		s.unitWriteError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, newUnitResponse(updated))
}

// handleDeleteUnit removes a unit, unless it is the last one.
//
// Every lease hangs off a unit (docs/DESIGN.md §3.2). A property with no units
// would have nowhere to put a lease and would fork the query shape for every
// milestone after this one, so the invariant is enforced here rather than left
// to be discovered in M2.
func (s *server) handleDeleteUnit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ctx := r.Context()

	err := s.repo.Tx(ctx, func(q *sqlc.Queries) error {
		unit, err := q.GetUnit(ctx, id)
		if err != nil {
			return err
		}

		remaining, err := q.CountUnitsByProperty(ctx, unit.PropertyID)
		if err != nil {
			return err
		}
		if remaining <= 1 {
			return errLastUnit
		}

		_, err = q.DeleteUnit(ctx, id)
		return err
	})
	if err != nil {
		if errors.Is(err, errLastUnit) {
			WriteProblem(w, r, http.StatusConflict,
				"A property keeps at least one unit. Add another before removing this one.")
			return
		}
		s.unitWriteError(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

var errLastUnit = errors.New("httpapi: a property keeps at least one unit")

func (s *server) unitWriteError(w http.ResponseWriter, r *http.Request, err error) {
	var invalid validationError
	switch {
	case errors.As(err, &invalid):
		WriteProblem(w, r, http.StatusUnprocessableEntity, invalid.detail)
	case store.NotFound(err):
		WriteProblem(w, r, http.StatusNotFound, "No such unit.")
	case store.Conflict(err):
		WriteProblem(w, r, http.StatusConflict, "This property already has a unit with that label.")
	default:
		loggerFrom(r.Context()).Error("write unit", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not save the unit.")
	}
}

// validateUnits checks a set of units that will share one property, and
// returns a message naming what to fix.
func validateUnits(units []unitInput) string {
	seen := make(map[string]struct{}, len(units))
	for _, u := range units {
		label := strings.TrimSpace(u.Label)
		switch {
		case label == "":
			return "Every unit needs a label."
		case len(label) > 60:
			return "A unit label is longer than 60 characters."
		}
		if _, dup := seen[label]; dup {
			return "Two units cannot share the label " + strconv.Quote(label) + "."
		}
		seen[label] = struct{}{}

		if detail := validateRoomCounts(u.Beds, u.Baths, u.Sqft); detail != "" {
			return detail
		}
	}
	return ""
}
