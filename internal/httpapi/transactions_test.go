package httpapi

import (
	"net/http"
	"testing"

	"github.com/farrellm/rental-bot/internal/domain"
)

// newEntry posts one ledger entry and returns it.
func (a *api) newEntry(propertyID int64, body map[string]any) transactionResponse {
	a.t.Helper()
	var out transactionResponse
	a.decode(a.do(http.MethodPost, propertyPath(propertyID)+"/transactions", body),
		http.StatusCreated, &out)
	return out
}

func (a *api) ledger(propertyID int64, query string) transactionList {
	a.t.Helper()
	var out transactionList
	a.decode(a.do(http.MethodGet, propertyPath(propertyID)+"/transactions"+query,
		nil), http.StatusOK, &out)
	return out
}

func transactionPath(id int64) string { return "/api/v1/transactions/" + itoa(id) }

func TestLedgerKeepsMoneyAsSignedCents(t *testing.T) {
	// Income positive, expense negative, integers throughout. A float anywhere
	// on this path is how 1450.00 becomes 1449.99.
	a := newAPI(t)
	p := a.newProperty(elmStreet())

	income := a.newEntry(p.ID, map[string]any{
		"occurred_on": "2026-07-01", "amount_cents": 145000,
		"category": "rent_income", "description": "Rent, Main",
	})
	expense := a.newEntry(p.ID, map[string]any{
		"occurred_on": "2026-07-03", "amount_cents": -28500,
		"category": "repair", "counterparty": "Ace Plumbing",
	})

	if income.AmountCents != domain.Money(145000) {
		t.Errorf("income = %d, want 145000", income.AmountCents)
	}
	if expense.AmountCents != domain.Money(-28500) {
		t.Errorf("expense = %d, want -28500", expense.AmountCents)
	}
	// Everything typed here was typed by a person; only M4 writes 'email'.
	if income.Source != "manual" {
		t.Errorf("Source = %q, want manual", income.Source)
	}
	if income.NeedsReview {
		t.Error("a hand-entered row arrived flagged for review")
	}
}

func TestLedgerTotalsDescribeTheFilteredRows(t *testing.T) {
	// The foot of the sheet is the server's arithmetic over exactly the rows
	// the filter selected -- including the ones past the end of the page the
	// client is holding.
	a := newAPI(t)
	p := a.newProperty(elmStreet())

	for _, e := range []struct {
		on       string
		cents    int
		category string
	}{
		{"2026-06-28", 145000, "rent_income"},
		{"2026-07-01", 145000, "rent_income"},
		{"2026-07-03", -28500, "repair"},
		{"2026-07-09", -112000, "insurance"},
		{"2026-08-02", -50000, "property_tax"},
	} {
		a.newEntry(p.ID, map[string]any{
			"occurred_on": e.on, "amount_cents": e.cents, "category": e.category,
		})
	}

	tests := []struct {
		name        string
		query       string
		wantCount   int64
		wantIncome  domain.Money
		wantExpense domain.Money
	}{
		{name: "everything", query: "", wantCount: 5,
			wantIncome: 290000, wantExpense: -190500},
		{name: "one month", query: "?from=2026-07-01&to=2026-07-31", wantCount: 3,
			wantIncome: 145000, wantExpense: -140500},
		{name: "one category", query: "?category=repair", wantCount: 1,
			wantIncome: 0, wantExpense: -28500},
		{name: "a range with nothing in it", query: "?from=2027-01-01&to=2027-12-31", wantCount: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := a.ledger(p.ID, tt.query)

			if got.Totals.EntryCount != tt.wantCount {
				t.Errorf("EntryCount = %d, want %d", got.Totals.EntryCount, tt.wantCount)
			}
			if got.Totals.IncomeCents != tt.wantIncome {
				t.Errorf("IncomeCents = %d, want %d", got.Totals.IncomeCents, tt.wantIncome)
			}
			if got.Totals.ExpenseCents != tt.wantExpense {
				t.Errorf("ExpenseCents = %d, want %d", got.Totals.ExpenseCents, tt.wantExpense)
			}
			// Net is the sum of the same column, so it cannot disagree.
			if got.Totals.NetCents != got.Totals.IncomeCents+got.Totals.ExpenseCents {
				t.Errorf("NetCents = %d, want %d",
					got.Totals.NetCents, got.Totals.IncomeCents+got.Totals.ExpenseCents)
			}
			if int64(len(got.Items)) != tt.wantCount {
				t.Errorf("%d rows on the page, want %d", len(got.Items), tt.wantCount)
			}
		})
	}
}

func TestLedgerTotalsCoverRowsPastTheEndOfThePage(t *testing.T) {
	// The reason the totals are a second query and not a sum of the page: a
	// foot that only added up what fitted would be quietly wrong.
	a := newAPI(t)
	p := a.newProperty(elmStreet())

	for i := 1; i <= 5; i++ {
		a.newEntry(p.ID, map[string]any{
			"occurred_on":  "2026-07-0" + itoa(int64(i)),
			"amount_cents": 10000, "category": "rent_income",
		})
	}

	page := a.ledger(p.ID, "?limit=2")
	if len(page.Items) != 2 {
		t.Fatalf("%d rows on the page, want 2", len(page.Items))
	}
	if page.NextCursor == "" {
		t.Error("no cursor, so the remaining entries are unreachable")
	}
	if page.Totals.EntryCount != 5 || page.Totals.NetCents != domain.Money(50000) {
		t.Errorf("totals = %+v, want all five entries", page.Totals)
	}
}

func TestLedgerPagesNewestFirstAcrossADay(t *testing.T) {
	// Several entries share a date, so the cursor has to break the tie on id
	// or the page boundary either repeats a row or skips one.
	a := newAPI(t)
	p := a.newProperty(elmStreet())

	var made []int64
	for range 4 {
		made = append(made, a.newEntry(p.ID, map[string]any{
			"occurred_on": "2026-07-01", "amount_cents": 1000, "category": "other",
		}).ID)
	}

	first := a.ledger(p.ID, "?limit=2")
	if len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("first page = %+v", first)
	}
	rest := a.ledger(p.ID, "?limit=10&cursor="+first.NextCursor)

	seen := map[int64]bool{}
	for _, item := range append(first.Items, rest.Items...) {
		if seen[item.ID] {
			t.Errorf("entry %d appeared on both pages", item.ID)
		}
		seen[item.ID] = true
	}
	if len(seen) != len(made) {
		t.Errorf("%d distinct entries across the pages, want %d", len(seen), len(made))
	}
}

func TestLedgerRejectsWhatItCannotRead(t *testing.T) {
	a := newAPI(t)
	p := a.newProperty(elmStreet())

	tests := []struct {
		name string
		body map[string]any
		want int
	}{
		{
			name: "a date that is not a date",
			body: map[string]any{"occurred_on": "July 1st", "amount_cents": 1000},
			want: http.StatusUnprocessableEntity,
		},
		{
			name: "a date that does not exist",
			body: map[string]any{"occurred_on": "2026-02-30", "amount_cents": 1000},
			want: http.StatusUnprocessableEntity,
		},
		{
			name: "a category outside the CHECK",
			body: map[string]any{"occurred_on": "2026-07-01", "amount_cents": 1000, "category": "bribes"},
			want: http.StatusUnprocessableEntity,
		},
		{
			name: "money as a decimal string",
			body: map[string]any{"occurred_on": "2026-07-01", "amount_cents": "1450.00"},
			want: http.StatusBadRequest,
		},
		{
			name: "a vendor that does not exist",
			body: map[string]any{"occurred_on": "2026-07-01", "amount_cents": 1000, "vendor_id": 9999},
			want: http.StatusUnprocessableEntity,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := a.do(http.MethodPost, propertyPath(p.ID)+"/transactions", tt.body)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

func TestLedgerAcceptsAZeroEntry(t *testing.T) {
	// A waived fee is a real thing to record, and refusing it would be the
	// application deciding what happened.
	a := newAPI(t)
	p := a.newProperty(elmStreet())

	entry := a.newEntry(p.ID, map[string]any{
		"occurred_on": "2026-07-01", "amount_cents": 0,
		"category": "other", "description": "Late fee waived",
	})
	if entry.AmountCents != 0 {
		t.Errorf("AmountCents = %d, want 0", entry.AmountCents)
	}
}

func TestPatchLedgerEntryLeavesProvenanceAlone(t *testing.T) {
	// How a row arrived is a fact about the row, not a field the operator
	// edits. M4 depends on that: an auto-applied entry has to stay traceable
	// to the email it came from.
	a := newAPI(t)
	p := a.newProperty(elmStreet())
	entry := a.newEntry(p.ID, map[string]any{
		"occurred_on": "2026-07-03", "amount_cents": -28500, "category": "repair",
	})

	var updated transactionResponse
	a.decode(a.do(http.MethodPatch, transactionPath(entry.ID), map[string]any{
		"amount_cents": -31000,
		"description":  "Ace Plumbing, kitchen tap",
	}), http.StatusOK, &updated)

	if updated.AmountCents != domain.Money(-31000) {
		t.Errorf("AmountCents = %d, want -31000", updated.AmountCents)
	}
	if updated.Source != "manual" {
		t.Errorf("Source = %q", updated.Source)
	}
	// The date was not in the patch, so it is untouched.
	if updated.OccurredOn != "2026-07-03" {
		t.Errorf("OccurredOn = %q, want the original", updated.OccurredOn)
	}

	for _, field := range []string{"source", "property_id", "confidence"} {
		rec := a.do(http.MethodPatch, transactionPath(entry.ID), map[string]any{field: "x"})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("patching %s = %d, want 400", field, rec.Code)
		}
	}
}

func TestPatchLedgerEntryClearsALinkWithNull(t *testing.T) {
	// The three PATCH states, on a ledger entry: absent leaves the link alone,
	// null clears it, a value sets it.
	a := newAPI(t)
	p := a.newProperty(elmStreet())
	vendor := a.newVendor(map[string]any{"name": "Ace Plumbing"})

	entry := a.newEntry(p.ID, map[string]any{
		"occurred_on": "2026-07-03", "amount_cents": -28500,
		"category": "repair", "vendor_id": vendor.ID,
	})
	if entry.VendorID == nil || *entry.VendorID != vendor.ID {
		t.Fatalf("VendorID = %v, want %d", entry.VendorID, vendor.ID)
	}

	var untouched transactionResponse
	a.decode(a.do(http.MethodPatch, transactionPath(entry.ID),
		map[string]any{"description": "still the plumber"}), http.StatusOK, &untouched)
	if untouched.VendorID == nil {
		t.Error("an absent vendor_id cleared the link")
	}

	var cleared transactionResponse
	a.decode(a.do(http.MethodPatch, transactionPath(entry.ID),
		map[string]any{"vendor_id": nil}), http.StatusOK, &cleared)
	if cleared.VendorID != nil {
		t.Errorf("VendorID = %v, want nil after an explicit null", *cleared.VendorID)
	}
}

func TestDeletingALedgerEntryRemovesIt(t *testing.T) {
	a := newAPI(t)
	p := a.newProperty(elmStreet())
	entry := a.newEntry(p.ID, map[string]any{
		"occurred_on": "2026-07-01", "amount_cents": 1000, "category": "other",
	})

	if rec := a.do(http.MethodDelete, transactionPath(entry.ID), nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204 (body %s)", rec.Code, rec.Body)
	}
	if rec := a.do(http.MethodDelete, transactionPath(entry.ID), nil); rec.Code != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", rec.Code)
	}
	if got := a.ledger(p.ID, ""); got.Totals.EntryCount != 0 {
		t.Errorf("totals still count %d entries", got.Totals.EntryCount)
	}
}

func TestLedgerIsScopedToItsProperty(t *testing.T) {
	a := newAPI(t)
	mine := a.newProperty(elmStreet())
	theirs := a.newProperty(map[string]any{"nickname": "Oak Street House", "address_line1": "9 Oak St"})

	a.newEntry(mine.ID, map[string]any{
		"occurred_on": "2026-07-01", "amount_cents": 145000, "category": "rent_income",
	})
	a.newEntry(theirs.ID, map[string]any{
		"occurred_on": "2026-07-01", "amount_cents": 99900, "category": "rent_income",
	})

	got := a.ledger(mine.ID, "")
	if got.Totals.EntryCount != 1 || got.Totals.NetCents != domain.Money(145000) {
		t.Errorf("totals = %+v, want only this property's entry", got.Totals)
	}
}

func TestLedgerRejectsABadFilter(t *testing.T) {
	a := newAPI(t)
	p := a.newProperty(elmStreet())

	for _, query := range []string{"?from=last-july", "?to=2026-13-01", "?category=bribes"} {
		rec := a.do(http.MethodGet, propertyPath(p.ID)+"/transactions"+query, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400 (body %s)", query, rec.Code, rec.Body)
		}
	}
}
