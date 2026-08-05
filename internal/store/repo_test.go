package store

import (
	"errors"
	"testing"
	"time"

	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
	"github.com/farrellm/rental-bot/migrations"
)

// openRepo returns a migrated database and its query layer. The generated code
// is only worth trusting against the real schema, so these tests run the real
// migrations rather than a synthetic fixture.
func openRepo(t *testing.T) *Repo {
	t.Helper()
	db := openTemp(t)
	if _, err := db.Migrate(t.Context(), migrations.FS); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db.Repo()
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func newProperty(t *testing.T, repo *Repo, nickname string) sqlc.Property {
	t.Helper()
	price := domain.Money(28500000)
	beds := int64(3)
	baths := 1.5
	p, err := repo.Write().CreateProperty(t.Context(), sqlc.CreatePropertyParams{
		Nickname:           nickname,
		AddressLine1:       "412 Elm St",
		City:               "Athens",
		State:              "OH",
		PostalCode:         "45701",
		NormalizedAddress:  domain.NormalizeAddress("412 Elm St", "", "Athens", "OH", "45701"),
		PurchasePriceCents: &price,
		Beds:               &beds,
		Baths:              &baths,
		Status:             "active",
		CreatedAt:          now(),
		UpdatedAt:          now(),
	})
	if err != nil {
		t.Fatalf("CreateProperty: %v", err)
	}
	return p
}

func TestRepoRoundTripsAProperty(t *testing.T) {
	repo := openRepo(t)
	created := newProperty(t, repo, "Elm Street Duplex")

	got, err := repo.Read().GetProperty(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("GetProperty: %v", err)
	}

	if got.Nickname != "Elm Street Duplex" {
		t.Errorf("Nickname = %q, want %q", got.Nickname, "Elm Street Duplex")
	}
	// Money survives the round trip as cents, not as a float that has been
	// through a decimal on the way.
	if got.PurchasePriceCents == nil || *got.PurchasePriceCents != domain.Money(28500000) {
		t.Errorf("PurchasePriceCents = %v, want 28500000", got.PurchasePriceCents)
	}
	if got.Baths == nil || *got.Baths != 1.5 {
		t.Errorf("Baths = %v, want 1.5", got.Baths)
	}
	if got.NormalizedAddress != "412 ELM ST ATHENS OH 45701" {
		t.Errorf("NormalizedAddress = %q", got.NormalizedAddress)
	}
}

func TestRepoDistinguishesNullFromZero(t *testing.T) {
	// The reason for emit_pointers_for_null_types: a property with no recorded
	// purchase price is not a property that cost nothing, and the two have to
	// stay distinguishable through the store.
	repo := openRepo(t)
	zero := domain.Money(0)

	unknown, err := repo.Write().CreateProperty(t.Context(), sqlc.CreatePropertyParams{
		Nickname: "Unknown price", AddressLine1: "1 A St", Status: "prospect",
		CreatedAt: now(), UpdatedAt: now(),
	})
	if err != nil {
		t.Fatalf("CreateProperty: %v", err)
	}
	free, err := repo.Write().CreateProperty(t.Context(), sqlc.CreatePropertyParams{
		Nickname: "Free", AddressLine1: "2 B St", Status: "prospect",
		PurchasePriceCents: &zero, CreatedAt: now(), UpdatedAt: now(),
	})
	if err != nil {
		t.Fatalf("CreateProperty: %v", err)
	}

	if unknown.PurchasePriceCents != nil {
		t.Errorf("an unrecorded price came back as %v, want nil", *unknown.PurchasePriceCents)
	}
	if free.PurchasePriceCents == nil {
		t.Error("a recorded price of zero came back as nil, want 0")
	} else if *free.PurchasePriceCents != 0 {
		t.Errorf("recorded price = %v, want 0", *free.PurchasePriceCents)
	}
}

func TestRepoPaginatesByNicknameThenID(t *testing.T) {
	// Two properties share a nickname, so the cursor has to break the tie on
	// id or the page boundary either repeats a row or skips one.
	repo := openRepo(t)
	for _, name := range []string{"Birch", "Alder", "Cedar", "Alder"} {
		newProperty(t, repo, name)
	}

	first, err := repo.Read().ListPropertiesFirstPage(t.Context(), 2)
	if err != nil {
		t.Fatalf("ListPropertiesFirstPage: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("first page has %d rows, want 2", len(first))
	}
	if first[0].Property.Nickname != "Alder" || first[1].Property.Nickname != "Alder" {
		t.Fatalf("first page = %q, %q; want both Alder",
			first[0].Property.Nickname, first[1].Property.Nickname)
	}
	if first[0].Property.ID >= first[1].Property.ID {
		t.Error("tied nicknames are not ordered by id")
	}

	last := first[len(first)-1].Property
	rest, err := repo.Read().ListPropertiesAfter(t.Context(), sqlc.ListPropertiesAfterParams{
		AfterNickname: last.Nickname,
		AfterID:       last.ID,
		PageSize:      10,
	})
	if err != nil {
		t.Fatalf("ListPropertiesAfter: %v", err)
	}

	var names []string
	for _, r := range rest {
		names = append(names, r.Property.Nickname)
	}
	if len(names) != 2 || names[0] != "Birch" || names[1] != "Cedar" {
		t.Errorf("page after the cursor = %v, want [Birch Cedar]", names)
	}
}

func TestRepoCountsUnitsWithoutASecondQuery(t *testing.T) {
	repo := openRepo(t)
	p := newProperty(t, repo, "Elm Street Duplex")

	for _, label := range []string{"Main", "Upper"} {
		if _, err := repo.Write().CreateUnit(t.Context(), sqlc.CreateUnitParams{
			PropertyID: p.ID, Label: label, CreatedAt: now(), UpdatedAt: now(),
		}); err != nil {
			t.Fatalf("CreateUnit(%q): %v", label, err)
		}
	}

	rows, err := repo.Read().ListPropertiesFirstPage(t.Context(), 10)
	if err != nil {
		t.Fatalf("ListPropertiesFirstPage: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].UnitCount != 2 {
		t.Errorf("UnitCount = %d, want 2", rows[0].UnitCount)
	}
}

func TestRepoRejectsADuplicateUnitLabel(t *testing.T) {
	// UNIQUE (property_id, label) is what stops two "Main" units accumulating
	// on one property and a lease attaching to the wrong one.
	repo := openRepo(t)
	p := newProperty(t, repo, "Elm Street Duplex")

	for i := range 2 {
		_, err := repo.Write().CreateUnit(t.Context(), sqlc.CreateUnitParams{
			PropertyID: p.ID, Label: "Main", CreatedAt: now(), UpdatedAt: now(),
		})
		if i == 0 && err != nil {
			t.Fatalf("first CreateUnit: %v", err)
		}
		if i == 1 && err == nil {
			t.Error("a duplicate unit label was accepted, want a constraint error")
		}
	}
}

func TestRepoCascadesUnitsWhenAPropertyGoes(t *testing.T) {
	repo := openRepo(t)
	p := newProperty(t, repo, "Elm Street Duplex")
	if _, err := repo.Write().CreateUnit(t.Context(), sqlc.CreateUnitParams{
		PropertyID: p.ID, Label: "Main", CreatedAt: now(), UpdatedAt: now(),
	}); err != nil {
		t.Fatalf("CreateUnit: %v", err)
	}

	rows, err := repo.Write().DeleteProperty(t.Context(), p.ID)
	if err != nil {
		t.Fatalf("DeleteProperty: %v", err)
	}
	if rows != 1 {
		t.Errorf("DeleteProperty affected %d rows, want 1", rows)
	}

	units, err := repo.Read().ListUnitsByProperty(t.Context(), p.ID)
	if err != nil {
		t.Fatalf("ListUnitsByProperty: %v", err)
	}
	if len(units) != 0 {
		t.Errorf("%d units survived the property, want 0", len(units))
	}
}

func TestRepoTxRollsBackOnError(t *testing.T) {
	repo := openRepo(t)
	boom := errors.New("boom")

	err := repo.Tx(t.Context(), func(q *sqlc.Queries) error {
		if _, err := q.CreateProperty(t.Context(), sqlc.CreatePropertyParams{
			Nickname: "Half written", AddressLine1: "1 A St", Status: "active",
			CreatedAt: now(), UpdatedAt: now(),
		}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx returned %v, want boom", err)
	}

	rows, err := repo.Read().ListPropertiesFirstPage(t.Context(), 10)
	if err != nil {
		t.Fatalf("ListPropertiesFirstPage: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("%d properties survived a rolled-back transaction, want 0", len(rows))
	}
}

func TestNotFoundRecognisesAMissingRow(t *testing.T) {
	repo := openRepo(t)
	_, err := repo.Read().GetProperty(t.Context(), 404)
	if err == nil {
		t.Fatal("GetProperty on a missing id returned no error")
	}
	if !NotFound(err) {
		t.Errorf("NotFound(%v) = false, want true", err)
	}
}
