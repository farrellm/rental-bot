package ingest

import (
	"testing"

	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

func TestSuggestProperty(t *testing.T) {
	elm := property(1, "Elm Street Duplex", "412 Elm St", "Athens", "OH", "45701")
	elmTwo := property(3, "Elm Street, Columbus", "412 Elm St", "Columbus", "OH", "43215")
	portfolio := []sqlc.ListPropertyMatchKeysRow{elm, elmTwo}

	tests := []struct {
		name       string
		candidates []sqlc.ListPropertyMatchKeysRow
		payload    string
		hint       string
		want       *Suggestion
	}{
		{
			name:       "nothing was read and nothing was hinted",
			candidates: portfolio,
			payload:    "{}",
		},
		{
			// The building is already on file. The picker is the answer here,
			// not a second record for the same roof.
			name:       "the address folds to a property that exists",
			candidates: portfolio,
			payload:    `{"address_guess":"412 Elm Street, Athens, Ohio 45701"}`,
		},
		{
			// Two buildings fit equally well. Creating a third is the one
			// answer that is certainly wrong.
			name:       "one street, two towns",
			candidates: portfolio,
			payload:    `{"address_guess":"412 Elm St"}`,
		},
		{
			name:       "a full address the portfolio does not hold",
			candidates: portfolio,
			payload:    `{"address_guess":"9 Sycamore Ln, Nelsonville, OH 45764"}`,
			want: &Suggestion{
				Nickname:     "9 Sycamore Ln",
				AddressLine1: "9 Sycamore Ln",
				City:         "Nelsonville",
				State:        "OH",
				PostalCode:   "45764",
				Source:       "9 Sycamore Ln, Nelsonville, OH 45764",
			},
		},
		{
			// A document that names only a street still names a building. The
			// town is blank rather than invented, and blank is a field the
			// operator fills in graphite.
			name:       "a street line and no town",
			candidates: portfolio,
			payload:    `{"address_guess":"9 Sycamore Ln"}`,
			want: &Suggestion{
				Nickname:     "9 Sycamore Ln",
				AddressLine1: "9 Sycamore Ln",
				Source:       "9 Sycamore Ln",
			},
		},
		{
			// The extract read the document; the hint read the covering email.
			// extract.go prefers the former and so does this.
			name:       "the extract's address beats the classifier's hint",
			candidates: portfolio,
			payload:    `{"address_guess":"9 Sycamore Ln"}`,
			hint:       "412 Elm St, Athens, OH 45701",
			want: &Suggestion{
				Nickname:     "9 Sycamore Ln",
				AddressLine1: "9 Sycamore Ln",
				Source:       "9 Sycamore Ln",
			},
		},
		{
			// A kind with no extractor never gets a payload, and the hint is
			// the only address on the row.
			name:       "an unextracted proposal falls back to the hint",
			candidates: portfolio,
			payload:    "{}",
			hint:       "9 Sycamore Ln, Nelsonville, OH 45764",
			want: &Suggestion{
				Nickname:     "9 Sycamore Ln",
				AddressLine1: "9 Sycamore Ln",
				City:         "Nelsonville",
				State:        "OH",
				PostalCode:   "45764",
				Source:       "9 Sycamore Ln, Nelsonville, OH 45764",
			},
		},
		{
			name:       "a payload that will not parse falls back to the hint",
			candidates: portfolio,
			payload:    "not json",
			hint:       "9 Sycamore Ln",
			want: &Suggestion{
				Nickname:     "9 Sycamore Ln",
				AddressLine1: "9 Sycamore Ln",
				Source:       "9 Sycamore Ln",
			},
		},
		{
			// Whitespace is not an address.
			name:       "a blank address guess falls back to the hint",
			candidates: portfolio,
			payload:    `{"address_guess":"   "}`,
			hint:       "  ",
		},
		{
			name:       "the first property of an empty portfolio",
			candidates: nil,
			payload:    `{"address_guess":"412 Elm St, Athens, OH 45701"}`,
			want: &Suggestion{
				Nickname:     "412 Elm St",
				AddressLine1: "412 Elm St",
				City:         "Athens",
				State:        "OH",
				PostalCode:   "45701",
				Source:       "412 Elm St, Athens, OH 45701",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SuggestProperty(tt.candidates, tt.payload, tt.hint)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("SuggestProperty() = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("SuggestProperty() = nil, want %+v", tt.want)
			}
			if *got != *tt.want {
				t.Errorf("SuggestProperty() = %+v, want %+v", *got, *tt.want)
			}
		})
	}
}
