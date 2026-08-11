package httpapi

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

const (
	defaultPageSize = 50
	maxPageSize     = 200

	// implicitUnitLabel names the unit a single-family property gets at
	// creation. Every lease hangs off a unit (docs/DESIGN.md §3.2), so a
	// property with no units would fork the query shape for the rest of the
	// application. "Main" reads correctly on a house without pretending to be
	// a street-facing unit number.
	implicitUnitLabel = "Main"
)

// propertyStatuses mirrors the CHECK constraint in migration 0001. The
// database is the authority; this exists so a bad value is a 422 naming the
// choices rather than a 500 carrying a constraint message.
var propertyStatuses = []string{"active", "sold", "prospect"}

// propertyResponse is a property on the wire.
//
// Money is an integer count of cents, here as everywhere (docs/DESIGN.md §3).
// Nullable columns are pointers so that null survives the trip: a property
// with no recorded purchase price is not a property that cost nothing.
type propertyResponse struct {
	ID                 int64         `json:"id"`
	Nickname           string        `json:"nickname"`
	AddressLine1       string        `json:"address_line1"`
	AddressLine2       string        `json:"address_line2"`
	City               string        `json:"city"`
	State              string        `json:"state"`
	PostalCode         string        `json:"postal_code"`
	County             string        `json:"county"`
	NormalizedAddress  string        `json:"normalized_address"`
	PurchaseDate       *string       `json:"purchase_date"`
	PurchasePriceCents *domain.Money `json:"purchase_price_cents"`
	Beds               *int64        `json:"beds"`
	Baths              *float64      `json:"baths"`
	Sqft               *int64        `json:"sqft"`
	YearBuilt          *int64        `json:"year_built"`
	Status             string        `json:"status"`
	Zpid               *string       `json:"zpid"`
	Notes              string        `json:"notes"`
	CreatedAt          string        `json:"created_at"`
	UpdatedAt          string        `json:"updated_at"`
}

// propertyListItem is an index row: the property plus how many units it has,
// which the index card shows and which the list query already counted.
type propertyListItem struct {
	propertyResponse
	UnitCount int64 `json:"unit_count"`
}

// propertyDetail carries the units inline, so opening a property is one
// request rather than two.
type propertyDetail struct {
	propertyResponse
	Units []unitResponse `json:"units"`
}

type propertyList struct {
	Items []propertyListItem `json:"items"`
	// NextCursor is absent on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

func newPropertyResponse(p sqlc.Property) propertyResponse {
	return propertyResponse{
		ID: p.ID, Nickname: p.Nickname,
		AddressLine1: p.AddressLine1, AddressLine2: p.AddressLine2,
		City: p.City, State: p.State, PostalCode: p.PostalCode, County: p.County,
		NormalizedAddress: p.NormalizedAddress,
		PurchaseDate:      p.PurchaseDate, PurchasePriceCents: p.PurchasePriceCents,
		Beds: p.Beds, Baths: p.Baths, Sqft: p.Sqft, YearBuilt: p.YearBuilt,
		Status: p.Status, Zpid: p.Zpid, Notes: p.Notes,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

func (s *server) routeProperties(mux *http.ServeMux) {
	route(mux, "/api/v1/properties", methods{
		http.MethodGet:  s.guarded(s.handleListProperties),
		http.MethodPost: s.guarded(s.handleCreateProperty),
	})
	route(mux, "/api/v1/properties/{id}", methods{
		http.MethodGet:    s.guarded(s.handleGetProperty),
		http.MethodPatch:  s.guarded(s.handleUpdateProperty),
		http.MethodDelete: s.guarded(s.handleDeleteProperty),
	})
	route(mux, "/api/v1/properties/{id}/units", methods{
		http.MethodGet:  s.guarded(s.handleListUnits),
		http.MethodPost: s.guarded(s.handleCreateUnit),
	})
	// Not in the §7.1 table, which gives no path for mutating one unit. The
	// screen needs both, so they live here and CLAUDE.md records the addition.
	route(mux, "/api/v1/units/{id}", methods{
		http.MethodPatch:  s.guarded(s.handleUpdateUnit),
		http.MethodDelete: s.guarded(s.handleDeleteUnit),
	})
}

func (s *server) handleListProperties(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	size, ok := pageSize(w, r)
	if !ok {
		return
	}

	// One more than asked for, so the presence of a next page is a fact rather
	// than a second count query that can disagree with the first.
	rows, err := s.listPage(ctx, r.URL.Query().Get("cursor"), size+1)
	if err != nil {
		if errors.Is(err, errBadCursor) {
			WriteProblem(w, r, http.StatusBadRequest, "The cursor is not one this endpoint issued.")
			return
		}
		loggerFrom(ctx).Error("list properties", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not read the properties.")
		return
	}

	out := propertyList{Items: make([]propertyListItem, 0, size)}
	for i, row := range rows {
		if i == size {
			last := rows[i-1]
			out.NextCursor = encodeCursor(last.Property.Nickname, last.Property.ID)
			break
		}
		out.Items = append(out.Items, propertyListItem{
			propertyResponse: newPropertyResponse(row.Property),
			UnitCount:        row.UnitCount,
		})
	}

	writeJSON(w, r, http.StatusOK, out)
}

// listRow flattens the two generated row types, which differ only in name.
type listRow struct {
	Property  sqlc.Property
	UnitCount int64
}

func (s *server) listPage(ctx context.Context, cursor string, limit int) ([]listRow, error) {
	if cursor == "" {
		rows, err := s.repo.Read().ListPropertiesFirstPage(ctx, int64(limit))
		if err != nil {
			return nil, err
		}
		out := make([]listRow, len(rows))
		for i, r := range rows {
			out[i] = listRow{Property: r.Property, UnitCount: r.UnitCount}
		}
		return out, nil
	}

	nickname, id, err := decodeCursor(cursor)
	if err != nil {
		return nil, err
	}
	rows, err := s.repo.Read().ListPropertiesAfter(ctx, sqlc.ListPropertiesAfterParams{
		AfterNickname: nickname,
		AfterID:       id,
		PageSize:      int64(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]listRow, len(rows))
	for i, r := range rows {
		out[i] = listRow{Property: r.Property, UnitCount: r.UnitCount}
	}
	return out, nil
}

func (s *server) handleGetProperty(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ctx := r.Context()

	property, err := s.repo.Read().GetProperty(ctx, id)
	if err != nil {
		s.propertyReadError(w, r, err)
		return
	}
	// The detail carries occupancy, so the Overview tab can say which units
	// are let without a second request. It is derived from the lease dates on
	// every read rather than stored.
	units, err := s.repo.Read().ListUnitsWithOccupancy(ctx, sqlc.ListUnitsWithOccupancyParams{
		PropertyID: id, Today: domain.Today(),
	})
	if err != nil {
		loggerFrom(ctx).Error("list units", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not read the units.")
		return
	}

	writeJSON(w, r, http.StatusOK, propertyDetail{
		propertyResponse: newPropertyResponse(property),
		Units:            newOccupiedUnitResponses(units),
	})
}

type createPropertyRequest struct {
	Nickname           string        `json:"nickname"`
	AddressLine1       string        `json:"address_line1"`
	AddressLine2       string        `json:"address_line2"`
	City               string        `json:"city"`
	State              string        `json:"state"`
	PostalCode         string        `json:"postal_code"`
	County             string        `json:"county"`
	PurchaseDate       *string       `json:"purchase_date"`
	PurchasePriceCents *domain.Money `json:"purchase_price_cents"`
	Beds               *int64        `json:"beds"`
	Baths              *float64      `json:"baths"`
	Sqft               *int64        `json:"sqft"`
	YearBuilt          *int64        `json:"year_built"`
	Status             string        `json:"status"`
	Zpid               *string       `json:"zpid"`
	Notes              string        `json:"notes"`
	// Units is optional. A single-family property that sends none gets one
	// implicit unit; a multi-family one names its own.
	Units []unitInput `json:"units"`
}

func (s *server) handleCreateProperty(w http.ResponseWriter, r *http.Request) {
	var req createPropertyRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	req.Nickname = strings.TrimSpace(req.Nickname)
	req.AddressLine1 = strings.TrimSpace(req.AddressLine1)
	if req.Status == "" {
		req.Status = "active"
	}

	if detail := validateProperty(req.Nickname, req.AddressLine1, req.Status,
		req.PurchaseDate, req.Beds, req.Baths, req.Sqft, req.YearBuilt); detail != "" {
		WriteProblem(w, r, http.StatusUnprocessableEntity, detail)
		return
	}

	units := req.Units
	if len(units) == 0 {
		units = []unitInput{{Label: implicitUnitLabel}}
	}
	if detail := validateUnits(units); detail != "" {
		WriteProblem(w, r, http.StatusUnprocessableEntity, detail)
		return
	}

	ctx := r.Context()
	now := timestamp()

	var created sqlc.Property
	var madeUnits []sqlc.Unit

	// The property and its units land together or not at all: a property that
	// exists with no units breaks the invariant the units table is here for.
	err := s.repo.Tx(ctx, func(q *sqlc.Queries) error {
		var err error
		created, err = q.CreateProperty(ctx, sqlc.CreatePropertyParams{
			Nickname:     req.Nickname,
			AddressLine1: req.AddressLine1,
			AddressLine2: req.AddressLine2,
			City:         req.City,
			State:        req.State,
			PostalCode:   req.PostalCode,
			County:       req.County,
			NormalizedAddress: domain.NormalizeAddress(
				req.AddressLine1, req.AddressLine2, req.City, req.State, req.PostalCode),
			PurchaseDate:       req.PurchaseDate,
			PurchasePriceCents: req.PurchasePriceCents,
			Beds:               req.Beds,
			Baths:              req.Baths,
			Sqft:               req.Sqft,
			YearBuilt:          req.YearBuilt,
			Status:             req.Status,
			Zpid:               req.Zpid,
			Notes:              req.Notes,
			CreatedAt:          now,
			UpdatedAt:          now,
		})
		if err != nil {
			return err
		}

		for _, u := range units {
			unit, err := q.CreateUnit(ctx, sqlc.CreateUnitParams{
				PropertyID: created.ID,
				Label:      strings.TrimSpace(u.Label),
				Beds:       u.Beds,
				Baths:      u.Baths,
				Sqft:       u.Sqft,
				CreatedAt:  now,
				UpdatedAt:  now,
			})
			if err != nil {
				return err
			}
			madeUnits = append(madeUnits, unit)
		}
		return nil
	})
	if err != nil {
		if store.Conflict(err) {
			WriteProblem(w, r, http.StatusConflict, "Two units cannot share a label.")
			return
		}
		loggerFrom(ctx).Error("create property", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not save the property.")
		return
	}

	w.Header().Set("Location", "/api/v1/properties/"+strconv.FormatInt(created.ID, 10))
	writeJSON(w, r, http.StatusCreated, propertyDetail{
		propertyResponse: newPropertyResponse(created),
		Units:            newUnitResponses(madeUnits),
	})
}

// propertyPatchFields are the fields PATCH accepts. normalized_address is not
// among them: it is derived from the address and recomputed here, so letting a
// client set it would let the match key disagree with the address it names.
var propertyPatchFields = []string{
	"nickname", "address_line1", "address_line2", "city", "state",
	"postal_code", "county", "purchase_date", "purchase_price_cents",
	"beds", "baths", "sqft", "year_built", "status", "zpid", "notes",
}

func (s *server) handleUpdateProperty(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	p, ok := decodePatch(w, r, propertyPatchFields...)
	if !ok {
		return
	}

	ctx := r.Context()
	var updated sqlc.Property

	// Read, merge, write, all inside the write transaction. COALESCE cannot
	// express the difference between an absent field and an explicit null, so
	// the merge happens in Go against the current row.
	err := s.repo.Tx(ctx, func(q *sqlc.Queries) error {
		current, err := q.GetProperty(ctx, id)
		if err != nil {
			return err
		}

		if err := applyPropertyPatch(p, &current); err != nil {
			return err
		}
		if detail := validateProperty(current.Nickname, current.AddressLine1, current.Status,
			current.PurchaseDate, current.Beds, current.Baths, current.Sqft, current.YearBuilt); detail != "" {
			return validationError{detail}
		}

		// The match key follows the address it describes, always.
		current.NormalizedAddress = domain.NormalizeAddress(
			current.AddressLine1, current.AddressLine2,
			current.City, current.State, current.PostalCode)

		updated, err = q.UpdateProperty(ctx, sqlc.UpdatePropertyParams{
			Nickname:           current.Nickname,
			AddressLine1:       current.AddressLine1,
			AddressLine2:       current.AddressLine2,
			City:               current.City,
			State:              current.State,
			PostalCode:         current.PostalCode,
			County:             current.County,
			NormalizedAddress:  current.NormalizedAddress,
			PurchaseDate:       current.PurchaseDate,
			PurchasePriceCents: current.PurchasePriceCents,
			Beds:               current.Beds,
			Baths:              current.Baths,
			Sqft:               current.Sqft,
			YearBuilt:          current.YearBuilt,
			Status:             current.Status,
			Zpid:               current.Zpid,
			Notes:              current.Notes,
			UpdatedAt:          timestamp(),
			ID:                 id,
		})
		return err
	})
	if err != nil {
		s.propertyWriteError(w, r, err)
		return
	}

	units, err := s.repo.Read().ListUnitsWithOccupancy(ctx, sqlc.ListUnitsWithOccupancyParams{
		PropertyID: id, Today: domain.Today(),
	})
	if err != nil {
		loggerFrom(ctx).Error("list units", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "The property was saved but its units could not be read.")
		return
	}

	writeJSON(w, r, http.StatusOK, propertyDetail{
		propertyResponse: newPropertyResponse(updated),
		Units:            newOccupiedUnitResponses(units),
	})
}

func applyPropertyPatch(p patch, into *sqlc.Property) error {
	for _, apply := range []func() error{
		func() error { return p.required("nickname", &into.Nickname) },
		func() error { return p.required("address_line1", &into.AddressLine1) },
		func() error { return p.required("address_line2", &into.AddressLine2) },
		func() error { return p.required("city", &into.City) },
		func() error { return p.required("state", &into.State) },
		func() error { return p.required("postal_code", &into.PostalCode) },
		func() error { return p.required("county", &into.County) },
		func() error { return p.required("status", &into.Status) },
		func() error { return p.required("notes", &into.Notes) },
		func() error { return p.nullable("purchase_date", &into.PurchaseDate) },
		func() error { return p.nullable("purchase_price_cents", &into.PurchasePriceCents) },
		func() error { return p.nullable("beds", &into.Beds) },
		func() error { return p.nullable("baths", &into.Baths) },
		func() error { return p.nullable("sqft", &into.Sqft) },
		func() error { return p.nullable("year_built", &into.YearBuilt) },
		func() error { return p.nullable("zpid", &into.Zpid) },
	} {
		if err := apply(); err != nil {
			return validationError{err.Error()}
		}
	}

	into.Nickname = strings.TrimSpace(into.Nickname)
	into.AddressLine1 = strings.TrimSpace(into.AddressLine1)
	return nil
}

// handleDeleteProperty removes a property outright.
//
// This is a hard delete: status 'sold' already records a property you no
// longer own, so reaching for delete means the record itself was a mistake.
// Units go with it, by the foreign key's ON DELETE CASCADE.
func (s *server) handleDeleteProperty(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}

	rows, err := s.repo.Write().DeleteProperty(r.Context(), id)
	if err != nil {
		loggerFrom(r.Context()).Error("delete property", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not delete the property.")
		return
	}
	if rows == 0 {
		WriteProblem(w, r, http.StatusNotFound, "No such property.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validationError carries a client-facing message out of a transaction.
type validationError struct{ detail string }

func (e validationError) Error() string { return e.detail }

// validateProperty checks the rules the database cannot state for itself, and
// returns a message naming what to fix.
func validateProperty(nickname, addressLine1, status string, purchaseDate *string,
	beds *int64, baths *float64, sqft, yearBuilt *int64) string {

	switch {
	case nickname == "":
		return "A property needs a nickname."
	case len(nickname) > 120:
		return "The nickname is longer than 120 characters."
	case addressLine1 == "":
		return "A property needs a street address."
	case len(addressLine1) > 200:
		return "The street address is longer than 200 characters."
	}

	if !slices.Contains(propertyStatuses, status) {
		return "Status has to be one of " + strings.Join(propertyStatuses, ", ") + "."
	}
	if purchaseDate != nil && !isCalendarDate(*purchaseDate) {
		return "The purchase date has to be written as YYYY-MM-DD."
	}
	if beds != nil && (*beds < 0 || *beds > 1000) {
		return "Beds has to be between 0 and 1000."
	}
	if baths != nil && (*baths < 0 || *baths > 1000) {
		return "Baths has to be between 0 and 1000."
	}
	if sqft != nil && (*sqft < 0 || *sqft > 10_000_000) {
		return "Square feet has to be between 0 and 10,000,000."
	}
	if yearBuilt != nil && (*yearBuilt < 1000 || *yearBuilt > 2200) {
		return "The year built has to be between 1000 and 2200."
	}
	return ""
}

// isCalendarDate reports whether s is a YYYY-MM-DD date that exists.
//
// Dates that come off documents are stored exactly as written, with no
// timezone invented for them (docs/DESIGN.md §3), so this checks the spelling
// rather than parsing into a time.Time and back.
func isCalendarDate(s string) bool {
	t, err := time.Parse(time.DateOnly, s)
	return err == nil && t.Format(time.DateOnly) == s
}

func (s *server) propertyReadError(w http.ResponseWriter, r *http.Request, err error) {
	if store.NotFound(err) {
		WriteProblem(w, r, http.StatusNotFound, "No such property.")
		return
	}
	loggerFrom(r.Context()).Error("read property", "error", err)
	WriteProblem(w, r, http.StatusInternalServerError, "Could not read the property.")
}

func (s *server) propertyWriteError(w http.ResponseWriter, r *http.Request, err error) {
	var invalid validationError
	switch {
	case errors.As(err, &invalid):
		WriteProblem(w, r, http.StatusUnprocessableEntity, invalid.detail)
	case store.NotFound(err):
		WriteProblem(w, r, http.StatusNotFound, "No such property.")
	case store.Conflict(err):
		WriteProblem(w, r, http.StatusConflict, "That would collide with a record already on file.")
	default:
		loggerFrom(r.Context()).Error("write property", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not save the property.")
	}
}

// pageSize reads the limit query parameter.
func pageSize(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return defaultPageSize, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		WriteProblem(w, r, http.StatusBadRequest, "limit has to be a positive whole number.")
		return 0, false
	}
	return min(n, maxPageSize), true
}

var errBadCursor = errors.New("httpapi: malformed cursor")

// encodeCursor names the last row of a page by its sort key.
//
// Every keyset in this API sorts on a text column and breaks the tie on id, so
// the cursor carries both: properties sort by nickname and documents by
// created_at, and neither is unique. A cursor that carried only the first would
// skip or repeat rows wherever two rows share it.
func encodeCursor(sortKey string, id int64) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(sortKey + "\x00" + strconv.FormatInt(id, 10)))
}

func decodeCursor(cursor string) (string, int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", 0, errBadCursor
	}
	sortKey, rest, found := strings.Cut(string(raw), "\x00")
	if !found {
		return "", 0, errBadCursor
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return "", 0, errBadCursor
	}
	return sortKey, id, nil
}

// timestamp is now, spelled the way this schema spells every timestamp. Unlike
// the subsystems with an injectable clock, a request handler has no clock to
// inject -- it always means this instant.
func timestamp() string { return domain.Stamp(time.Now()) }

// pathID reads a numeric path parameter, reporting a bad one and returning
// false. A non-numeric id is a 404 rather than a 400: /properties/banana names
// no property, and saying so is enough.
func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id < 1 {
		WriteProblem(w, r, http.StatusNotFound, "No such record.")
		return 0, false
	}
	return id, true
}
