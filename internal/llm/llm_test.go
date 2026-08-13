package llm

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/zendev-sh/goai/provider"
)

// Every field the model is asked for has to have a json name and a description
// worth reading, because those two strings are the prompt. A field that
// reaches the schema unnamed or unexplained is a field the model fills in by
// guessing what it is for.
func TestEveryExtractedFieldIsNamedAndDescribed(t *testing.T) {
	// Money and date rules from docs/DESIGN.md section 5.3: a cents field says
	// "no decimal point" and a date field says YYYY-MM-DD. Models drift
	// between 482.19 and 48219 without the first, and invent a timezone
	// without the second.
	for _, schema := range []any{
		Classification{},
		ReceiptExtract{},
		LineItem{},
		LeaseExtract{},
		LeaseTenantExtract{},
		InsuranceExtract{},
		MortgageStatementExtract{},
	} {
		typ := reflect.TypeOf(schema)
		for i := range typ.NumField() {
			field := typ.Field(i)
			name := field.Tag.Get("json")
			if name == "" {
				t.Errorf("%s.%s has no json tag", typ.Name(), field.Name)
				continue
			}
			doc := field.Tag.Get("jsonschema")

			if strings.HasSuffix(name, "_cents") && !strings.Contains(doc, "no decimal point") {
				t.Errorf("%s.%s is a cents field and does not say 'no decimal point'", typ.Name(), name)
			}
			if strings.HasSuffix(name, "_iso") && !strings.Contains(doc, "YYYY-MM-DD") {
				t.Errorf("%s.%s is a date field and does not say YYYY-MM-DD", typ.Name(), name)
			}
		}
	}
}

// Every kind the classifier can answer with has to be a kind the
// ingest_proposals CHECK accepts. The two lists are in different languages and
// nothing but this keeps them in step.
func TestTheClassifierKindsAreTheOnesTheColumnAccepts(t *testing.T) {
	field, ok := reflect.TypeOf(Classification{}).FieldByName("Kind")
	if !ok {
		t.Fatal("Classification has no Kind field")
	}
	tag := field.Tag.Get("jsonschema")

	// migrations/0005_proposals.sql, ingest_proposals.kind.
	want := []string{"receipt", "lease", "insurance", "mortgage_statement",
		"repair", "valuation", "note", "unknown"}
	for _, kind := range want {
		if !strings.Contains(tag, kind) {
			t.Errorf("the schema does not offer %q, which the column accepts", kind)
		}
	}
}

// An enclosure goes inline as a data URI. A receipt photograph goes as an
// image so the model reads it as one; a lease PDF goes as a file with its
// media type and its name.
func TestAnEnclosureBecomesADataURI(t *testing.T) {
	in := Input{
		Text: "Fwd: your receipt",
		Files: []File{
			{Filename: "receipt.png", MediaType: "image/png", Bytes: []byte{0x89, 'P'}},
			{Filename: "lease.pdf", MediaType: "application/pdf", Bytes: []byte("%PDF-")},
		},
	}

	msg := message(in)
	if msg.Role != provider.RoleUser {
		t.Fatalf("role = %q, want user", msg.Role)
	}
	if len(msg.Content) != 3 {
		t.Fatalf("parts = %d, want the text and both enclosures", len(msg.Content))
	}

	// The instruction's subject comes before what was attached to it.
	if msg.Content[0].Type != provider.PartText {
		t.Fatalf("first part = %q, want the text", msg.Content[0].Type)
	}
	if img := msg.Content[1]; img.Type != provider.PartImage ||
		!strings.HasPrefix(img.URL, "data:image/png;base64,") {
		t.Fatalf("image part = %+v, want an inline image data URI", img)
	}
	if file := msg.Content[2]; file.Type != provider.PartFile ||
		!strings.HasPrefix(file.URL, "data:application/pdf;base64,") ||
		file.Filename != "lease.pdf" {
		t.Fatalf("file part = %+v, want an inline pdf data URI carrying its name", file)
	}
}

// An email with an empty body and no readable enclosure is a real thing to
// receive. It should come back classified 'unknown' rather than as a transport
// error, and every provider rejects a message with no content at all.
func TestAnEmptyMessageStillHasContent(t *testing.T) {
	msg := message(Input{})
	if len(msg.Content) != 1 || msg.Content[0].Type != provider.PartText || msg.Content[0].Text == "" {
		t.Fatalf("parts = %+v, want one non-empty text part", msg.Content)
	}
}

// The extraction structs are what a proposal's payload holds, and the review
// screen reads that JSON back. A field that does not survive the round trip is
// a field the operator cannot correct.
func TestAnExtractSurvivesThePayloadRoundTrip(t *testing.T) {
	want := ReceiptExtract{
		VendorName: "Home Depot",
		DateISO:    "2026-08-04",
		TotalCents: 48219,
		Category:   "repair",
		LineItems:  []LineItem{{Description: "Ball valve", AmountCents: 1899}},
	}

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got ReceiptExtract
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}
