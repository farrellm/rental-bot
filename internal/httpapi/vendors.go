package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// Vendors and tenants are portfolio-wide, not per-property: one plumber works
// on several houses, and re-entering them per property would make "what did I
// pay Ace Plumbing last year" unanswerable.

type vendorResponse struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Trade     string `json:"trade"`
	Phone     string `json:"phone"`
	Email     string `json:"email"`
	Notes     string `json:"notes"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type vendorList struct {
	Items []vendorResponse `json:"items"`
}

func newVendorResponse(v sqlc.Vendor) vendorResponse {
	return vendorResponse{
		ID: v.ID, Name: v.Name, Trade: v.Trade, Phone: v.Phone,
		Email: v.Email, Notes: v.Notes,
		CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt,
	}
}

// recVendor names a vendor for the shared error answers.
var recVendor = record{noun: "vendor"}

func (s *server) routeVendors(mux *http.ServeMux) {
	route(mux, "/api/v1/vendors", methods{
		http.MethodGet:  s.guarded(s.handleListVendors),
		http.MethodPost: s.guarded(s.handleCreateVendor),
	})
	route(mux, "/api/v1/vendors/{id}", methods{
		http.MethodGet:    s.guarded(s.handleGetVendor),
		http.MethodPatch:  s.guarded(s.handleUpdateVendor),
		http.MethodDelete: s.guarded(s.handleDeleteVendor),
	})
}

func (s *server) handleListVendors(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	vendors, err := s.repo.Read().ListVendors(ctx)
	if err != nil {
		loggerFrom(ctx).Error("list vendors", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not read the vendors.")
		return
	}

	out := vendorList{Items: make([]vendorResponse, 0, len(vendors))}
	for _, v := range vendors {
		out.Items = append(out.Items, newVendorResponse(v))
	}
	writeJSON(w, r, http.StatusOK, out)
}

func (s *server) handleGetVendor(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	vendor, err := s.repo.Read().GetVendor(r.Context(), id)
	if err != nil {
		s.readError(w, r, recVendor, err)
		return
	}
	writeJSON(w, r, http.StatusOK, newVendorResponse(vendor))
}

type vendorRequest struct {
	Name  string `json:"name"`
	Trade string `json:"trade"`
	Phone string `json:"phone"`
	Email string `json:"email"`
	Notes string `json:"notes"`
}

func (s *server) handleCreateVendor(w http.ResponseWriter, r *http.Request) {
	var req vendorRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if detail := validateContact(req.Name, "vendor"); detail != "" {
		WriteProblem(w, r, http.StatusUnprocessableEntity, detail)
		return
	}

	now := timestamp()
	created, err := s.repo.Write().CreateVendor(r.Context(), sqlc.CreateVendorParams{
		Name:      req.Name,
		Trade:     strings.TrimSpace(req.Trade),
		Phone:     strings.TrimSpace(req.Phone),
		Email:     strings.TrimSpace(req.Email),
		Notes:     req.Notes,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		loggerFrom(r.Context()).Error("create vendor", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not save the vendor.")
		return
	}

	w.Header().Set("Location", "/api/v1/vendors/"+strconv.FormatInt(created.ID, 10))
	writeJSON(w, r, http.StatusCreated, newVendorResponse(created))
}

var vendorPatchFields = []string{"name", "trade", "phone", "email", "notes"}

func (s *server) handleUpdateVendor(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	p, ok := decodePatch(w, r, vendorPatchFields...)
	if !ok {
		return
	}
	ctx := r.Context()

	var updated sqlc.Vendor
	err := s.repo.Tx(ctx, func(q *sqlc.Queries) error {
		current, err := q.GetVendor(ctx, id)
		if err != nil {
			return err
		}
		if err := p.apply(
			required("name", &current.Name),
			required("trade", &current.Trade),
			required("phone", &current.Phone),
			required("email", &current.Email),
			required("notes", &current.Notes),
		); err != nil {
			return err
		}

		current.Name = strings.TrimSpace(current.Name)
		if detail := validateContact(current.Name, "vendor"); detail != "" {
			return validationError{detail}
		}

		updated, err = q.UpdateVendor(ctx, sqlc.UpdateVendorParams{
			Name:      current.Name,
			Trade:     strings.TrimSpace(current.Trade),
			Phone:     strings.TrimSpace(current.Phone),
			Email:     strings.TrimSpace(current.Email),
			Notes:     current.Notes,
			UpdatedAt: timestamp(),
			ID:        id,
		})
		return err
	})
	if err != nil {
		s.writeError(w, r, recVendor, err)
		return
	}
	writeJSON(w, r, http.StatusOK, newVendorResponse(updated))
}

// handleDeleteVendor removes a vendor.
//
// The ledger entries and repairs that named them keep their money and their
// dates; the foreign keys are ON DELETE SET NULL. The payment happened, and a
// record of what happened does not go away because a contact was tidied up.
func (s *server) handleDeleteVendor(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	rows, err := s.repo.Write().DeleteVendor(r.Context(), id)
	if err != nil {
		loggerFrom(r.Context()).Error("delete vendor", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not remove the vendor.")
		return
	}
	if rows == 0 {
		WriteProblem(w, r, http.StatusNotFound, "No such vendor.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validateContact checks a name on a vendor or tenant. noun names the thing in
// the message, so the operator is told what they were editing.
func validateContact(name, noun string) string {
	switch {
	case name == "":
		return "A " + noun + " needs a name."
	case len(name) > 120:
		return "The name is longer than 120 characters."
	}
	return ""
}
