package ingest

import (
	"testing"

	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// property builds a match candidate the way the store hands one over.
func property(id int64, nickname, line1, city, state, postal string) sqlc.ListPropertyMatchKeysRow {
	return sqlc.ListPropertyMatchKeysRow{
		ID:                id,
		Nickname:          nickname,
		AddressLine1:      line1,
		NormalizedAddress: domain.NormalizeAddress(line1, "", city, state, postal),
	}
}

func TestMatchProperty(t *testing.T) {
	elm := property(1, "Elm Street Duplex", "412 Elm St", "Athens", "OH", "45701")
	oak := property(2, "Oak Ridge", "88 Oak Ridge Rd", "Athens", "OH", "45701")
	// Same street number and name, different town. This is what makes the
	// street-only tier ambiguous rather than a guess.
	elmTwo := property(3, "Elm Street, Columbus", "412 Elm St", "Columbus", "OH", "43215")

	tests := []struct {
		name       string
		candidates []sqlc.ListPropertyMatchKeysRow
		extracted  string
		wantID     int64
		wantResult Outcome
	}{
		{
			name:       "the folded addresses agree in full",
			candidates: []sqlc.ListPropertyMatchKeysRow{elm, oak},
			extracted:  "412 Elm Street, Athens, Ohio 45701",
			wantID:     1,
			wantResult: MatchedExactly,
		},
		{
			// The unit lives in the units table. An invoice addressed to a door
			// is still an invoice about the building.
			name:       "a unit designator does not stop a match",
			candidates: []sqlc.ListPropertyMatchKeysRow{elm, oak},
			extracted:  "412 Elm St Apt 2, Athens, OH 45701",
			wantID:     1,
			wantResult: MatchedExactly,
		},
		{
			name:       "a receipt that names only the street",
			candidates: []sqlc.ListPropertyMatchKeysRow{elm, oak},
			extracted:  "412 Elm St",
			wantID:     1,
			wantResult: MatchedOnStreet,
		},
		{
			// The cost of a miss is a proposal that waits for a person. The
			// cost of a wrong match is a roof filed against the wrong building.
			name:       "one street, two towns",
			candidates: []sqlc.ListPropertyMatchKeysRow{elm, elmTwo},
			extracted:  "412 Elm St",
			wantResult: Ambiguous,
		},
		{
			name:       "a town names which of the two",
			candidates: []sqlc.ListPropertyMatchKeysRow{elm, elmTwo},
			extracted:  "412 Elm St, Columbus, OH 43215",
			wantID:     3,
			wantResult: MatchedExactly,
		},
		{
			name:       "a transposed digit is still the same building",
			candidates: []sqlc.ListPropertyMatchKeysRow{elm, oak},
			extracted:  "412 Eml Street, Athens, Ohio 45701",
			wantID:     1,
			wantResult: MatchedApproximately,
		},
		{
			name:       "an address the portfolio does not hold",
			candidates: []sqlc.ListPropertyMatchKeysRow{elm, oak},
			extracted:  "9 Beacon Hill Way, Boston, MA 02108",
			wantResult: NoMatch,
		},
		{
			name:       "a document that names no address",
			candidates: []sqlc.ListPropertyMatchKeysRow{elm, oak},
			extracted:  "",
			wantResult: NoAddress,
		},
		{
			name:       "an empty portfolio",
			candidates: nil,
			extracted:  "412 Elm St, Athens, OH 45701",
			wantResult: NoMatch,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, outcome := MatchProperty(tt.candidates, tt.extracted)
			if outcome != tt.wantResult {
				t.Fatalf("outcome = %q, want %q", outcome, tt.wantResult)
			}
			switch {
			case tt.wantID == 0 && id != nil:
				t.Fatalf("matched property %d, want none", *id)
			case tt.wantID != 0 && id == nil:
				t.Fatalf("matched nothing, want property %d", tt.wantID)
			case tt.wantID != 0 && *id != tt.wantID:
				t.Fatalf("matched property %d, want %d", *id, tt.wantID)
			}
		})
	}
}

// Auto-apply's third condition is the strict one. A street-only match is a
// real match and a fine thing to show an operator; it is not grounds for
// putting money in the ledger with nobody looking.
func TestOnlyAnExactMatchIsGoodEnoughToFileUnread(t *testing.T) {
	tests := []struct {
		outcome Outcome
		matched bool
		enough  bool
	}{
		{MatchedExactly, true, true},
		{MatchedOnStreet, true, false},
		{MatchedApproximately, true, false},
		{Ambiguous, false, false},
		{NoAddress, false, false},
		{NoMatch, false, false},
	}
	for _, tt := range tests {
		if got := tt.outcome.Matched(); got != tt.matched {
			t.Errorf("%q.Matched() = %v, want %v", tt.outcome, got, tt.matched)
		}
		if got := tt.outcome.Unambiguous(); got != tt.enough {
			t.Errorf("%q.Unambiguous() = %v, want %v", tt.outcome, got, tt.enough)
		}
	}
}

// The folding is what both sides of every comparison go through, so a change
// to how a free-text address is split is a change to what matches what.
func TestParseFreeAddress(t *testing.T) {
	tests := []struct {
		in                         string
		line1, city, state, postal string
	}{
		{"412 Elm St, Athens, OH 45701", "412 Elm St", "Athens", "OH", "45701"},
		// The region comes back tokenized, because recognising it meant
		// tokenizing it. Every field here feeds NormalizeAddress, which
		// uppercases anyway; none of them is ever rendered.
		{"412 Elm St, Athens, Ohio", "412 Elm St", "Athens", "OHIO", ""},
		{"412 Elm St, Athens", "412 Elm St", "Athens", "", ""},
		{"412 Elm St", "412 Elm St", "", "", ""},
		{"412 Elm St, Apt 2, Athens, OH 45701", "412 Elm St", "Apt 2 Athens", "OH", "45701"},
		{"", "", "", "", ""},
	}
	for _, tt := range tests {
		got := domain.ParseFreeAddress(tt.in)
		if got.Line1 != tt.line1 || got.City != tt.city || got.State != tt.state || got.Postal != tt.postal {
			t.Errorf("ParseFreeAddress(%q) = %+v, want {%q %q %q %q}",
				tt.in, got, tt.line1, tt.city, tt.state, tt.postal)
		}
	}
}
