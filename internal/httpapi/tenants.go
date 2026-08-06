package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// Tenants are portfolio-wide, like vendors: a tenant who moves from one unit
// to another is the same person, and the two leases should say so.

type tenantResponse struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Phone     string `json:"phone"`
	Notes     string `json:"notes"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type tenantList struct {
	Items []tenantResponse `json:"items"`
}

func newTenantResponse(t sqlc.Tenant) tenantResponse {
	return tenantResponse{
		ID: t.ID, Name: t.Name, Email: t.Email, Phone: t.Phone, Notes: t.Notes,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

func (s *server) routeTenants(mux *http.ServeMux) {
	route(mux, "/api/v1/tenants", methods{
		http.MethodGet:  s.guarded(s.handleListTenants),
		http.MethodPost: s.guarded(s.handleCreateTenant),
	})
	route(mux, "/api/v1/tenants/{id}", methods{
		http.MethodGet:    s.guarded(s.handleGetTenant),
		http.MethodPatch:  s.guarded(s.handleUpdateTenant),
		http.MethodDelete: s.guarded(s.handleDeleteTenant),
	})
}

func (s *server) handleListTenants(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenants, err := s.repo.Read().ListTenants(ctx)
	if err != nil {
		loggerFrom(ctx).Error("list tenants", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not read the tenants.")
		return
	}

	out := tenantList{Items: make([]tenantResponse, 0, len(tenants))}
	for _, t := range tenants {
		out.Items = append(out.Items, newTenantResponse(t))
	}
	writeJSON(w, r, http.StatusOK, out)
}

func (s *server) handleGetTenant(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	tenant, err := s.repo.Read().GetTenant(r.Context(), id)
	if err != nil {
		s.tenantReadError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, newTenantResponse(tenant))
}

type tenantRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Phone string `json:"phone"`
	Notes string `json:"notes"`
}

func (s *server) handleCreateTenant(w http.ResponseWriter, r *http.Request) {
	var req tenantRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if detail := validateContact(req.Name, "tenant"); detail != "" {
		WriteProblem(w, r, http.StatusUnprocessableEntity, detail)
		return
	}

	now := timestamp()
	created, err := s.repo.Write().CreateTenant(r.Context(), sqlc.CreateTenantParams{
		Name:      req.Name,
		Email:     strings.TrimSpace(req.Email),
		Phone:     strings.TrimSpace(req.Phone),
		Notes:     req.Notes,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		loggerFrom(r.Context()).Error("create tenant", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not save the tenant.")
		return
	}

	w.Header().Set("Location", "/api/v1/tenants/"+strconv.FormatInt(created.ID, 10))
	writeJSON(w, r, http.StatusCreated, newTenantResponse(created))
}

var tenantPatchFields = []string{"name", "email", "phone", "notes"}

func (s *server) handleUpdateTenant(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	p, ok := decodePatch(w, r, tenantPatchFields...)
	if !ok {
		return
	}
	ctx := r.Context()

	var updated sqlc.Tenant
	err := s.repo.Tx(ctx, func(q *sqlc.Queries) error {
		current, err := q.GetTenant(ctx, id)
		if err != nil {
			return err
		}
		for _, apply := range []func() error{
			func() error { return p.required("name", &current.Name) },
			func() error { return p.required("email", &current.Email) },
			func() error { return p.required("phone", &current.Phone) },
			func() error { return p.required("notes", &current.Notes) },
		} {
			if err := apply(); err != nil {
				return validationError{err.Error()}
			}
		}

		current.Name = strings.TrimSpace(current.Name)
		if detail := validateContact(current.Name, "tenant"); detail != "" {
			return validationError{detail}
		}

		updated, err = q.UpdateTenant(ctx, sqlc.UpdateTenantParams{
			Name:      current.Name,
			Email:     strings.TrimSpace(current.Email),
			Phone:     strings.TrimSpace(current.Phone),
			Notes:     current.Notes,
			UpdatedAt: timestamp(),
			ID:        id,
		})
		return err
	})
	if err != nil {
		s.tenantWriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, newTenantResponse(updated))
}

// handleDeleteTenant removes a tenant and their place on every lease.
//
// lease_tenants cascades, but the leases themselves stay: the tenancy happened
// and its dates and rent are still the record of what happened, whoever has
// since been removed from the contact list.
func (s *server) handleDeleteTenant(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	rows, err := s.repo.Write().DeleteTenant(r.Context(), id)
	if err != nil {
		loggerFrom(r.Context()).Error("delete tenant", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not remove the tenant.")
		return
	}
	if rows == 0 {
		WriteProblem(w, r, http.StatusNotFound, "No such tenant.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) tenantReadError(w http.ResponseWriter, r *http.Request, err error) {
	if store.NotFound(err) {
		WriteProblem(w, r, http.StatusNotFound, "No such tenant.")
		return
	}
	loggerFrom(r.Context()).Error("read tenant", "error", err)
	WriteProblem(w, r, http.StatusInternalServerError, "Could not read the tenant.")
}

func (s *server) tenantWriteError(w http.ResponseWriter, r *http.Request, err error) {
	var invalid validationError
	switch {
	case errors.As(err, &invalid):
		WriteProblem(w, r, http.StatusUnprocessableEntity, invalid.detail)
	case store.NotFound(err):
		WriteProblem(w, r, http.StatusNotFound, "No such tenant.")
	default:
		loggerFrom(r.Context()).Error("write tenant", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not save the tenant.")
	}
}
