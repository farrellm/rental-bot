package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// patch is a decoded PATCH body: the fields the client actually sent, still
// unparsed.
//
// A PATCH field has three states, and all three mean different things:
//
//	absent  leave the column alone
//	null    clear the column
//	value   set the column
//
// A plain struct collapses the first two — an omitted int64 and a null one
// both arrive as the zero value — which is why the body is decoded into raw
// messages first and applied field by field. Everything downstream of here
// depends on that distinction surviving: "the purchase price is unknown" and
// "the purchase price is zero" are not the same claim about a property.
type patch map[string]json.RawMessage

// decodePatch reads a PATCH body, rejecting any field the caller does not
// know. Silently ignoring a misspelled field would report a save that changed
// nothing as a success.
func decodePatch(w http.ResponseWriter, r *http.Request, known ...string) (patch, bool) {
	var p patch
	if !decodeJSON(w, r, &p) {
		return nil, false
	}

	allowed := make(map[string]struct{}, len(known))
	for _, name := range known {
		allowed[name] = struct{}{}
	}

	var unknown []string
	for name := range p {
		if _, ok := allowed[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		WriteProblem(w, r, http.StatusBadRequest,
			"The request body has fields this endpoint does not accept: "+
				strings.Join(unknown, ", ")+".")
		return nil, false
	}

	if len(p) == 0 {
		WriteProblem(w, r, http.StatusBadRequest, "The request body changes nothing.")
		return nil, false
	}
	return p, true
}

// binding pairs a field name with the column the merge writes it into.
//
// A PATCH applier is a list of these and nothing else, which is what makes it
// readable as the table it is: the wire name on the left, the column on the
// right. Building the list with required and nullable keeps the three-state
// distinction at the point where the field is named, rather than in a
// convention the reader has to hold in their head.
type binding struct {
	name string
	dst  any
	// null reports whether the field can be cleared. A required field's null is
	// a client error; a nullable one's is the instruction to clear the column.
	null bool
}

// required binds a field that has no null state. dst is a *T.
func required(name string, dst any) binding { return binding{name: name, dst: dst} }

// nullable binds a field that can be cleared. dst is a **T, so unmarshalling
// null sets the pointer to nil and the column follows.
func nullable(name string, dst any) binding { return binding{name: name, dst: dst, null: true} }

// apply merges every bound field the client sent, in order, and reports the
// first one it could not.
//
// The failure comes back as a validationError so a caller inside a write
// transaction can return it and have the handler answer 422.
func (p patch) apply(bindings ...binding) error {
	for _, b := range bindings {
		var err error
		if b.null {
			err = p.nullable(b.name, b.dst)
		} else {
			err = p.required(b.name, b.dst)
		}
		if err != nil {
			return validationError{err.Error()}
		}
	}
	return nil
}

// required applies a field that has no null state. dst is a *T.
func (p patch) required(name string, dst any) error {
	raw, ok := p[name]
	if !ok {
		return nil
	}
	if isNull(raw) {
		return fmt.Errorf("%s cannot be null", name)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("%s is not the right type", name)
	}
	return nil
}

// nullable applies a field that can be cleared. dst is a **T, so unmarshalling
// null sets the pointer to nil and the column follows.
func (p patch) nullable(name string, dst any) error {
	raw, ok := p[name]
	if !ok {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("%s is not the right type", name)
	}
	return nil
}

// isNull reports whether a raw message is the JSON literal null.
func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
