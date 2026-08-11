package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// maxBodyBytes caps a request body. Nothing this API accepts is large — the
// documents of M2 arrive as uploads through their own handler — so a body past
// this is either a mistake or an attempt to exhaust memory.
const maxBodyBytes = 1 << 20 // 1 MiB

// decodeJSON reads the request body into dst, reporting failure to the client
// and returning false. The caller stops on false.
//
// Unknown fields are rejected. A client that sends "nickmame" has a bug, and
// silently dropping the value would leave the operator staring at a save that
// reported success and changed nothing.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		WriteProblem(w, r, http.StatusBadRequest, describeDecodeError(err))
		return false
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		WriteProblem(w, r, http.StatusBadRequest, "The request body must be a single JSON object.")
		return false
	}
	return true
}

// describeDecodeError turns a decoder error into something an operator can act
// on, without leaking the shape of the server's structs beyond the field name
// the client already sent.
func describeDecodeError(err error) string {
	var syntax *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	var tooLarge *http.MaxBytesError

	switch {
	case errors.As(err, &syntax):
		return "The request body is not valid JSON (at byte " +
			strconv.FormatInt(syntax.Offset, 10) + ")."
	case errors.As(err, &typeErr):
		if typeErr.Field != "" {
			return "The field " + strconv.Quote(typeErr.Field) + " is not a " + typeErr.Type.String() + "."
		}
		return "A field in the request body has the wrong type."
	case errors.As(err, &tooLarge):
		return "The request body is too large."
	case errors.Is(err, io.EOF):
		return "The request body is empty."
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		return "The request body has an unrecognised field: " +
			strings.TrimPrefix(err.Error(), "json: unknown field ") + "."
	}
	return "The request body could not be read."
}

// pathID reads a numeric path parameter, reporting a bad one and returning
// false. A non-numeric id is a 404 rather than a 400: /properties/banana names
// no property, and saying so is enough.
func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id < 1 {
		WriteProblem(w, r, http.StatusNotFound, "No such record.")
		return 0, false
	}
	return id, true
}

// optionalID reads a numeric form field that may be absent. It is the multipart
// counterpart of pathID: an upload names what it is filed against in the form
// rather than in the path.
func optionalID(w http.ResponseWriter, r *http.Request, raw, name string) (*int64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, true
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		WriteProblem(w, r, http.StatusUnprocessableEntity, name+" has to name a record.")
		return nil, false
	}
	return &id, true
}
