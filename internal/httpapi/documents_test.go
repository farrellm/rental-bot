package httpapi

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/farrellm/rental-bot/internal/config"
)

// upload posts one document as a multipart form and returns the response.
func (a *api) upload(filename, contentType string, content []byte, fields map[string]string) *httptest.ResponseRecorder {
	a.t.Helper()

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for name, value := range fields {
		if err := w.WriteField(name, value); err != nil {
			a.t.Fatal(err)
		}
	}
	header := map[string][]string{
		"Content-Disposition": {`form-data; name="file"; filename="` + filename + `"`},
	}
	if contentType != "" {
		header["Content-Type"] = []string{contentType}
	}
	part, err := w.CreatePart(header)
	if err != nil {
		a.t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		a.t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		a.t.Fatal(err)
	}

	req := a.raw(http.MethodPost, "/api/v1/documents", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())

	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)
	return rec
}

// newDocument uploads one and returns it, failing the test if it was refused.
func (a *api) newDocument(filename, contentType string, content []byte, fields map[string]string) uploadResponse {
	a.t.Helper()
	var out uploadResponse
	a.decode(a.upload(filename, contentType, content, fields), http.StatusCreated, &out)
	return out
}

func documentPath(id int64, suffix string) string {
	return "/api/v1/documents/" + itoa(id) + suffix
}

func TestUploadFilesADocument(t *testing.T) {
	a := newAPI(t)
	property := a.newProperty(elmStreet())

	doc := a.newDocument("receipt.pdf", "application/pdf", []byte("%PDF-1.4 not really"),
		map[string]string{
			"kind":        "receipt",
			"title":       "Ace Plumbing, kitchen tap",
			"property_id": itoa(property.ID),
		})

	if doc.Deduplicated {
		t.Error("a first upload reported itself as already on file")
	}
	if doc.Title != "Ace Plumbing, kitchen tap" || doc.Kind != "receipt" {
		t.Errorf("document = %+v", doc)
	}
	if doc.Mime != "application/pdf" {
		t.Errorf("Mime = %q, want application/pdf", doc.Mime)
	}
	if len(doc.SHA256) != 64 {
		t.Errorf("SHA256 = %q, want a 64-character digest", doc.SHA256)
	}
	if doc.SizeBytes != int64(len("%PDF-1.4 not really")) {
		t.Errorf("SizeBytes = %d", doc.SizeBytes)
	}
	if doc.UploadedBy == nil {
		t.Error("UploadedBy is null; the signed-in operator should be recorded")
	}

	// The bytes reached the store under the digest the API reported.
	if _, err := a.blobs.Stat(doc.SHA256); err != nil {
		t.Errorf("the document is not in the blob store: %v", err)
	}
}

func TestUploadingTheSameBytesTwiceFilesOneDocument(t *testing.T) {
	// Forwarding the same receipt twice is normal. The second upload is the
	// same document, and saying so is more useful than either a duplicate row
	// or an error.
	a := newAPI(t)
	content := []byte("the same lease, forwarded again")

	first := a.newDocument("lease.pdf", "application/pdf", content, map[string]string{"kind": "lease"})

	var second uploadResponse
	a.decode(a.upload("forwarded-lease.pdf", "application/pdf", content,
		map[string]string{"kind": "lease"}), http.StatusOK, &second)

	if second.ID != first.ID {
		t.Errorf("second upload made document %d, want the existing %d", second.ID, first.ID)
	}
	if !second.Deduplicated {
		t.Error("the second upload did not report itself as already on file")
	}
	// The first upload's description stands: the document was already named.
	if second.Title != first.Title {
		t.Errorf("Title = %q, want the filed %q", second.Title, first.Title)
	}
}

func TestUploadRefusesAnOversizedDocument(t *testing.T) {
	a := newAPIWith(t, Options{Config: smallUploads(64)})

	rec := a.upload("big.pdf", "application/pdf",
		bytes.Repeat([]byte("x"), 4096), map[string]string{"kind": "other"})

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body %s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "limit") {
		t.Errorf("the refusal does not mention the limit: %s", rec.Body)
	}
}

func TestDocumentContentDecidesWhatMayRenderInTheBrowser(t *testing.T) {
	// A document is served from the app's own origin. An uploaded HTML file or
	// SVG rendered inline would run script with the operator's session, so
	// anything outside the allowlist downloads instead, which is inert.
	a := newAPI(t)

	tests := []struct {
		name        string
		filename    string
		contentType string
		content     string
		want        string
	}{
		{name: "pdf renders", filename: "a.pdf", contentType: "application/pdf",
			content: "%PDF", want: "inline"},
		{name: "png renders", filename: "a.png", contentType: "image/png",
			content: "\x89PNG", want: "inline"},
		{name: "html downloads", filename: "a.html", contentType: "text/html",
			content: "<script>alert(document.cookie)</script>", want: "attachment"},
		{name: "svg downloads", filename: "a.svg", contentType: "image/svg+xml",
			content: "<svg xmlns='http://www.w3.org/2000/svg'><script/></svg>", want: "attachment"},
		{name: "unknown downloads", filename: "a.bin", contentType: "application/octet-stream",
			content: "\x00\x01", want: "attachment"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := a.newDocument(tt.filename, tt.contentType, []byte(tt.content),
				map[string]string{"kind": "other"})

			rec := a.do(http.MethodGet, documentPath(doc.ID, "/content"), nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("content = %d, want 200 (body %s)", rec.Code, rec.Body)
			}
			if disp := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(disp, tt.want) {
				t.Errorf("Content-Disposition = %q, want %s", disp, tt.want)
			}
			if rec.Body.String() != tt.content {
				t.Errorf("body = %q, want %q", rec.Body, tt.content)
			}
			// The second and third layers, on every document regardless.
			if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Error("no nosniff header; a browser may second-guess the declared type")
			}
			if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
				t.Errorf("Content-Security-Policy = %q", csp)
			}
		})
	}
}

func TestDocumentContentNeedsASession(t *testing.T) {
	// A document URL without a session is a 401, not a file (DESIGN.md 9.2).
	// The blob directory is never mapped by the proxy, and this handler is the
	// only way in.
	a := newAPI(t)
	doc := a.newDocument("lease.pdf", "application/pdf", []byte("private"),
		map[string]string{"kind": "lease"})

	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, documentPath(doc.ID, "/content"), nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "private") {
		t.Error("the document's bytes were served to an unauthenticated request")
	}
}

func TestUploadRejectsWhatTheSchemaWould(t *testing.T) {
	a := newAPI(t)

	tests := []struct {
		name     string
		fields   map[string]string
		want     int
		wantText string
	}{
		{
			name:     "unknown kind",
			fields:   map[string]string{"kind": "invoice"},
			want:     http.StatusUnprocessableEntity,
			wantText: "receipt",
		},
		{
			name:   "a property that does not exist",
			fields: map[string]string{"kind": "other", "property_id": "9999"},
			want:   http.StatusNotFound,
		},
		{
			name:     "half a link",
			fields:   map[string]string{"kind": "other", "entity_type": "property"},
			want:     http.StatusUnprocessableEntity,
			wantText: "entity_id",
		},
		{
			name:     "an entity type the CHECK would refuse",
			fields:   map[string]string{"kind": "other", "entity_type": "mortgage", "entity_id": "1"},
			want:     http.StatusUnprocessableEntity,
			wantText: "entity_type",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := a.upload("x.pdf", "application/pdf", []byte(tt.name), tt.fields)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.want, rec.Body)
			}
			if tt.wantText != "" && !strings.Contains(rec.Body.String(), tt.wantText) {
				t.Errorf("the refusal does not mention %q: %s", tt.wantText, rec.Body)
			}
		})
	}
}

func TestUploadWithoutAFileIsRefused(t *testing.T) {
	a := newAPI(t)

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("kind", "receipt"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	req := a.raw(http.MethodPost, "/api/v1/documents", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body)
	}
}

func TestDocumentsAreFiledAgainstTheRecordsTheyEvidence(t *testing.T) {
	a := newAPI(t)
	property := a.newProperty(elmStreet())

	doc := a.newDocument("lease.pdf", "application/pdf", []byte("a lease"),
		map[string]string{
			"kind":        "lease",
			"property_id": itoa(property.ID),
			"entity_type": "property",
			"entity_id":   itoa(property.ID),
		})

	var full documentResponse
	a.decode(a.do(http.MethodGet, documentPath(doc.ID, ""), nil), http.StatusOK, &full)
	if len(full.Links) != 1 || full.Links[0].EntityType != "property" || full.Links[0].EntityID != property.ID {
		t.Fatalf("links = %+v, want one property link", full.Links)
	}

	// Filing it against the same thing again is what the caller already asked
	// for, so it is not an error.
	link := map[string]any{"entity_type": "property", "entity_id": property.ID}
	if rec := a.do(http.MethodPost, documentPath(doc.ID, "/links"), link); rec.Code != http.StatusNoContent {
		t.Errorf("re-filing = %d, want 204 (body %s)", rec.Code, rec.Body)
	}

	if rec := a.do(http.MethodDelete, documentPath(doc.ID, "/links"), link); rec.Code != http.StatusNoContent {
		t.Errorf("unfiling = %d, want 204 (body %s)", rec.Code, rec.Body)
	}
	if rec := a.do(http.MethodDelete, documentPath(doc.ID, "/links"), link); rec.Code != http.StatusNotFound {
		t.Errorf("unfiling twice = %d, want 404", rec.Code)
	}
}

func TestListDocumentsIsScopedToItsProperty(t *testing.T) {
	a := newAPI(t)
	mine := a.newProperty(elmStreet())
	theirs := a.newProperty(map[string]any{"nickname": "Oak Street House", "address_line1": "9 Oak St"})

	a.newDocument("a.pdf", "application/pdf", []byte("mine"),
		map[string]string{"kind": "receipt", "property_id": itoa(mine.ID)})
	a.newDocument("b.pdf", "application/pdf", []byte("theirs"),
		map[string]string{"kind": "receipt", "property_id": itoa(theirs.ID)})

	var list documentList
	a.decode(a.do(http.MethodGet, propertyPath(mine.ID)+"/documents", nil), http.StatusOK, &list)

	if len(list.Items) != 1 {
		t.Fatalf("%d documents, want 1: %+v", len(list.Items), list.Items)
	}
	if list.Items[0].OriginalFilename != "a.pdf" {
		t.Errorf("listed %q, want the property's own document", list.Items[0].OriginalFilename)
	}
}

func TestDeletingADocumentKeepsTheBytes(t *testing.T) {
	// The row goes; the blob stays. A digest is the only name content has, and
	// a restore should still find the file where the backup put it. Reclaiming
	// unreferenced blobs is a sweep, not a side effect of one delete.
	a := newAPI(t)
	doc := a.newDocument("x.pdf", "application/pdf", []byte("keep me"),
		map[string]string{"kind": "other"})

	if rec := a.do(http.MethodDelete, documentPath(doc.ID, ""), nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204 (body %s)", rec.Code, rec.Body)
	}
	if _, err := a.blobs.Stat(doc.SHA256); err != nil {
		t.Errorf("the blob went with the row: %v", err)
	}
	if rec := a.do(http.MethodGet, documentPath(doc.ID, ""), nil); rec.Code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", rec.Code)
	}
}

func TestPatchDocumentLeavesTheContentAlone(t *testing.T) {
	// Title and kind are the operator's description of a document. The bytes,
	// the digest, and the size are what was uploaded, and PATCH cannot reach
	// them -- a record that claimed a different hash than its content would be
	// worse than no record.
	a := newAPI(t)
	doc := a.newDocument("scan.pdf", "application/pdf", []byte("a lease"),
		map[string]string{"kind": "other"})

	var updated documentResponse
	a.decode(a.do(http.MethodPatch, documentPath(doc.ID, ""), map[string]any{
		"kind":  "lease",
		"title": "2026 lease, Apt 2",
	}), http.StatusOK, &updated)

	if updated.Kind != "lease" || updated.Title != "2026 lease, Apt 2" {
		t.Errorf("document = %+v", updated)
	}
	if updated.SHA256 != doc.SHA256 || updated.SizeBytes != doc.SizeBytes {
		t.Errorf("PATCH changed the content identity: %+v", updated)
	}

	rec := a.do(http.MethodPatch, documentPath(doc.ID, ""), map[string]any{"sha256": "x"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("patching sha256 = %d, want 400", rec.Code)
	}
}

// smallUploads returns a config whose upload cap is n bytes.
func smallUploads(n int64) config.Config {
	cfg := config.Default()
	cfg.Storage.MaxUploadBytes = n
	return cfg
}
