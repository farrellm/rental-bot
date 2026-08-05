package httpapi

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/farrellm/rental-bot/internal/domain"
)

func (a *api) newLease(propertyID int64, body map[string]any) leaseResponse {
	a.t.Helper()
	var out leaseResponse
	a.decode(a.do(http.MethodPost, propertyPath(propertyID)+"/leases", body),
		http.StatusCreated, &out)
	return out
}

func (a *api) newTenant(body map[string]any) tenantResponse {
	a.t.Helper()
	var out tenantResponse
	a.decode(a.do(http.MethodPost, "/api/v1/tenants", body), http.StatusCreated, &out)
	return out
}

func leasePath(id int64, suffix string) string {
	return "/api/v1/leases/" + itoa(id) + suffix
}

// duplex returns a property with two units, which is what leases need.
func duplex(a *api) (propertyDetail, unitResponse, unitResponse) {
	a.t.Helper()
	p := a.newProperty(map[string]any{
		"nickname":      "Elm Street Duplex",
		"address_line1": "412 Elm Street",
		"units":         []map[string]any{{"label": "Apt 1"}, {"label": "Apt 2"}},
	})
	if len(p.Units) != 2 {
		a.t.Fatalf("%d units, want 2", len(p.Units))
	}
	return p, p.Units[0], p.Units[1]
}

func daysOut(n int) string {
	return time.Now().UTC().AddDate(0, 0, n).Format(time.DateOnly)
}

func TestLeaseRoundTrips(t *testing.T) {
	a := newAPI(t)
	p, unit, _ := duplex(a)

	lease := a.newLease(p.ID, map[string]any{
		"unit_id":        unit.ID,
		"start_date":     "2026-01-01",
		"end_date":       "2026-12-31",
		"rent_cents":     145000,
		"deposit_cents":  145000,
		"due_day":        1,
		"late_fee_cents": 5000,
		"status":         "active",
	})

	if lease.UnitLabel != unit.Label {
		t.Errorf("UnitLabel = %q, want %q", lease.UnitLabel, unit.Label)
	}
	if lease.RentCents != domain.Money(145000) {
		t.Errorf("RentCents = %d, want 145000", lease.RentCents)
	}
	if lease.DueDay == nil || *lease.DueDay != 1 {
		t.Errorf("DueDay = %v, want 1", lease.DueDay)
	}
}

func TestAMonthToMonthLeaseHasNoEndDate(t *testing.T) {
	// A null end_date is an open-ended tenancy, not a missing value. The term
	// rule on the screen draws an open end from exactly this.
	a := newAPI(t)
	p, unit, _ := duplex(a)

	lease := a.newLease(p.ID, map[string]any{
		"unit_id": unit.ID, "start_date": "2026-01-01",
		"rent_cents": 95000, "status": "active",
	})
	if lease.EndDate != nil {
		t.Errorf("EndDate = %v, want nil", *lease.EndDate)
	}
}

func TestOneUnitHoldsOneLiveLease(t *testing.T) {
	// Occupancy is derived from the lease dates rather than stored, so the
	// write path is the only place that can keep the answer unambiguous. Two
	// live leases covering the same days would make "is this unit let" a
	// question with two correct answers.
	a := newAPI(t)
	p, unit, other := duplex(a)

	a.newLease(p.ID, map[string]any{
		"unit_id": unit.ID, "start_date": "2026-01-01", "end_date": "2026-12-31",
		"rent_cents": 145000, "status": "active",
	})

	tests := []struct {
		name string
		body map[string]any
		want int
	}{
		{
			name: "starting inside the term",
			body: map[string]any{"unit_id": unit.ID, "start_date": "2026-06-01",
				"end_date": "2027-05-31", "rent_cents": 150000, "status": "active"},
			want: http.StatusConflict,
		},
		{
			name: "an open-ended lease swallowing it",
			body: map[string]any{"unit_id": unit.ID, "start_date": "2025-01-01",
				"rent_cents": 150000, "status": "active"},
			want: http.StatusConflict,
		},
		{
			name: "a pending lease is still a claim on the unit",
			body: map[string]any{"unit_id": unit.ID, "start_date": "2026-06-01",
				"end_date": "2026-08-31", "rent_cents": 150000, "status": "pending"},
			want: http.StatusConflict,
		},
		{
			name: "the renewal that starts the day after",
			body: map[string]any{"unit_id": unit.ID, "start_date": "2027-01-01",
				"end_date": "2027-12-31", "rent_cents": 150000, "status": "active"},
			want: http.StatusCreated,
		},
		{
			name: "history, which overlaps nothing",
			body: map[string]any{"unit_id": unit.ID, "start_date": "2026-03-01",
				"end_date": "2026-06-30", "rent_cents": 150000, "status": "ended"},
			want: http.StatusCreated,
		},
		{
			name: "the other unit, which is its own question",
			body: map[string]any{"unit_id": other.ID, "start_date": "2026-01-01",
				"end_date": "2026-12-31", "rent_cents": 99000, "status": "active"},
			want: http.StatusCreated,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := a.do(http.MethodPost, propertyPath(p.ID)+"/leases", tt.body)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.want, rec.Body)
			}
			if tt.want == http.StatusConflict && !strings.Contains(rec.Body.String(), "End that one first") {
				t.Errorf("the refusal does not say what to do: %s", rec.Body)
			}
		})
	}
}

func TestAmendingALeaseDoesNotCollideWithItself(t *testing.T) {
	// The overlap check has to exclude the row being amended, or extending a
	// lease by a month would report the lease colliding with itself.
	a := newAPI(t)
	p, unit, _ := duplex(a)

	lease := a.newLease(p.ID, map[string]any{
		"unit_id": unit.ID, "start_date": "2026-01-01", "end_date": "2026-12-31",
		"rent_cents": 145000, "status": "active",
	})

	var extended leaseResponse
	a.decode(a.do(http.MethodPatch, leasePath(lease.ID, ""),
		map[string]any{"end_date": "2027-01-31"}), http.StatusOK, &extended)

	if extended.EndDate == nil || *extended.EndDate != "2027-01-31" {
		t.Errorf("EndDate = %v, want 2027-01-31", extended.EndDate)
	}
}

func TestEndingALeaseFreesTheUnit(t *testing.T) {
	// The way out of the conflict, and the thing the refusal tells you to do.
	a := newAPI(t)
	p, unit, _ := duplex(a)

	held := a.newLease(p.ID, map[string]any{
		"unit_id": unit.ID, "start_date": "2026-01-01", "end_date": "2026-12-31",
		"rent_cents": 145000, "status": "active",
	})

	blocked := a.do(http.MethodPost, propertyPath(p.ID)+"/leases", map[string]any{
		"unit_id": unit.ID, "start_date": "2026-06-01", "end_date": "2027-05-31",
		"rent_cents": 150000, "status": "active",
	})
	if blocked.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", blocked.Code)
	}

	a.decode(a.do(http.MethodPatch, leasePath(held.ID, ""),
		map[string]any{"status": "terminated", "end_date": "2026-05-31"}), http.StatusOK, nil)

	a.newLease(p.ID, map[string]any{
		"unit_id": unit.ID, "start_date": "2026-06-01", "end_date": "2027-05-31",
		"rent_cents": 150000, "status": "active",
	})
}

func TestOccupancyIsDerivedOnEveryRead(t *testing.T) {
	// Nothing is written to say a unit is occupied. The property detail asks
	// the lease dates, every time.
	a := newAPI(t)
	p, let, vacant := duplex(a)

	lease := a.newLease(p.ID, map[string]any{
		"unit_id": let.ID, "start_date": daysOut(-200), "end_date": daysOut(160),
		"rent_cents": 145000, "status": "active",
	})

	var detail propertyDetail
	a.decode(a.do(http.MethodGet, propertyPath(p.ID), nil), http.StatusOK, &detail)

	byLabel := map[string]unitResponse{}
	for _, u := range detail.Units {
		byLabel[u.Label] = u
	}

	occupied := byLabel[let.Label]
	if occupied.ActiveLeaseID == nil || *occupied.ActiveLeaseID != lease.ID {
		t.Errorf("%s active lease = %v, want %d", let.Label, occupied.ActiveLeaseID, lease.ID)
	}
	if occupied.ActiveLeaseEndDate == nil || *occupied.ActiveLeaseEndDate != daysOut(160) {
		t.Errorf("%s lease end = %v, want %s", let.Label, occupied.ActiveLeaseEndDate, daysOut(160))
	}
	if byLabel[vacant.Label].ActiveLeaseID != nil {
		t.Errorf("%s reports a lease it does not have", vacant.Label)
	}
}

func TestALapsedLeaseStopsCountingOnItsOwn(t *testing.T) {
	// The lease is still marked active and nothing was run to change it. The
	// unit reads as vacant because the dates say so, which is the whole point
	// of deriving the answer.
	a := newAPI(t)
	p, unit, _ := duplex(a)

	a.newLease(p.ID, map[string]any{
		"unit_id": unit.ID, "start_date": daysOut(-400), "end_date": daysOut(-10),
		"rent_cents": 145000, "status": "active",
	})

	var detail propertyDetail
	a.decode(a.do(http.MethodGet, propertyPath(p.ID), nil), http.StatusOK, &detail)

	for _, u := range detail.Units {
		if u.ID == unit.ID && u.ActiveLeaseID != nil {
			t.Errorf("%s still reports a lease that ended ten days ago", u.Label)
		}
	}
}

func TestLeaseRejectsWhatItCannotRead(t *testing.T) {
	a := newAPI(t)
	p, unit, _ := duplex(a)
	other := a.newProperty(map[string]any{"nickname": "Oak", "address_line1": "9 Oak St"})

	tests := []struct {
		name     string
		body     map[string]any
		want     int
		wantText string
	}{
		{
			name: "ending before it starts",
			body: map[string]any{"unit_id": unit.ID, "start_date": "2026-12-31",
				"end_date": "2026-01-01", "rent_cents": 100000},
			want:     http.StatusUnprocessableEntity,
			wantText: "before it starts",
		},
		{
			name: "a due day that is not a day",
			body: map[string]any{"unit_id": unit.ID, "start_date": "2026-01-01",
				"rent_cents": 100000, "due_day": 32},
			want: http.StatusUnprocessableEntity,
		},
		{
			name: "negative rent",
			body: map[string]any{"unit_id": unit.ID, "start_date": "2026-01-01", "rent_cents": -1},
			want: http.StatusUnprocessableEntity,
		},
		{
			name: "a unit that does not exist",
			body: map[string]any{"unit_id": 9999, "start_date": "2026-01-01", "rent_cents": 100000},
			want: http.StatusUnprocessableEntity,
		},
		{
			name: "another property's unit",
			body: map[string]any{"unit_id": other.Units[0].ID, "start_date": "2026-01-01",
				"rent_cents": 100000},
			want:     http.StatusUnprocessableEntity,
			wantText: "another property",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := a.do(http.MethodPost, propertyPath(p.ID)+"/leases", tt.body)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.want, rec.Body)
			}
			if tt.wantText != "" && !strings.Contains(rec.Body.String(), tt.wantText) {
				t.Errorf("the refusal does not mention %q: %s", tt.wantText, rec.Body)
			}
		})
	}
}

func TestLeaseCarriesItsTenants(t *testing.T) {
	a := newAPI(t)
	p, unit, _ := duplex(a)
	lease := a.newLease(p.ID, map[string]any{
		"unit_id": unit.ID, "start_date": "2026-01-01", "rent_cents": 145000,
	})

	dana := a.newTenant(map[string]any{"name": "Dana Reyes", "email": "dana@example.com"})
	sam := a.newTenant(map[string]any{"name": "Sam Okafor"})

	for _, add := range []map[string]any{
		{"tenant_id": dana.ID, "role": "primary"},
		{"tenant_id": sam.ID, "role": "cosigner"},
	} {
		if rec := a.do(http.MethodPost, leasePath(lease.ID, "/tenants"), add); rec.Code != http.StatusNoContent {
			t.Fatalf("add tenant = %d (body %s)", rec.Code, rec.Body)
		}
	}

	var full leaseResponse
	a.decode(a.do(http.MethodGet, leasePath(lease.ID, ""), nil), http.StatusOK, &full)
	if len(full.Tenants) != 2 {
		t.Fatalf("%d tenants, want 2", len(full.Tenants))
	}

	// Adding the same person twice is a mistake worth naming.
	again := a.do(http.MethodPost, leasePath(lease.ID, "/tenants"),
		map[string]any{"tenant_id": dana.ID, "role": "occupant"})
	if again.Code != http.StatusConflict {
		t.Errorf("adding a tenant twice = %d, want 409", again.Code)
	}

	// And a role outside the CHECK is refused before it reaches the database.
	bad := a.do(http.MethodPost, leasePath(lease.ID, "/tenants"),
		map[string]any{"tenant_id": sam.ID, "role": "landlord"})
	if bad.Code != http.StatusUnprocessableEntity {
		t.Errorf("an unknown role = %d, want 422", bad.Code)
	}

	if rec := a.do(http.MethodDelete, leasePath(lease.ID, "/tenants"),
		map[string]any{"tenant_id": sam.ID}); rec.Code != http.StatusNoContent {
		t.Errorf("remove tenant = %d, want 204", rec.Code)
	}
}

func TestDeletingATenantKeepsTheTenancy(t *testing.T) {
	// The tenancy happened. Its dates and rent are the record of what
	// happened, whoever has since been removed from the contact list.
	a := newAPI(t)
	p, unit, _ := duplex(a)
	lease := a.newLease(p.ID, map[string]any{
		"unit_id": unit.ID, "start_date": "2026-01-01", "rent_cents": 145000,
	})
	dana := a.newTenant(map[string]any{"name": "Dana Reyes"})
	a.decode(a.do(http.MethodPost, leasePath(lease.ID, "/tenants"),
		map[string]any{"tenant_id": dana.ID}), http.StatusNoContent, nil)

	if rec := a.do(http.MethodDelete, "/api/v1/tenants/"+itoa(dana.ID), nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete tenant = %d, want 204", rec.Code)
	}

	var full leaseResponse
	a.decode(a.do(http.MethodGet, leasePath(lease.ID, ""), nil), http.StatusOK, &full)
	if len(full.Tenants) != 0 {
		t.Errorf("%d tenants survived, want 0", len(full.Tenants))
	}
	if full.RentCents != domain.Money(145000) {
		t.Errorf("RentCents = %d, want the lease untouched", full.RentCents)
	}
}

func TestDeletingAUnitTakesItsLeases(t *testing.T) {
	a := newAPI(t)
	p, unit, _ := duplex(a)
	lease := a.newLease(p.ID, map[string]any{
		"unit_id": unit.ID, "start_date": "2026-01-01", "rent_cents": 145000,
	})

	if rec := a.do(http.MethodDelete, "/api/v1/units/"+itoa(unit.ID), nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete unit = %d, want 204 (body %s)", rec.Code, rec.Body)
	}
	if rec := a.do(http.MethodGet, leasePath(lease.ID, ""), nil); rec.Code != http.StatusNotFound {
		t.Errorf("the lease survived its unit: %d", rec.Code)
	}
}
