package httpapi

import (
	"net/http"
	"testing"

	"github.com/farrellm/rental-bot/internal/domain"
)

func (a *api) newVendor(body map[string]any) vendorResponse {
	a.t.Helper()
	var out vendorResponse
	a.decode(a.do(http.MethodPost, "/api/v1/vendors", body), http.StatusCreated, &out)
	return out
}

func vendorPath(id int64) string { return "/api/v1/vendors/" + itoa(id) }

func TestVendorsAreRoundTripped(t *testing.T) {
	a := newAPI(t)

	created := a.newVendor(map[string]any{
		"name": "Ace Plumbing", "trade": "plumber",
		"phone": "740-555-0134", "email": "book@aceplumbing.example",
	})
	if created.Name != "Ace Plumbing" || created.Trade != "plumber" {
		t.Errorf("vendor = %+v", created)
	}

	var list vendorList
	a.decode(a.do(http.MethodGet, "/api/v1/vendors", nil), http.StatusOK, &list)
	if len(list.Items) != 1 || list.Items[0].ID != created.ID {
		t.Errorf("list = %+v, want the one vendor", list.Items)
	}

	var updated vendorResponse
	a.decode(a.do(http.MethodPatch, vendorPath(created.ID),
		map[string]any{"phone": "740-555-0199"}), http.StatusOK, &updated)
	if updated.Phone != "740-555-0199" {
		t.Errorf("Phone = %q", updated.Phone)
	}
	if updated.Name != "Ace Plumbing" {
		t.Errorf("an absent field was overwritten: Name = %q", updated.Name)
	}
}

func TestVendorNeedsAName(t *testing.T) {
	a := newAPI(t)

	rec := a.do(http.MethodPost, "/api/v1/vendors", map[string]any{"trade": "plumber"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", rec.Code, rec.Body)
	}

	vendor := a.newVendor(map[string]any{"name": "Ace Plumbing"})
	blank := a.do(http.MethodPatch, vendorPath(vendor.ID), map[string]any{"name": "   "})
	if blank.Code != http.StatusUnprocessableEntity {
		t.Errorf("blanking a name = %d, want 422", blank.Code)
	}
}

func TestDeletingAVendorKeepsWhatTheyWerePaid(t *testing.T) {
	// The payment happened. A record of what happened does not go away because
	// a contact was tidied up -- the foreign key is ON DELETE SET NULL, and
	// the money and the date stay exactly as entered.
	a := newAPI(t)
	p := a.newProperty(elmStreet())
	vendor := a.newVendor(map[string]any{"name": "Ace Plumbing"})

	entry := a.newEntry(p.ID, map[string]any{
		"occurred_on": "2026-07-03", "amount_cents": -28500,
		"category": "repair", "vendor_id": vendor.ID,
	})

	if rec := a.do(http.MethodDelete, vendorPath(vendor.ID), nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204 (body %s)", rec.Code, rec.Body)
	}

	var after transactionResponse
	a.decode(a.do(http.MethodGet, transactionPath(entry.ID), nil), http.StatusOK, &after)
	if after.VendorID != nil {
		t.Errorf("VendorID = %v, want nil", *after.VendorID)
	}
	if after.AmountCents != domain.Money(-28500) {
		t.Errorf("AmountCents = %d, want the entry untouched", after.AmountCents)
	}
}
