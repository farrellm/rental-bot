// Package httpapi serves the JSON API, the health endpoints, and the
// embedded single-page app.
package httpapi

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/farrellm/rental-bot/internal/config"
	"github.com/farrellm/rental-bot/internal/store"
)

// Health is the slice of the store the health and status endpoints need.
// *store.DB satisfies it; tests substitute a database that is unwell.
type Health interface {
	Ping(ctx context.Context) error
	SchemaVersion(ctx context.Context) (int, error)
	Applied(ctx context.Context) ([]store.Migration, error)
	Path() string
}

// Options configures a server. Later milestones add fields here — sessions,
// the job queue, the alert bus — rather than growing New's parameter list.
type Options struct {
	Config config.Config
	DB     Health
	Logger *slog.Logger
	// SPA holds the built frontend, rooted at index.html. A nil SPA means
	// this binary was built without the `spa` tag and serves the API only;
	// see web/embed.go.
	SPA fs.FS
	// StartedAt seeds the reported uptime. Zero means now.
	StartedAt time.Time
}

// server holds what the handlers share.
type server struct {
	cfg       config.Config
	db        Health
	log       *slog.Logger
	spa       fs.FS
	startedAt time.Time
}

// New builds the HTTP handler for the whole application.
func New(opts Options) http.Handler {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.StartedAt.IsZero() {
		opts.StartedAt = time.Now()
	}

	s := &server{
		cfg:       opts.Config,
		db:        opts.DB,
		log:       opts.Logger,
		spa:       opts.SPA,
		startedAt: opts.StartedAt,
	}

	mux := http.NewServeMux()
	get(mux, "/healthz", s.handleHealthz)
	get(mux, "/readyz", s.handleReadyz)
	get(mux, "/api/v1/status", s.handleStatus)

	// Anything else under /api/ is a client mistake, and it gets a
	// problem+json 404 rather than the SPA's index.html.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		WriteProblem(w, r, http.StatusNotFound, "No such endpoint.")
	})
	mux.Handle("/", s.spaHandler())

	return withRequestID(opts.Logger, withAccessLog(withRecover(mux)))
}

// get registers a read-only route, answering anything else with a
// problem+json 405 so every error in this API has one shape.
func get(mux *http.ServeMux, pattern string, h http.HandlerFunc) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			WriteProblem(w, r, http.StatusMethodNotAllowed, "This endpoint only answers GET.")
			return
		}
		h(w, r)
	})
}
