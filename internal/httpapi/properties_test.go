package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/farrellm/rental-bot/internal/blob"
)

// api drives the guarded API as a signed-in operator.
type api struct {
	t       *testing.T
	handler http.Handler
	request func(method, target string, body any) *http.Request
	// raw builds a request whose body is not JSON, for multipart uploads.
	raw func(method, target string, body io.Reader) *http.Request
	// blobs is the store the documents actually land in, so a test can check
	// what reached the disk rather than only what the API said.
	blobs *blob.Store
}

func newAPI(t *testing.T) *api { return newAPIWith(t, Options{}) }

// newAPIWith is newAPI with the options a test wants to change -- a smaller
// upload cap, an unwell database.
func newAPIWith(t *testing.T, opts Options) *api {
	t.Helper()
	opts, request := authed(t, opts)
	handler := New(opts)
	return &api{
		t:       t,
		handler: handler,
		blobs:   opts.Blobs,
		raw:     request,
		request: func(method, target string, body any) *http.Request {
			if body == nil {
				return request(method, target, nil)
			}
			return request(method, target, jsonBody(t, body))
		},
	}
}

func (a *api) do(method, target string, body any) *httptest.ResponseRecorder {
	a.t.Helper()
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, a.request(method, target, body))
	return rec
}

// decode reads a successful response, failing the test on any other status.
func (a *api) decode(rec *httptest.ResponseRecorder, want int, into any) {
	a.t.Helper()
	if rec.Code != want {
		a.t.Fatalf("status = %d, want %d (body %s)", rec.Code, want, rec.Body)
	}
	if into != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
			a.t.Fatalf("decode %s: %v", rec.Body, err)
		}
	}
}

// newProperty creates one and returns it.
func (a *api) newProperty(body map[string]any) propertyDetail {
	a.t.Helper()
	var out propertyDetail
	a.decode(a.do(http.MethodPost, "/api/v1/properties", body), http.StatusCreated, &out)
	return out
}

func elmStreet() map[string]any {
	return map[string]any{
		"nickname":             "Elm Street Duplex",
		"address_line1":        "412 Elm Street",
		"city":                 "Athens",
		"state":                "Ohio",
		"postal_code":          "45701-2233",
		"purchase_price_cents": 28500000,
		"beds":                 3,
		"baths":                1.5,
	}
}

func TestCreatePropertyMakesAnImplicitUnit(t *testing.T) {
	// Every lease hangs off a unit, so a single-family property cannot be
	// created without one.
	a := newAPI(t)
	got := a.newProperty(elmStreet())

	if len(got.Units) != 1 {
		t.Fatalf("%d units, want 1", len(got.Units))
	}
	if got.Units[0].Label != implicitUnitLabel {
		t.Errorf("implicit unit label = %q, want %q", got.Units[0].Label, implicitUnitLabel)
	}
	if got.Units[0].PropertyID != got.ID {
		t.Errorf("the unit belongs to property %d, want %d", got.Units[0].PropertyID, got.ID)
	}
}

func TestCreatePropertyNormalizesTheAddress(t *testing.T) {
	a := newAPI(t)
	got := a.newProperty(elmStreet())

	if got.NormalizedAddress != "412 ELM ST ATHENS OH 45701" {
		t.Errorf("NormalizedAddress = %q, want %q", got.NormalizedAddress, "412 ELM ST ATHENS OH 45701")
	}
	// The display address is untouched; only the match key is folded.
	if got.AddressLine1 != "412 Elm Street" {
		t.Errorf("AddressLine1 = %q, want it unchanged", got.AddressLine1)
	}
}

func TestCreatePropertyKeepsMoneyAsCents(t *testing.T) {
	a := newAPI(t)
	rec := a.do(http.MethodPost, "/api/v1/properties", elmStreet())

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body)
	}
	// On the wire it is an integer, never a decimal a client could misread.
	if !strings.Contains(rec.Body.String(), `"purchase_price_cents":28500000`) {
		t.Errorf("money is not integer cents on the wire: %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "285000.00") {
		t.Errorf("money was rendered as a decimal: %s", rec.Body)
	}
}

func TestCreatePropertyAcceptsNamedUnits(t *testing.T) {
	a := newAPI(t)
	body := elmStreet()
	body["units"] = []map[string]any{
		{"label": "Upper", "beds": 2},
		{"label": "Lower", "beds": 1},
	}
	got := a.newProperty(body)

	if len(got.Units) != 2 {
		t.Fatalf("%d units, want 2", len(got.Units))
	}
	for _, u := range got.Units {
		if u.Label == implicitUnitLabel {
			t.Error("a named multi-family property also got the implicit unit")
		}
	}
}

func TestCreatePropertyRejectsInvalidInput(t *testing.T) {
	a := newAPI(t)

	tests := []struct {
		name   string
		mutate func(map[string]any)
		want   int
	}{
		{"no nickname", func(m map[string]any) { m["nickname"] = "  " }, http.StatusUnprocessableEntity},
		{"no address", func(m map[string]any) { m["address_line1"] = "" }, http.StatusUnprocessableEntity},
		{"bad status", func(m map[string]any) { m["status"] = "haunted" }, http.StatusUnprocessableEntity},
		{"bad date", func(m map[string]any) { m["purchase_date"] = "04/02/2019" }, http.StatusUnprocessableEntity},
		{"negative beds", func(m map[string]any) { m["beds"] = -1 }, http.StatusUnprocessableEntity},
		{"impossible year", func(m map[string]any) { m["year_built"] = 12 }, http.StatusUnprocessableEntity},
		{"duplicate labels", func(m map[string]any) {
			m["units"] = []map[string]any{{"label": "A"}, {"label": "A"}}
		}, http.StatusUnprocessableEntity},
		{"blank label", func(m map[string]any) {
			m["units"] = []map[string]any{{"label": " "}}
		}, http.StatusUnprocessableEntity},
		{"unknown field", func(m map[string]any) { m["haunted"] = true }, http.StatusBadRequest},
		{"money as a decimal", func(m map[string]any) { m["purchase_price_cents"] = 285000.50 }, http.StatusBadRequest},
	}
	for _, tt := range tests {
		body := elmStreet()
		tt.mutate(body)
		rec := a.do(http.MethodPost, "/api/v1/properties", body)
		if rec.Code != tt.want {
			t.Errorf("%s: status = %d, want %d (body %s)", tt.name, rec.Code, tt.want, rec.Body)
		}
		if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/problem+json") {
			t.Errorf("%s: error is not problem+json", tt.name)
		}
	}
}

func TestGetPropertyCarriesItsUnits(t *testing.T) {
	a := newAPI(t)
	created := a.newProperty(elmStreet())

	var got propertyDetail
	a.decode(a.do(http.MethodGet, propertyPath(created.ID), nil), http.StatusOK, &got)

	if got.ID != created.ID || len(got.Units) != 1 {
		t.Errorf("detail = %+v, want the property and its one unit", got)
	}
}

func TestGetPropertyMissingIs404(t *testing.T) {
	a := newAPI(t)
	for _, target := range []string{"/api/v1/properties/999", "/api/v1/properties/banana", "/api/v1/properties/0"} {
		if rec := a.do(http.MethodGet, target, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", target, rec.Code)
		}
	}
}

func TestPatchDistinguishesAbsentFromNull(t *testing.T) {
	// The whole reason PATCH decodes into raw messages. An omitted field means
	// "leave it", a null means "clear it", and a value means "set it". A
	// struct would collapse the first two and quietly wipe the column.
	a := newAPI(t)
	created := a.newProperty(elmStreet())

	// Absent: beds survives a patch that only touches the nickname.
	var afterAbsent propertyDetail
	a.decode(a.do(http.MethodPatch, propertyPath(created.ID),
		map[string]any{"nickname": "Elm Street"}), http.StatusOK, &afterAbsent)
	if afterAbsent.Beds == nil || *afterAbsent.Beds != 3 {
		t.Errorf("beds = %v after an unrelated patch, want 3", afterAbsent.Beds)
	}
	if afterAbsent.Nickname != "Elm Street" {
		t.Errorf("nickname = %q, want it changed", afterAbsent.Nickname)
	}

	// Null: beds clears.
	var afterNull propertyDetail
	a.decode(a.do(http.MethodPatch, propertyPath(created.ID),
		map[string]any{"beds": nil}), http.StatusOK, &afterNull)
	if afterNull.Beds != nil {
		t.Errorf("beds = %v after an explicit null, want nil", *afterNull.Beds)
	}
	// And clearing beds left the price alone.
	if afterNull.PurchasePriceCents == nil {
		t.Error("the purchase price was cleared by a patch that did not name it")
	}

	// Value: beds sets, including to zero, which is not the same as unknown.
	var afterZero propertyDetail
	a.decode(a.do(http.MethodPatch, propertyPath(created.ID),
		map[string]any{"beds": 0}), http.StatusOK, &afterZero)
	if afterZero.Beds == nil {
		t.Fatal("beds = null after being set to 0, want 0")
	}
	if *afterZero.Beds != 0 {
		t.Errorf("beds = %d, want 0", *afterZero.Beds)
	}
}

func TestPatchRecomputesTheMatchKey(t *testing.T) {
	// The normalized address has to follow the address it describes, or the
	// ingestion pipeline matches documents to the old one.
	a := newAPI(t)
	created := a.newProperty(elmStreet())

	var got propertyDetail
	a.decode(a.do(http.MethodPatch, propertyPath(created.ID),
		map[string]any{"address_line1": "88 North Oak Avenue"}), http.StatusOK, &got)

	if got.NormalizedAddress != "88 N OAK AVE ATHENS OH 45701" {
		t.Errorf("NormalizedAddress = %q, want it recomputed", got.NormalizedAddress)
	}
}

func TestPatchRejectsDerivedAndUnknownFields(t *testing.T) {
	a := newAPI(t)
	created := a.newProperty(elmStreet())

	tests := []struct {
		name string
		body map[string]any
		want int
	}{
		// normalized_address is derived; setting it would let the match key
		// disagree with the address it names.
		{"derived field", map[string]any{"normalized_address": "ANYTHING"}, http.StatusBadRequest},
		{"unknown field", map[string]any{"nickmame": "typo"}, http.StatusBadRequest},
		{"read-only id", map[string]any{"id": 7}, http.StatusBadRequest},
		{"empty body", map[string]any{}, http.StatusBadRequest},
		{"null on a non-nullable column", map[string]any{"nickname": nil}, http.StatusUnprocessableEntity},
		{"wrong type", map[string]any{"beds": "three"}, http.StatusUnprocessableEntity},
		{"invalid value", map[string]any{"status": "haunted"}, http.StatusUnprocessableEntity},
	}
	for _, tt := range tests {
		rec := a.do(http.MethodPatch, propertyPath(created.ID), tt.body)
		if rec.Code != tt.want {
			t.Errorf("%s: status = %d, want %d (body %s)", tt.name, rec.Code, tt.want, rec.Body)
		}
	}

	// None of that changed anything.
	var after propertyDetail
	a.decode(a.do(http.MethodGet, propertyPath(created.ID), nil), http.StatusOK, &after)
	if after.Nickname != "Elm Street Duplex" || after.NormalizedAddress != "412 ELM ST ATHENS OH 45701" {
		t.Errorf("a rejected patch changed the record: %+v", after)
	}
}

func TestDeletePropertyTakesItsUnits(t *testing.T) {
	a := newAPI(t)
	created := a.newProperty(elmStreet())

	if rec := a.do(http.MethodDelete, propertyPath(created.ID), nil); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if rec := a.do(http.MethodGet, propertyPath(created.ID), nil); rec.Code != http.StatusNotFound {
		t.Errorf("the property survived deletion: %d", rec.Code)
	}
	if rec := a.do(http.MethodDelete, propertyPath(created.ID), nil); rec.Code != http.StatusNotFound {
		t.Errorf("deleting it twice = %d, want 404", rec.Code)
	}
}

func TestListPaginatesAcrossThePageBoundary(t *testing.T) {
	a := newAPI(t)
	for _, name := range []string{"Cedar", "Alder", "Birch", "Elm", "Dogwood"} {
		body := elmStreet()
		body["nickname"] = name
		a.newProperty(body)
	}

	var seen []string
	cursor := ""
	for range 10 {
		target := "/api/v1/properties?limit=2"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		var page propertyList
		a.decode(a.do(http.MethodGet, target, nil), http.StatusOK, &page)

		for _, item := range page.Items {
			seen = append(seen, item.Nickname)
			if item.UnitCount != 1 {
				t.Errorf("%s: UnitCount = %d, want 1", item.Nickname, item.UnitCount)
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	want := []string{"Alder", "Birch", "Cedar", "Dogwood", "Elm"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Errorf("paging returned %v, want %v", seen, want)
	}
}

func TestListRejectsAForgedCursor(t *testing.T) {
	a := newAPI(t)
	for _, target := range []string{
		"/api/v1/properties?cursor=not-base64!!",
		"/api/v1/properties?cursor=bm90aGluZw",
		"/api/v1/properties?limit=0",
		"/api/v1/properties?limit=nine",
	} {
		if rec := a.do(http.MethodGet, target, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", target, rec.Code)
		}
	}
}

func TestListIsEmptyNotNull(t *testing.T) {
	// The frontend maps over this; null would be a crash on an empty portfolio.
	a := newAPI(t)
	rec := a.do(http.MethodGet, "/api/v1/properties", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Errorf("empty list = %s, want an empty array", rec.Body)
	}
}

func TestPropertyRoutesAllNeedASession(t *testing.T) {
	opts, _ := authed(t, Options{})
	handler := New(opts)

	tests := []struct{ method, target string }{
		{http.MethodGet, "/api/v1/properties"},
		{http.MethodPost, "/api/v1/properties"},
		{http.MethodGet, "/api/v1/properties/1"},
		{http.MethodPatch, "/api/v1/properties/1"},
		{http.MethodDelete, "/api/v1/properties/1"},
		{http.MethodGet, "/api/v1/properties/1/units"},
		{http.MethodPost, "/api/v1/properties/1/units"},
		{http.MethodPatch, "/api/v1/units/1"},
		{http.MethodDelete, "/api/v1/units/1"},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.target, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", tt.method, tt.target, rec.Code)
		}
	}
}

func TestPropertyMutationsNeedACSRFToken(t *testing.T) {
	opts, request := authed(t, Options{})
	handler := New(opts)

	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
		target := "/api/v1/properties"
		if method != http.MethodPost {
			target = "/api/v1/properties/1"
		}
		req := request(method, target, jsonBody(t, map[string]any{"nickname": "x"}))
		req.Header.Del("X-CSRF-Token")

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s without a CSRF token = %d, want 403", method, target, rec.Code)
		}
	}
}

func propertyPath(id int64) string {
	return "/api/v1/properties/" + itoa(id)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
