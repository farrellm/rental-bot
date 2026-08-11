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
