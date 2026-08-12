package ingest

import (
	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// Outcome is what the matcher concluded, in the words the review screen uses.
type Outcome string

const (
	// MatchedExactly means the folded addresses agree in full.
	MatchedExactly Outcome = "exact"
	// MatchedOnStreet means one side named a town and the other did not, and
	// the house number and street name agree.
	MatchedOnStreet Outcome = "street"
	// MatchedApproximately means the folds are close but not equal, which is
	// what a typo or a bad OCR pass produces.
	MatchedApproximately Outcome = "approximate"
	// Ambiguous means more than one property fits equally well. It is not a
	// failure of the model and not a near miss: it is two buildings the record
	// cannot tell apart from what the document says.
	Ambiguous Outcome = "ambiguous"
	// NoAddress means the document named none.
	NoAddress Outcome = "none"
	// NoMatch means it named one and the portfolio does not hold it.
	NoMatch Outcome = "unmatched"
)

// Matched reports whether the outcome carries a property.
func (o Outcome) Matched() bool {
	switch o {
	case MatchedExactly, MatchedOnStreet, MatchedApproximately:
		return true
	}
	return false
}

// Unambiguous reports whether §5.4's third auto-apply condition is met.
//
// Only an exact fold counts. A street-only match is a real match and a fine
// thing to show an operator, but "the model named a street and only one of
// your buildings is on it" is a weaker claim than the one that should let a
// receipt into the ledger with nobody looking.
func (o Outcome) Unambiguous() bool { return o == MatchedExactly }

// similarityFloor is how close two folded addresses have to be before they are
// treated as the same one.
//
// High on purpose. The cost of a miss is a proposal that waits for a person,
// which is the normal path anyway; the cost of a wrong match is a roof
// replacement filed against the wrong building, which is the kind of error
// that makes every number on the dashboard suspect.
const similarityFloor = 0.88

// MatchProperty resolves a model's address string to a property, or refuses to.
//
// This is deterministic Go and never the model's job (docs/DESIGN.md §5.3).
// The model returns a string; this folds it with the same function that
// produced properties.normalized_address and compares. Three tiers are tried
// in order, and within a tier more than one candidate is Ambiguous rather than
// a guess -- ambiguity routes to review, always.
func MatchProperty(candidates []sqlc.ListPropertyMatchKeysRow, extracted string) (*int64, Outcome) {
	address := domain.ParseFreeAddress(extracted)
	key, street := address.Key(), address.Street()
	if key == "" && street == "" {
		return nil, NoAddress
	}

	// Tier one: the folds agree in full.
	if id, outcome := only(candidates, func(c sqlc.ListPropertyMatchKeysRow) bool {
		return key != "" && c.NormalizedAddress == key
	}, MatchedExactly); outcome != NoMatch {
		return id, outcome
	}

	// Tier two: the street lines agree. This is the common case, because a
	// receipt says "412 Elm St" and the record says where that is.
	if street != "" {
		if id, outcome := only(candidates, func(c sqlc.ListPropertyMatchKeysRow) bool {
			return streetKey(c) == street
		}, MatchedOnStreet); outcome != NoMatch {
			return id, outcome
		}
	}

	// Tier three: close enough that the difference is noise rather than a
	// different building.
	best, ties, bestID := 0.0, 0, int64(0)
	for _, c := range candidates {
		score := max(similarity(key, c.NormalizedAddress), similarity(street, streetKey(c)))
		switch {
		case score > best:
			best, ties, bestID = score, 1, c.ID
		case score == best:
			ties++
		}
	}
	if best < similarityFloor {
		return nil, NoMatch
	}
	if ties > 1 {
		return nil, Ambiguous
	}
	return &bestID, MatchedApproximately
}

// streetKey folds a stored property's street line the way the extracted one is
// folded, so both sides of the comparison went through one function.
func streetKey(c sqlc.ListPropertyMatchKeysRow) string {
	return domain.NormalizeAddress(c.AddressLine1, c.AddressLine2, "", "", "")
}

// only returns the single candidate matching pred, or reports why it cannot.
func only(candidates []sqlc.ListPropertyMatchKeysRow, pred func(sqlc.ListPropertyMatchKeysRow) bool, hit Outcome) (*int64, Outcome) {
	var found *int64
	for _, c := range candidates {
		if !pred(c) {
			continue
		}
		if found != nil {
			return nil, Ambiguous
		}
		found = &c.ID
	}
	if found == nil {
		return nil, NoMatch
	}
	return found, hit
}

// similarity is 1 for identical strings and 0 for entirely different ones.
func similarity(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	longest := max(len(a), len(b))
	return 1 - float64(distance(a, b))/float64(longest)
}

// distance is the Levenshtein edit distance between two strings.
//
// Two rows rather than a full matrix: the strings are addresses, the portfolio
// is tens of properties, and this runs once per ingested document. It is here
// rather than behind a dependency because it is fifteen lines and a dependency
// is forever.
func distance(a, b string) int {
	if len(a) < len(b) {
		a, b = b, a
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}

	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}
