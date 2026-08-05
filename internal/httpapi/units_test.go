package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

func unitsPath(propertyID int64) string {
	return "/api/v1/properties/" + itoa(propertyID) + "/units"
}

func unitPath(unitID int64) string {
	return "/api/v1/units/" + itoa(unitID)
}

func TestCreateUnit(t *testing.T) {
	a := newAPI(t)
	property := a.newProperty(elmStreet())

	var created unitResponse
	a.decode(a.do(http.MethodPost, unitsPath(property.ID),
		map[string]any{"label": "Upper", "beds": 2, "baths": 1.0, "sqft": 780}),
		http.StatusCreated, &created)

	if created.Label != "Upper" || created.PropertyID != property.ID {
		t.Errorf("created = %+v", created)
	}
	if created.Beds == nil || *created.Beds != 2 {
		t.Errorf("Beds = %v, want 2", created.Beds)
	}
	if created.Baths == nil || *created.Baths != 1.0 {
		t.Errorf("Baths = %v, want 1", created.Baths)
	}

	var list unitList
	a.decode(a.do(http.MethodGet, unitsPath(property.ID), nil), http.StatusOK, &list)
	if len(list.Items) != 2 {
		t.Errorf("%d units, want 2", len(list.Items))
	}
}

func TestCreateUnitRejectsADuplicateLabel(t *testing.T) {
	// UNIQUE (property_id, label) is what stops a lease attaching to the wrong
	// one of two units called "Main".
	a := newAPI(t)
	property := a.newProperty(elmStreet())

	rec := a.do(http.MethodPost, unitsPath(property.ID), map[string]any{"label": implicitUnitLabel})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), implicitUnitLabel) {
		t.Errorf("the conflict does not name the label: %s", rec.Body)
	}
}

func TestCreateUnitOnAMissingPropertyIs404(t *testing.T) {
	a := newAPI(t)
	rec := a.do(http.MethodPost, unitsPath(999), map[string]any{"label": "Upper"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestListUnitsOnAMissingPropertyIs404(t *testing.T) {
	// Not an empty list, which would read as "this property has no units".
	a := newAPI(t)
	if rec := a.do(http.MethodGet, unitsPath(999), nil); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestUpdateUnit(t *testing.T) {
	a := newAPI(t)
	property := a.newProperty(elmStreet())
	unit := property.Units[0]

	var got unitResponse
	a.decode(a.do(http.MethodPatch, unitPath(unit.ID),
		map[string]any{"label": "Whole house", "sqft": 1450}), http.StatusOK, &got)

	if got.Label != "Whole house" {
		t.Errorf("Label = %q, want %q", got.Label, "Whole house")
	}
	if got.Sqft == nil || *got.Sqft != 1450 {
		t.Errorf("Sqft = %v, want 1450", got.Sqft)
	}
}

func TestUpdateUnitDistinguishesAbsentFromNull(t *testing.T) {
	a := newAPI(t)
	property := a.newProperty(elmStreet())
	unit := property.Units[0]

	a.decode(a.do(http.MethodPatch, unitPath(unit.ID),
		map[string]any{"beds": 2, "sqft": 900}), http.StatusOK, nil)

	// A patch that names only sqft leaves beds alone.
	var kept unitResponse
	a.decode(a.do(http.MethodPatch, unitPath(unit.ID),
		map[string]any{"sqft": 950}), http.StatusOK, &kept)
	if kept.Beds == nil || *kept.Beds != 2 {
		t.Errorf("Beds = %v after an unrelated patch, want 2", kept.Beds)
	}

	// An explicit null clears it.
	var cleared unitResponse
	a.decode(a.do(http.MethodPatch, unitPath(unit.ID),
		map[string]any{"beds": nil}), http.StatusOK, &cleared)
	if cleared.Beds != nil {
		t.Errorf("Beds = %v after an explicit null, want nil", *cleared.Beds)
	}
	if cleared.Sqft == nil || *cleared.Sqft != 950 {
		t.Errorf("Sqft = %v; clearing beds should not have touched it", cleared.Sqft)
	}
}

func TestUpdateUnitRejectsBadInput(t *testing.T) {
	a := newAPI(t)
	property := a.newProperty(elmStreet())
	unit := property.Units[0]

	tests := []struct {
		name string
		body map[string]any
		want int
	}{
		{"blank label", map[string]any{"label": "  "}, http.StatusUnprocessableEntity},
		{"null label", map[string]any{"label": nil}, http.StatusUnprocessableEntity},
		{"negative beds", map[string]any{"beds": -2}, http.StatusUnprocessableEntity},
		{"unknown field", map[string]any{"rooms": 4}, http.StatusBadRequest},
		{"empty body", map[string]any{}, http.StatusBadRequest},
	}
	for _, tt := range tests {
		if rec := a.do(http.MethodPatch, unitPath(unit.ID), tt.body); rec.Code != tt.want {
			t.Errorf("%s: status = %d, want %d (body %s)", tt.name, rec.Code, tt.want, rec.Body)
		}
	}
}

func TestUpdateUnitToAnotherUnitsLabelIs409(t *testing.T) {
	a := newAPI(t)
	property := a.newProperty(elmStreet())
	a.decode(a.do(http.MethodPost, unitsPath(property.ID),
		map[string]any{"label": "Upper"}), http.StatusCreated, nil)

	rec := a.do(http.MethodPatch, unitPath(property.Units[0].ID), map[string]any{"label": "Upper"})
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 (body %s)", rec.Code, rec.Body)
	}
}

func TestDeleteTheLastUnitIsRefused(t *testing.T) {
	// Every lease hangs off a unit. A property with none would have nowhere to
	// put one, and would fork the query shape for every later milestone.
	a := newAPI(t)
	property := a.newProperty(elmStreet())

	rec := a.do(http.MethodDelete, unitPath(property.Units[0].ID), nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body)
	}
	// The refusal says what to do about it.
	if !strings.Contains(rec.Body.String(), "Add another") {
		t.Errorf("the refusal does not say how to proceed: %s", rec.Body)
	}

	var list unitList
	a.decode(a.do(http.MethodGet, unitsPath(property.ID), nil), http.StatusOK, &list)
	if len(list.Items) != 1 {
		t.Errorf("%d units survived, want 1", len(list.Items))
	}
}

func TestDeleteAUnitWhenAnotherRemains(t *testing.T) {
	a := newAPI(t)
	property := a.newProperty(elmStreet())

	var added unitResponse
	a.decode(a.do(http.MethodPost, unitsPath(property.ID),
		map[string]any{"label": "Upper"}), http.StatusCreated, &added)

	if rec := a.do(http.MethodDelete, unitPath(added.ID), nil); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body %s)", rec.Code, rec.Body)
	}

	var list unitList
	a.decode(a.do(http.MethodGet, unitsPath(property.ID), nil), http.StatusOK, &list)
	if len(list.Items) != 1 || list.Items[0].Label != implicitUnitLabel {
		t.Errorf("units after deletion = %+v, want just the implicit one", list.Items)
	}
}

func TestDeleteAMissingUnitIs404(t *testing.T) {
	a := newAPI(t)
	if rec := a.do(http.MethodDelete, unitPath(999), nil); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
