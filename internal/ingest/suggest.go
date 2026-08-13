package ingest

import (
	"encoding/json"
	"strings"

	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// Suggestion is a property the portfolio does not hold, typed up from what a
// document said.
//
// The matcher refuses to guess, and the refusal is right: a roof filed against
// the wrong building is worse than a proposal that waits. But "no property
// matched" is a dead end on the review screen, and the first document about a
// building is exactly the moment that building should join the portfolio. The
// address is already on the page; this is it in the fields the create path
// takes.
//
// Nothing here is authoritative. It is a draft for a person to countersign,
// which is why it carries Source: the operator should be able to see what it
// was read off before agreeing to it.
type Suggestion struct {
	Nickname     string
	AddressLine1 string
	City         string
	State        string
	PostalCode   string
	// Source is the address string this was read off, verbatim.
	Source string
}

// SuggestProperty reads a new property off a proposal nothing matched, or
// reports that there is nothing to suggest.
//
// The gate is the matcher's own verdict, recomputed here for the same reason
// extract.go recomputes it: a stored outcome would be a second copy of
// something the addresses already say. Only NoMatch earns a suggestion.
// Ambiguous does not -- two buildings fit equally well, so the answer is for
// the operator to pick one, not for the record to grow a third. NoAddress does
// not either, because there is nothing to type up.
//
// It is deliberately indifferent to the kind. "The document names an address
// and no building folds to it" is true whether the enclosure is a receipt or
// something nobody could classify.
func SuggestProperty(candidates []sqlc.ListPropertyMatchKeysRow, payload, hint string) *Suggestion {
	source := strings.TrimSpace(payloadAddress(payload))
	if source == "" {
		source = strings.TrimSpace(hint)
	}
	if source == "" {
		return nil
	}

	if _, outcome := MatchProperty(candidates, source); outcome != NoMatch {
		return nil
	}

	parsed := domain.ParseFreeAddress(source)

	// The extract carries no nickname -- a document does not know what you
	// call the building. The street line is the honest stand-in, and it is
	// shown in carbon like every other value a machine typed, so renaming it
	// is one field rather than a decision.
	nickname := parsed.Line1
	if nickname == "" {
		nickname = source
	}

	return &Suggestion{
		Nickname:     nickname,
		AddressLine1: parsed.Line1,
		City:         parsed.City,
		State:        parsed.State,
		PostalCode:   parsed.Postal,
		Source:       source,
	}
}

// payloadAddress reads the address field every extract carries out of the
// stored payload.
//
// The typed structs are gone by the time a proposal is read back, so this
// takes the one field it wants rather than dispatching over four shapes the
// way addressGuess does. A payload that will not parse is not a fault to
// report: the caller falls back to the classifier's hint, which is what an
// unextracted proposal has anyway.
func payloadAddress(payload string) string {
	var v struct {
		AddressGuess string `json:"address_guess"`
	}
	if err := json.Unmarshal([]byte(payload), &v); err != nil {
		return ""
	}
	return v.AddressGuess
}
