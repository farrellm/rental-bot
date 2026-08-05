package store

import (
	"testing"
	"time"

	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// today is the calendar date the derived-occupancy queries are asked about.
// Dates in this schema are written, never reinterpreted through a timezone.
func today() string { return time.Now().UTC().Format(time.DateOnly) }

func daysFromToday(n int) string {
	return time.Now().UTC().AddDate(0, 0, n).Format(time.DateOnly)
}

func newUnit(t *testing.T, repo *Repo, propertyID int64, label string) sqlc.Unit {
	t.Helper()
	u, err := repo.Write().CreateUnit(t.Context(), sqlc.CreateUnitParams{
		PropertyID: propertyID, Label: label, CreatedAt: now(), UpdatedAt: now(),
	})
	if err != nil {
		t.Fatalf("CreateUnit(%q): %v", label, err)
	}
	return u
}

func newEntry(t *testing.T, repo *Repo, propertyID int64, on string, cents domain.Money, category string) sqlc.Transaction {
	t.Helper()
	tx, err := repo.Write().CreateTransaction(t.Context(), sqlc.CreateTransactionParams{
		PropertyID: propertyID, OccurredOn: on, AmountCents: cents, Category: category,
		Source: "manual", CreatedAt: now(), UpdatedAt: now(),
	})
	if err != nil {
		t.Fatalf("CreateTransaction: %v", err)
	}
	return tx
}

func TestLedgerKeepsTheSignThatSeparatesIncomeFromExpense(t *testing.T) {
	// The sign is the only thing distinguishing the two, so it has to survive
	// the round trip exactly: an expense that came back positive would be
	// counted as rent.
	repo := openRepo(t)
	p := newProperty(t, repo, "Elm Street Duplex")

	newEntry(t, repo, p.ID, "2026-07-01", domain.Money(145000), "rent_income")
	newEntry(t, repo, p.ID, "2026-07-03", domain.Money(-28500), "repair")

	rows, err := repo.Read().ListTransactionsFirstPage(t.Context(), sqlc.ListTransactionsFirstPageParams{
		PropertyID: p.ID, PageSize: 10,
	})
	if err != nil {
		t.Fatalf("ListTransactionsFirstPage: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d entries, want 2", len(rows))
	}
	// Newest first.
	if rows[0].OccurredOn != "2026-07-03" {
		t.Errorf("first entry is %s, want the newest (2026-07-03)", rows[0].OccurredOn)
	}
	if rows[0].AmountCents != domain.Money(-28500) {
		t.Errorf("expense = %d, want -28500", rows[0].AmountCents)
	}
	if rows[1].AmountCents != domain.Money(145000) {
		t.Errorf("income = %d, want 145000", rows[1].AmountCents)
	}
}

func TestLedgerTotalsDescribeTheSameRowsAsThePage(t *testing.T) {
	// The foot of the ledger sheet has to be arithmetic over exactly the rows
	// above it. A total that ignored the filter would be a number the operator
	// cannot check against what they are looking at.
	repo := openRepo(t)
	p := newProperty(t, repo, "Elm Street Duplex")

	newEntry(t, repo, p.ID, "2026-06-28", domain.Money(145000), "rent_income")  // outside
	newEntry(t, repo, p.ID, "2026-07-01", domain.Money(145000), "rent_income")  // inside
	newEntry(t, repo, p.ID, "2026-07-03", domain.Money(-28500), "repair")       // inside
	newEntry(t, repo, p.ID, "2026-07-09", domain.Money(-112000), "insurance")   // inside
	newEntry(t, repo, p.ID, "2026-08-02", domain.Money(-50000), "property_tax") // outside

	from, to := "2026-07-01", "2026-07-31"
	totals, err := repo.Read().SumTransactions(t.Context(), sqlc.SumTransactionsParams{
		PropertyID: p.ID, FromDate: &from, ToDate: &to,
	})
	if err != nil {
		t.Fatalf("SumTransactions: %v", err)
	}

	if totals.EntryCount != 3 {
		t.Errorf("EntryCount = %d, want 3", totals.EntryCount)
	}
	if totals.IncomeCents != 145000 {
		t.Errorf("IncomeCents = %d, want 145000", totals.IncomeCents)
	}
	if totals.ExpenseCents != -140500 {
		t.Errorf("ExpenseCents = %d, want -140500", totals.ExpenseCents)
	}
	// Net is the sum of the same column, so it can never disagree with the two.
	if totals.NetCents != totals.IncomeCents+totals.ExpenseCents {
		t.Errorf("NetCents = %d, want %d", totals.NetCents, totals.IncomeCents+totals.ExpenseCents)
	}

	category := "repair"
	filtered, err := repo.Read().SumTransactions(t.Context(), sqlc.SumTransactionsParams{
		PropertyID: p.ID, Category: &category,
	})
	if err != nil {
		t.Fatalf("SumTransactions(category): %v", err)
	}
	if filtered.EntryCount != 1 || filtered.NetCents != -28500 {
		t.Errorf("category filter = %+v, want one entry of -28500", filtered)
	}
}

func TestLedgerTotalsOfNothingAreZeroRatherThanNull(t *testing.T) {
	// SUM over no rows is NULL in SQLite. Without the COALESCE the scan fails
	// and a property with an empty ledger cannot be opened at all.
	repo := openRepo(t)
	p := newProperty(t, repo, "Empty")

	totals, err := repo.Read().SumTransactions(t.Context(), sqlc.SumTransactionsParams{PropertyID: p.ID})
	if err != nil {
		t.Fatalf("SumTransactions: %v", err)
	}
	if totals.EntryCount != 0 || totals.NetCents != 0 {
		t.Errorf("empty ledger totals = %+v, want zeroes", totals)
	}
}

func TestOccupancyIsDerivedFromTheLeaseDates(t *testing.T) {
	// There is no is_occupied column. A unit is occupied because a lease says
	// so, and the answer changes on its own as the dates pass.
	repo := openRepo(t)
	p := newProperty(t, repo, "Elm Street Duplex")
	let := newUnit(t, repo, p.ID, "Apt 1")
	newUnit(t, repo, p.ID, "Apt 2") // never let
	expired := newUnit(t, repo, p.ID, "Apt 3")

	ends := daysFromToday(120)
	current, err := repo.Write().CreateLease(t.Context(), sqlc.CreateLeaseParams{
		UnitID: let.ID, StartDate: daysFromToday(-240), EndDate: &ends,
		RentCents: domain.Money(145000), Status: "active",
		CreatedAt: now(), UpdatedAt: now(),
	})
	if err != nil {
		t.Fatalf("CreateLease: %v", err)
	}

	lapsed := daysFromToday(-10)
	if _, err := repo.Write().CreateLease(t.Context(), sqlc.CreateLeaseParams{
		UnitID: expired.ID, StartDate: daysFromToday(-380), EndDate: &lapsed,
		RentCents: domain.Money(120000), Status: "active",
		CreatedAt: now(), UpdatedAt: now(),
	}); err != nil {
		t.Fatalf("CreateLease(expired): %v", err)
	}

	rows, err := repo.Read().ListUnitsWithOccupancy(t.Context(), sqlc.ListUnitsWithOccupancyParams{
		PropertyID: p.ID, Today: today(),
	})
	if err != nil {
		t.Fatalf("ListUnitsWithOccupancy: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d units, want 3", len(rows))
	}

	occupied := map[string]*int64{}
	for _, r := range rows {
		occupied[r.Unit.Label] = r.ActiveLeaseID
	}
	if got := occupied["Apt 1"]; got == nil || *got != current.ID {
		t.Errorf("Apt 1 active lease = %v, want %d", got, current.ID)
	}
	if occupied["Apt 2"] != nil {
		t.Errorf("Apt 2 reports a lease it does not have: %v", *occupied["Apt 2"])
	}
	// The lease ended ten days ago. Nothing was updated to say so.
	if occupied["Apt 3"] != nil {
		t.Errorf("Apt 3 still reports a lapsed lease: %v", *occupied["Apt 3"])
	}
}

func TestOccupancyCountsAnOpenEndedLease(t *testing.T) {
	// A null end_date is a month-to-month tenancy, not a missing value, and it
	// covers today for as long as nobody ends it.
	repo := openRepo(t)
	p := newProperty(t, repo, "Elm Street Duplex")
	unit := newUnit(t, repo, p.ID, "Main")

	if _, err := repo.Write().CreateLease(t.Context(), sqlc.CreateLeaseParams{
		UnitID: unit.ID, StartDate: daysFromToday(-30), EndDate: nil,
		RentCents: domain.Money(95000), Status: "active",
		CreatedAt: now(), UpdatedAt: now(),
	}); err != nil {
		t.Fatalf("CreateLease: %v", err)
	}

	rows, err := repo.Read().ListUnitsWithOccupancy(t.Context(), sqlc.ListUnitsWithOccupancyParams{
		PropertyID: p.ID, Today: today(),
	})
	if err != nil {
		t.Fatalf("ListUnitsWithOccupancy: %v", err)
	}
	if len(rows) != 1 || rows[0].ActiveLeaseID == nil {
		t.Fatalf("an open-ended lease left the unit reading as vacant: %+v", rows)
	}
	if rows[0].ActiveLeaseEndDate != nil {
		t.Errorf("end date = %v, want nil", *rows[0].ActiveLeaseEndDate)
	}
}

func TestOverlappingLeasesAreCountable(t *testing.T) {
	// Two live leases on one unit make occupancy ambiguous, so the write path
	// has to be able to see the collision before it commits one.
	repo := openRepo(t)
	p := newProperty(t, repo, "Elm Street Duplex")
	unit := newUnit(t, repo, p.ID, "Main")

	ends := "2026-12-31"
	held, err := repo.Write().CreateLease(t.Context(), sqlc.CreateLeaseParams{
		UnitID: unit.ID, StartDate: "2026-01-01", EndDate: &ends,
		RentCents: domain.Money(145000), Status: "active",
		CreatedAt: now(), UpdatedAt: now(),
	})
	if err != nil {
		t.Fatalf("CreateLease: %v", err)
	}

	tests := []struct {
		name      string
		start     string
		end       *string
		exclude   int64
		wantCount int64
	}{
		{name: "starts inside the term", start: "2026-06-01", end: strptr("2027-05-31"), wantCount: 1},
		{name: "ends inside the term", start: "2025-06-01", end: strptr("2026-02-01"), wantCount: 1},
		{name: "swallows the term", start: "2025-01-01", end: nil, wantCount: 1},
		{name: "starts the day after", start: "2027-01-01", end: strptr("2027-12-31"), wantCount: 0},
		{name: "ends the day before", start: "2025-01-01", end: strptr("2025-12-31"), wantCount: 0},
		{name: "the lease itself, being amended", start: "2026-01-01", end: &ends, exclude: held.ID, wantCount: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := repo.Read().CountOverlappingLeases(t.Context(), sqlc.CountOverlappingLeasesParams{
				UnitID: unit.ID, ExcludeID: tt.exclude, StartDate: &tt.start, EndDate: tt.end,
			})
			if err != nil {
				t.Fatalf("CountOverlappingLeases: %v", err)
			}
			if got != tt.wantCount {
				t.Errorf("overlap count = %d, want %d", got, tt.wantCount)
			}
		})
	}
}

func TestDocumentsAreIdentifiedByTheirHash(t *testing.T) {
	// Content addressing is what makes re-forwarding the same PDF a no-op. The
	// UNIQUE constraint is the enforcement; the handler reads the collision as
	// "already on file" rather than as an error.
	repo := openRepo(t)
	p := newProperty(t, repo, "Elm Street Duplex")

	sum := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	first, err := repo.Write().CreateDocument(t.Context(), sqlc.CreateDocumentParams{
		PropertyID: &p.ID, Kind: "receipt", Title: "Ace Plumbing",
		OriginalFilename: "receipt.pdf", Mime: "application/pdf", SizeBytes: 1024,
		Sha256: sum, StoragePath: "e3/b0/" + sum, CreatedAt: now(), UpdatedAt: now(),
	})
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}

	_, err = repo.Write().CreateDocument(t.Context(), sqlc.CreateDocumentParams{
		PropertyID: &p.ID, Kind: "receipt", Title: "The same bytes again",
		OriginalFilename: "forwarded.pdf", Mime: "application/pdf", SizeBytes: 1024,
		Sha256: sum, StoragePath: "e3/b0/" + sum, CreatedAt: now(), UpdatedAt: now(),
	})
	if err == nil {
		t.Fatal("a second document with the same hash was accepted")
	}
	if !Conflict(err) {
		t.Errorf("Conflict(%v) = false, want true", err)
	}

	found, err := repo.Read().GetDocumentBySHA(t.Context(), sum)
	if err != nil {
		t.Fatalf("GetDocumentBySHA: %v", err)
	}
	if found.ID != first.ID {
		t.Errorf("GetDocumentBySHA returned %d, want %d", found.ID, first.ID)
	}
}

func TestDocumentLinksGoWithTheirDocument(t *testing.T) {
	repo := openRepo(t)
	p := newProperty(t, repo, "Elm Street Duplex")

	sum := "aa" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd"
	doc, err := repo.Write().CreateDocument(t.Context(), sqlc.CreateDocumentParams{
		PropertyID: &p.ID, Kind: "lease", Sha256: sum, StoragePath: "aa/01/" + sum,
		Mime: "application/pdf", CreatedAt: now(), UpdatedAt: now(),
	})
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if _, err := repo.Write().CreateDocumentLink(t.Context(), sqlc.CreateDocumentLinkParams{
		DocumentID: doc.ID, EntityType: "property", EntityID: p.ID,
		CreatedAt: now(), UpdatedAt: now(),
	}); err != nil {
		t.Fatalf("CreateDocumentLink: %v", err)
	}

	linked, err := repo.Read().ListDocumentsByEntity(t.Context(), sqlc.ListDocumentsByEntityParams{
		EntityType: "property", EntityID: p.ID,
	})
	if err != nil {
		t.Fatalf("ListDocumentsByEntity: %v", err)
	}
	if len(linked) != 1 || linked[0].ID != doc.ID {
		t.Fatalf("entity lookup returned %+v, want the one document", linked)
	}

	if _, err := repo.Write().DeleteDocument(t.Context(), doc.ID); err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}
	links, err := repo.Read().ListDocumentLinks(t.Context(), doc.ID)
	if err != nil {
		t.Fatalf("ListDocumentLinks: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("%d links survived their document, want 0", len(links))
	}
}

func TestRepairEventsGoWithTheirRepair(t *testing.T) {
	repo := openRepo(t)
	p := newProperty(t, repo, "Elm Street Duplex")

	repair, err := repo.Write().CreateRepair(t.Context(), sqlc.CreateRepairParams{
		PropertyID: p.ID, OpenedOn: "2026-07-01", Status: "open",
		Description: "Kitchen tap drips", CreatedAt: now(), UpdatedAt: now(),
	})
	if err != nil {
		t.Fatalf("CreateRepair: %v", err)
	}

	// Written out of order on purpose: the timeline is ordered by `at`, not by
	// the order someone happened to record things.
	for _, at := range []string{"2026-07-05T12:00:00Z", "2026-07-02T09:00:00Z"} {
		if _, err := repo.Write().CreateRepairEvent(t.Context(), sqlc.CreateRepairEventParams{
			RepairID: repair.ID, At: at, Note: at, CreatedAt: now(), UpdatedAt: now(),
		}); err != nil {
			t.Fatalf("CreateRepairEvent: %v", err)
		}
	}

	events, err := repo.Read().ListRepairEvents(t.Context(), repair.ID)
	if err != nil {
		t.Fatalf("ListRepairEvents: %v", err)
	}
	if len(events) != 2 || events[0].At != "2026-07-02T09:00:00Z" {
		t.Fatalf("timeline = %+v, want oldest first", events)
	}

	if _, err := repo.Write().DeleteRepair(t.Context(), repair.ID); err != nil {
		t.Fatalf("DeleteRepair: %v", err)
	}
	events, err = repo.Read().ListRepairEvents(t.Context(), repair.ID)
	if err != nil {
		t.Fatalf("ListRepairEvents: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("%d events survived their repair, want 0", len(events))
	}
}

func TestTheLedgerSurvivesTheRecordsItPointsAt(t *testing.T) {
	// A vendor going away must not take the money entry with it: the payment
	// happened, and the ledger is a record of what happened.
	repo := openRepo(t)
	p := newProperty(t, repo, "Elm Street Duplex")

	vendor, err := repo.Write().CreateVendor(t.Context(), sqlc.CreateVendorParams{
		Name: "Ace Plumbing", CreatedAt: now(), UpdatedAt: now(),
	})
	if err != nil {
		t.Fatalf("CreateVendor: %v", err)
	}

	entry, err := repo.Write().CreateTransaction(t.Context(), sqlc.CreateTransactionParams{
		PropertyID: p.ID, OccurredOn: "2026-07-03", AmountCents: domain.Money(-28500),
		Category: "repair", VendorID: &vendor.ID, Source: "manual",
		CreatedAt: now(), UpdatedAt: now(),
	})
	if err != nil {
		t.Fatalf("CreateTransaction: %v", err)
	}

	if _, err := repo.Write().DeleteVendor(t.Context(), vendor.ID); err != nil {
		t.Fatalf("DeleteVendor: %v", err)
	}

	got, err := repo.Read().GetTransaction(t.Context(), entry.ID)
	if err != nil {
		t.Fatalf("GetTransaction: %v", err)
	}
	if got.VendorID != nil {
		t.Errorf("VendorID = %v, want nil after the vendor was deleted", *got.VendorID)
	}
	if got.AmountCents != domain.Money(-28500) {
		t.Errorf("AmountCents = %d, want the entry to be untouched", got.AmountCents)
	}
}

func strptr(s string) *string { return &s }
