package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// These benchmarks cover the per-request work every handler shares: routing,
// decoding a PATCH body and merging it, paging cursors, and encoding a list
// response. None of it is hot in the sense a web server is hot -- this is a
// single-operator application -- but all of it is on the path of every request,
// which makes it the part of the API worth having a number for.

func BenchmarkEncodeCursor(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		cursor = encodeCursor("Maple Street duplex", 4823)
	}
}

func BenchmarkDecodeCursor(b *testing.B) {
	encoded := encodeCursor("Maple Street duplex", 4823)
	b.ReportAllocs()
	for b.Loop() {
		cursor, cursorID, _ = decodeCursor(encoded)
	}
}

func BenchmarkDecodePatch(b *testing.B) {
	const body = `{"nickname":"Maple Street duplex","city":"Athens",` +
		`"purchase_price_cents":18500000,"beds":3,"zpid":null}`

	b.ReportAllocs()
	for b.Loop() {
		r := httptest.NewRequest(http.MethodPatch, "/api/v1/properties/1", strings.NewReader(body))
		if _, ok := decodePatch(httptest.NewRecorder(), r, propertyPatchFields...); !ok {
			b.Fatal("decodePatch rejected a body it should accept")
		}
	}
}

func BenchmarkApplyPropertyPatch(b *testing.B) {
	const body = `{"nickname":"Maple Street duplex","city":"Athens",` +
		`"purchase_price_cents":18500000,"beds":3,"zpid":null}`

	r := httptest.NewRequest(http.MethodPatch, "/api/v1/properties/1", strings.NewReader(body))
	p, ok := decodePatch(httptest.NewRecorder(), r, propertyPatchFields...)
	if !ok {
		b.Fatal("decodePatch rejected a body it should accept")
	}

	b.ReportAllocs()
	for b.Loop() {
		current := benchProperty()
		if err := applyPropertyPatch(p, &current); err != nil {
			b.Fatalf("applyPropertyPatch: %v", err)
		}
	}
}

func BenchmarkAllowHeader(b *testing.B) {
	m := methods{
		http.MethodGet:    nil,
		http.MethodPatch:  nil,
		http.MethodDelete: nil,
	}
	b.ReportAllocs()
	for b.Loop() {
		allow = allowHeader(m)
	}
}

// BenchmarkRouteDispatch measures the wrapper `route` puts in front of every
// handler, on both the path it answers and the 405 path it owns.
func BenchmarkRouteDispatch(b *testing.B) {
	mux := http.NewServeMux()
	route(mux, "/api/v1/properties/{id}", methods{
		http.MethodGet:    func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) },
		http.MethodPatch:  func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) },
		http.MethodDelete: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) },
	})

	b.Run("allowed", func(b *testing.B) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/properties/12", nil)
		b.ReportAllocs()
		for b.Loop() {
			mux.ServeHTTP(httptest.NewRecorder(), req)
		}
	})
	b.Run("not-allowed", func(b *testing.B) {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/properties/12", nil)
		b.ReportAllocs()
		for b.Loop() {
			mux.ServeHTTP(httptest.NewRecorder(), req)
		}
	})
}

func BenchmarkWriteJSONPropertyList(b *testing.B) {
	out := propertyList{Items: make([]propertyListItem, 0, 50)}
	for i := range 50 {
		p := benchProperty()
		p.ID = int64(i + 1)
		p.Nickname = "Property " + strconv.Itoa(i+1)
		out.Items = append(out.Items, propertyListItem{
			propertyResponse: newPropertyResponse(p),
			UnitCount:        2,
		})
	}
	out.NextCursor = encodeCursor(out.Items[len(out.Items)-1].Nickname, out.Items[len(out.Items)-1].ID)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/properties", nil)
	b.ReportAllocs()
	for b.Loop() {
		writeJSON(httptest.NewRecorder(), req, http.StatusOK, out)
	}
}

// benchProperty is a row with every nullable column populated, so a merge or an
// encode does the most work it ever has to.
func benchProperty() sqlc.Property {
	purchaseDate := "2019-04-11"
	price := domain.Money(18500000)
	beds := int64(3)
	baths := 1.5
	sqft := int64(1840)
	year := int64(1962)
	zpid := "2078445513"

	return sqlc.Property{
		ID:                 1,
		Nickname:           "Elm Street house",
		AddressLine1:       "412 Elm Street",
		AddressLine2:       "Rear",
		City:               "Athens",
		State:              "OH",
		PostalCode:         "45701",
		County:             "Athens",
		NormalizedAddress:  "412 ELM ST ATHENS OH 45701",
		PurchaseDate:       &purchaseDate,
		PurchasePriceCents: &price,
		Beds:               &beds,
		Baths:              &baths,
		Sqft:               &sqft,
		YearBuilt:          &year,
		Status:             "active",
		Zpid:               &zpid,
		Notes:              "Tenant pays gas; landlord pays water.",
		CreatedAt:          "2026-02-01T10:00:00Z",
		UpdatedAt:          "2026-08-01T10:00:00Z",
	}
}

// These keep the compiler from eliding the work being measured.
var (
	cursor   string
	cursorID int64
	allow    string
)
