package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/farrellm/rental-bot/internal/store"
)

// Problem is an RFC 7807 error body.
//
// Every error this API returns has this shape, from M0 onward, so clients
// never have to guess which of three error formats a given endpoint speaks.
type Problem struct {
	// Type is a URI identifying the problem kind. "about:blank" means the
	// status code is the whole story.
	Type string `json:"type"`
	// Title is a short, stable summary — safe to show a user.
	Title string `json:"title"`
	// Status repeats the HTTP status code.
	Status int `json:"status"`
	// Detail explains this particular occurrence.
	Detail string `json:"detail,omitempty"`
	// Instance identifies the request, for correlating with the logs.
	Instance string `json:"instance,omitempty"`
}

// WriteProblem sends an RFC 7807 response.
func WriteProblem(w http.ResponseWriter, r *http.Request, status int, detail string) {
	p := Problem{
		Type:     "about:blank",
		Title:    http.StatusText(status),
		Status:   status,
		Detail:   detail,
		Instance: RequestIDFrom(r.Context()),
	}

	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(p); err != nil {
		slog.Default().Error("write problem response", "error", err)
	}
}

// record names one kind of row for the two registers an error is written in.
//
// Every entity in this API answers a failed read and a failed write the same
// way, differing only in the word. Naming the word once per entity is what lets
// the answering live here rather than being written out again in each handler
// file, which is where eight near-identical copies of it used to be.
type record struct {
	// noun is what the operator reads: "lease", "entry", "message".
	noun string
	// table is the word for the log, when it differs from the noun. A
	// transactions row is "the entry" on a ledger card and a transaction
	// everywhere a developer looks, and both readers should get their own word.
	table string
}

// logged is the word for a log line.
func (rec record) logged() string {
	if rec.table != "" {
		return rec.table
	}
	return rec.noun
}

// readError answers a failed read of one row: absent is a 404, anything else is
// a 500 with the reason in the log rather than in the response.
func (s *server) readError(w http.ResponseWriter, r *http.Request, rec record, err error) {
	if store.NotFound(err) {
		WriteProblem(w, r, http.StatusNotFound, "No such "+rec.noun+".")
		return
	}
	loggerFrom(r.Context()).Error("read "+rec.logged(), "error", err)
	WriteProblem(w, r, http.StatusInternalServerError, "Could not read the "+rec.noun+".")
}

// writeError answers a failed write, handling the three cases every entity
// shares: a validation message raised inside the write transaction, a row that
// is not there, and everything else.
//
// An entity with a conflict of its own -- two units labelled the same, two
// leases covering one unit -- handles that case in its own helper and delegates
// the rest here. The predicates are disjoint, so the order between the two is
// free.
func (s *server) writeError(w http.ResponseWriter, r *http.Request, rec record, err error) {
	var invalid validationError
	switch {
	case errors.As(err, &invalid):
		WriteProblem(w, r, http.StatusUnprocessableEntity, invalid.detail)
	case store.NotFound(err):
		WriteProblem(w, r, http.StatusNotFound, "No such "+rec.noun+".")
	default:
		loggerFrom(r.Context()).Error("write "+rec.logged(), "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not save the "+rec.noun+".")
	}
}

// danglingReference answers a write that names a row which is not there.
//
// It is a 422 rather than a 500 because the caller named it: a lease against a
// unit that was deleted is a bad request, and SQLite refusing it is the
// foreign key doing its job.
func (s *server) danglingReference(w http.ResponseWriter, r *http.Request, rec record) {
	WriteProblem(w, r, http.StatusUnprocessableEntity,
		"One of the records this "+rec.noun+" points at does not exist.")
}

// writeJSON sends a successful JSON response.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		loggerFrom(r.Context()).Error("encode response", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "The response could not be encoded.")
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		loggerFrom(r.Context()).Debug("write response", "error", err)
	}
}
