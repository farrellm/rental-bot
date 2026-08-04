package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
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
