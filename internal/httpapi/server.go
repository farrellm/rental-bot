// Package httpapi serves the JSON API, the health endpoints, and the
// embedded single-page app.
package httpapi

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/farrellm/rental-bot/internal/auth"
	"github.com/farrellm/rental-bot/internal/blob"
	"github.com/farrellm/rental-bot/internal/config"
	"github.com/farrellm/rental-bot/internal/jobs"
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

// Options configures a server. Later milestones add fields here — the job
// queue, the alert bus — rather than growing New's parameter list.
type Options struct {
	Config config.Config
	DB     Health
	// Repo is the query layer the CRUD handlers read and write through.
	Repo *store.Repo
	// Blobs holds document content. A nil Blobs fails the document routes with
	// a 503 rather than panicking on the first upload.
	Blobs *blob.Store
	// Guard authenticates requests. A nil Guard fails every guarded route
	// closed with a 503 rather than leaving the API open.
	Guard *auth.Guard
	// Queue takes the work a request cannot do inside its own deadline: a
	// Pub/Sub push enqueues a sync and answers immediately. A nil Queue fails
	// those routes with a 503.
	Queue *jobs.Queue
	// Runner is notified after an enqueue so the work starts now rather than
	// at the pool's next poll. It may be nil.
	Runner *jobs.Runner
	// Limiter throttles sign-in attempts. A nil Limiter gets a fresh one.
	Limiter *auth.Limiter
	Logger  *slog.Logger
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
	repo      *store.Repo
	blobs     *blob.Store
	guard     *auth.Guard
	queue     *jobs.Queue
	runner    *jobs.Runner
	limiter   *auth.Limiter
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
	if opts.Limiter == nil {
		opts.Limiter = auth.NewLimiter()
	}

	s := &server{
		cfg:       opts.Config,
		db:        opts.DB,
		repo:      opts.Repo,
		blobs:     opts.Blobs,
		guard:     opts.Guard,
		queue:     opts.Queue,
		runner:    opts.Runner,
		limiter:   opts.Limiter,
		log:       opts.Logger,
		spa:       opts.SPA,
		startedAt: opts.StartedAt,
	}

	mux := http.NewServeMux()

	// Open on purpose: a process manager has no session, and a liveness probe
	// that needs one is a liveness probe that reports the wrong thing.
	get(mux, "/healthz", s.handleHealthz)
	get(mux, "/readyz", s.handleReadyz)

	// Signing in is necessarily open. Everything else under /api/v1 is not,
	// including the status endpoint, which was open through M0 only because
	// there was no session middleware to put it behind.
	route(mux, "/api/v1/auth/login", methods{http.MethodPost: s.handleLogin})
	route(mux, "/api/v1/auth/logout", methods{http.MethodPost: s.guarded(s.handleLogout)})
	route(mux, "/api/v1/auth/me", methods{http.MethodGet: s.guarded(s.handleMe)})
	route(mux, "/api/v1/status", methods{http.MethodGet: s.guarded(s.handleStatus)})

	s.routeProperties(mux)
	s.routeDocuments(mux)
	s.routeTransactions(mux)
	s.routeVendors(mux)
	s.routeRepairs(mux)
	s.routeLeases(mux)
	s.routeTenants(mux)

	// Anything else under /api/ is a client mistake, and it gets a
	// problem+json 404 rather than the SPA's index.html.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		WriteProblem(w, r, http.StatusNotFound, "No such endpoint.")
	})
	mux.Handle("/", s.spaHandler())

	return withRequestID(opts.Logger, withAccessLog(withRecover(mux)))
}

// guarded wraps a handler so it is reachable only with a live session, and
// only with a valid CSRF token when it mutates.
//
// A missing guard fails closed. Nothing in cmd builds a server without one, so
// reaching the 503 means the wiring is wrong — which is a far better outcome
// than an API that quietly serves the ledger to anyone who asks.
func (s *server) guarded(h http.HandlerFunc) http.HandlerFunc {
	if s.guard == nil {
		return func(w http.ResponseWriter, r *http.Request) {
			WriteProblem(w, r, http.StatusServiceUnavailable, "Authentication is not configured.")
		}
	}
	return s.guard.RequireSession(h).ServeHTTP
}

// methods maps an HTTP method to the handler for one path.
type methods map[string]http.HandlerFunc

// route registers every method a path answers, and answers the rest with a
// problem+json 405.
//
// net/http would produce that 405 on its own from a method pattern, but as
// plain text — and every error in this API is RFC 7807, 405s included.
func route(mux *http.ServeMux, pattern string, m methods) {
	allow := allowHeader(m)
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		h, ok := m[r.Method]
		if !ok && r.Method == http.MethodHead {
			// HEAD is GET without a body; net/http discards the body for us.
			h, ok = m[http.MethodGet]
		}
		if !ok {
			w.Header().Set("Allow", allow)
			WriteProblem(w, r, http.StatusMethodNotAllowed, "This endpoint answers "+allow+".")
			return
		}
		h(w, r)
	})
}

// get registers a read-only route.
func get(mux *http.ServeMux, pattern string, h http.HandlerFunc) {
	route(mux, pattern, methods{http.MethodGet: h})
}

// allowHeader renders the Allow header for a route, in a stable order so the
// value does not change between restarts.
func allowHeader(m methods) string {
	names := make([]string, 0, len(m)+1)
	for name := range m {
		names = append(names, name)
		if name == http.MethodGet {
			names = append(names, http.MethodHead)
		}
	}
	sort.Slice(names, func(i, j int) bool { return rank(names[i]) < rank(names[j]) })
	return strings.Join(names, ", ")
}

// rank orders methods the way a person reads them rather than alphabetically:
// what you can read, then what you can do.
func rank(method string) int {
	order := []string{
		http.MethodGet, http.MethodHead, http.MethodPost,
		http.MethodPatch, http.MethodPut, http.MethodDelete,
	}
	for i, m := range order {
		if m == method {
			return i
		}
	}
	return len(order)
}
