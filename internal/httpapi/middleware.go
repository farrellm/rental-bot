package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/farrellm/rental-bot/internal/alert"
)

// contextKey scopes this package's context values.
type contextKey int

const (
	requestIDKey contextKey = iota
	loggerKey
)

// requestIDHeader is both read and written, so a reverse proxy that already
// assigns one keeps its value and the two logs correlate.
const requestIDHeader = "X-Request-Id"

// RequestIDFrom returns the request ID carried on ctx, or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// loggerFrom returns the request-scoped logger, falling back to the default.
func loggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// withRequestID assigns each request an ID and hands every downstream log
// line a logger already carrying it. Ingestion later correlates on the Gmail
// message ID the same way (docs/DESIGN.md §9.3).
func withRequestID(logger *slog.Logger, next http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)

		ctx := context.WithValue(r.Context(), requestIDKey, id)
		ctx = context.WithValue(ctx, loggerKey, logger.With("request_id", id))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// withAccessLog records one line per request, after it completes.
func withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		level := slog.LevelInfo
		if rec.status >= http.StatusInternalServerError {
			level = slog.LevelError
		}
		loggerFrom(r.Context()).LogAttrs(r.Context(), level, "request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Int64("bytes", rec.bytes),
			slog.Duration("duration", time.Since(start)),
		)
	})
}

// withRecover turns a panic into a 500 rather than a dropped connection, keeps
// the process up, and says so out loud.
//
// A recovered panic is critical: the request is lost, the operator has no
// reason to look, and it is the one class of failure that leaves no other
// trace on any screen in this application.
func (s *server) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			loggerFrom(r.Context()).Error("panic serving request",
				"panic", v,
				"method", r.Method,
				"path", r.URL.Path,
				"stack", string(debug.Stack()),
			)
			// Keyed by route rather than by occurrence: one broken handler hit
			// by a refreshing browser is one condition, and the cooldown is
			// what stops it being forty messages. The stack stays in the log —
			// §8.6 keeps an alert body short and pointed at the app.
			alert.Publish(r.Context(), s.alerts, alert.Alert{
				Key:      "http.panic." + r.Method + " " + r.Pattern,
				Severity: alert.Critical,
				Title:    "A request crashed the handler serving it",
				Detail: alert.Errorf("%s %s panicked: %v. The request was lost; the stack is in the log.",
					r.Method, r.URL.Path, v),
			})
			WriteProblem(w, r, http.StatusInternalServerError, "The server could not complete the request.")
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder remembers what the handler wrote, for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	bytes   int64
	written bool
}

func (rec *statusRecorder) WriteHeader(status int) {
	if rec.written {
		return
	}
	rec.status = status
	rec.written = true
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *statusRecorder) Write(b []byte) (int, error) {
	rec.written = true
	n, err := rec.ResponseWriter.Write(b)
	rec.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (rec *statusRecorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}
