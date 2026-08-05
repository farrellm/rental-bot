package httpapi

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/farrellm/rental-bot/internal/domain"
)

func (a *api) newRepair(propertyID int64, body map[string]any) repairResponse {
	a.t.Helper()
	var out repairResponse
	a.decode(a.do(http.MethodPost, propertyPath(propertyID)+"/repairs", body),
		http.StatusCreated, &out)
	return out
}

func repairPath(id int64, suffix string) string {
	return "/api/v1/repairs/" + itoa(id) + suffix
}

func todayUTC() string { return time.Now().UTC().Format(time.DateOnly) }

func TestRepairRoundTrips(t *testing.T) {
	a := newAPI(t)
	p := a.newProperty(elmStreet())
	estimate := 45000

	repair := a.newRepair(p.ID, map[string]any{
		"opened_on":      "2026-07-01",
		"description":    "Kitchen tap drips",
		"category":       "plumbing",
		"estimate_cents": estimate,
		"is_capex":       false,
	})

	if repair.Status != "open" {
		t.Errorf("Status = %q, want open", repair.Status)
	}
	if repair.ClosedOn != nil {
		t.Errorf("ClosedOn = %v on a new repair, want nil", *repair.ClosedOn)
	}
	if repair.EstimateCents == nil || *repair.EstimateCents != domain.Money(45000) {
		t.Errorf("EstimateCents = %v, want 45000", repair.EstimateCents)
	}
	if repair.ActualCents != nil {
		t.Error("a repair with no recorded cost came back with one; unknown is not zero")
	}
	if repair.IsCapex {
		t.Error("IsCapex = true, want false")
	}
}

func TestClosingARepairDatesItAndReopeningClearsIt(t *testing.T) {
	// closed_on and status cannot tell different stories: a job that is done
	// has a date, and a job that is open does not.
	a := newAPI(t)
	p := a.newProperty(elmStreet())
	repair := a.newRepair(p.ID, map[string]any{
		"opened_on": "2026-07-01", "description": "Kitchen tap drips",
	})

	var closed repairResponse
	a.decode(a.do(http.MethodPatch, repairPath(repair.ID, ""),
		map[string]any{"status": "done", "actual_cents": -28500}), http.StatusOK, &closed)

	if closed.ClosedOn == nil {
		t.Fatal("a repair marked done carries no closing date")
	}
	if *closed.ClosedOn != todayUTC() {
		t.Errorf("ClosedOn = %q, want today (%s)", *closed.ClosedOn, todayUTC())
	}

	var reopened repairResponse
	a.decode(a.do(http.MethodPatch, repairPath(repair.ID, ""),
		map[string]any{"status": "in_progress"}), http.StatusOK, &reopened)

	if reopened.ClosedOn != nil {
		t.Errorf("ClosedOn = %v after reopening, want nil", *reopened.ClosedOn)
	}
}

func TestAnExplicitClosingDateWins(t *testing.T) {
	// The repair was finished last Tuesday and recorded today. What the
	// operator says happened beats what the clock says.
	a := newAPI(t)
	p := a.newProperty(elmStreet())
	repair := a.newRepair(p.ID, map[string]any{
		"opened_on": "2026-07-01", "description": "Kitchen tap drips",
	})

	var closed repairResponse
	a.decode(a.do(http.MethodPatch, repairPath(repair.ID, ""),
		map[string]any{"status": "done", "closed_on": "2026-07-08"}), http.StatusOK, &closed)

	if closed.ClosedOn == nil || *closed.ClosedOn != "2026-07-08" {
		t.Errorf("ClosedOn = %v, want 2026-07-08", closed.ClosedOn)
	}
}

func TestRepairRejectsWhatItCannotRead(t *testing.T) {
	a := newAPI(t)
	p := a.newProperty(elmStreet())

	tests := []struct {
		name     string
		body     map[string]any
		want     int
		wantText string
	}{
		{
			name: "no description",
			body: map[string]any{"opened_on": "2026-07-01"},
			want: http.StatusUnprocessableEntity,
		},
		{
			name: "a status outside the CHECK",
			body: map[string]any{"opened_on": "2026-07-01", "description": "x", "status": "pending"},
			want: http.StatusUnprocessableEntity,
		},
		{
			name:     "closed before it opened",
			body:     map[string]any{"opened_on": "2026-07-01", "description": "x", "status": "done", "closed_on": "2026-06-01"},
			want:     http.StatusUnprocessableEntity,
			wantText: "before it opened",
		},
		{
			name: "a vendor that does not exist",
			body: map[string]any{"opened_on": "2026-07-01", "description": "x", "vendor_id": 9999},
			want: http.StatusUnprocessableEntity,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := a.do(http.MethodPost, propertyPath(p.ID)+"/repairs", tt.body)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.want, rec.Body)
			}
			if tt.wantText != "" && !strings.Contains(rec.Body.String(), tt.wantText) {
				t.Errorf("the refusal does not mention %q: %s", tt.wantText, rec.Body)
			}
		})
	}
}

func TestRepairTimelineReadsInOrder(t *testing.T) {
	// The timeline is a sequence, and it is ordered by when things happened
	// rather than by when someone got round to recording them.
	a := newAPI(t)
	p := a.newProperty(elmStreet())
	repair := a.newRepair(p.ID, map[string]any{
		"opened_on": "2026-07-01", "description": "Kitchen tap drips",
	})

	// Recorded out of order on purpose.
	for _, e := range []map[string]any{
		{"at": "2026-07-05T12:00:00Z", "note": "Ace Plumbing replaced the cartridge"},
		{"at": "2026-07-02T09:00:00Z", "note": "Quoted 285.00"},
		{"at": "2026-07-09T16:30:00Z", "note": "Paid"},
	} {
		rec := a.do(http.MethodPost, repairPath(repair.ID, "/events"), e)
		if rec.Code != http.StatusCreated {
			t.Fatalf("event = %d (body %s)", rec.Code, rec.Body)
		}
	}

	var full repairResponse
	a.decode(a.do(http.MethodGet, repairPath(repair.ID, ""), nil), http.StatusOK, &full)

	if len(full.Events) != 3 {
		t.Fatalf("%d events, want 3", len(full.Events))
	}
	want := []string{"Quoted 285.00", "Ace Plumbing replaced the cartridge", "Paid"}
	for i, note := range want {
		if full.Events[i].Note != note {
			t.Errorf("event %d = %q, want %q", i, full.Events[i].Note, note)
		}
	}
}

func TestRepairEventNeedsToSayWhatHappened(t *testing.T) {
	a := newAPI(t)
	p := a.newProperty(elmStreet())
	repair := a.newRepair(p.ID, map[string]any{
		"opened_on": "2026-07-01", "description": "Kitchen tap drips",
	})

	tests := []struct {
		name string
		body map[string]any
	}{
		{name: "no note", body: map[string]any{"at": "2026-07-02T09:00:00Z"}},
		{name: "a blank note", body: map[string]any{"note": "   "}},
		{name: "a date rather than a timestamp", body: map[string]any{"at": "2026-07-02", "note": "Quoted"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := a.do(http.MethodPost, repairPath(repair.ID, "/events"), tt.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("status = %d, want 422 (body %s)", rec.Code, rec.Body)
			}
		})
	}
}

func TestDeletingARepairTakesItsTimeline(t *testing.T) {
	a := newAPI(t)
	p := a.newProperty(elmStreet())
	repair := a.newRepair(p.ID, map[string]any{
		"opened_on": "2026-07-01", "description": "Kitchen tap drips",
	})

	var event repairEventResponse
	a.decode(a.do(http.MethodPost, repairPath(repair.ID, "/events"),
		map[string]any{"note": "Quoted 285.00"}), http.StatusCreated, &event)

	if rec := a.do(http.MethodDelete, repairPath(repair.ID, ""), nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204 (body %s)", rec.Code, rec.Body)
	}
	if rec := a.do(http.MethodGet, repairPath(repair.ID, ""), nil); rec.Code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", rec.Code)
	}
	// The event cascaded, so removing it again finds nothing.
	rec := a.do(http.MethodDelete, "/api/v1/repair-events/"+itoa(event.ID), nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("the event survived its repair: %d", rec.Code)
	}
}

func TestListRepairsFiltersByStatus(t *testing.T) {
	// The docket shows the open work without reading years of closed jobs.
	a := newAPI(t)
	p := a.newProperty(elmStreet())

	open := a.newRepair(p.ID, map[string]any{"opened_on": "2026-07-01", "description": "Tap drips"})
	done := a.newRepair(p.ID, map[string]any{
		"opened_on": "2026-06-01", "description": "Gutter cleared", "status": "done",
	})
	if done.ClosedOn == nil {
		t.Error("a repair created as done carries no closing date")
	}

	var list repairList
	a.decode(a.do(http.MethodGet, propertyPath(p.ID)+"/repairs?status=open", nil), http.StatusOK, &list)
	if len(list.Items) != 1 || list.Items[0].ID != open.ID {
		t.Errorf("filtered list = %+v, want only the open repair", list.Items)
	}

	var all repairList
	a.decode(a.do(http.MethodGet, propertyPath(p.ID)+"/repairs", nil), http.StatusOK, &all)
	if len(all.Items) != 2 {
		t.Errorf("%d repairs unfiltered, want 2", len(all.Items))
	}

	rec := a.do(http.MethodGet, propertyPath(p.ID)+"/repairs?status=abandoned", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("an unknown status filter = %d, want 400", rec.Code)
	}
}
