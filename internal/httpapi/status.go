package httpapi

import (
	"net/http"
	"time"

	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/version"
)

// Status is everything the record-of-service card renders.
//
// Note for M1: this endpoint is unauthenticated, because M0 has no auth. It
// exposes build identity and schema state only, on a single-operator host,
// but it moves behind the session middleware as soon as there is one.
type Status struct {
	// Status is "operational" or "degraded" — the word the card stamps.
	Status        string            `json:"status"`
	Version       string            `json:"version"`
	Commit        string            `json:"commit"`
	BuildDate     string            `json:"build_date"`
	GoVersion     string            `json:"go_version"`
	StartedAt     string            `json:"started_at"`
	UptimeSeconds int64             `json:"uptime_seconds"`
	SchemaVersion int               `json:"schema_version"`
	Database      string            `json:"database"`
	Checks        []Check           `json:"checks"`
	Migrations    []store.Migration `json:"migrations"`
	CheckedAt     string            `json:"checked_at"`
}

// handleStatus answers with the current state of the process.
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	checks := s.runChecks(ctx)

	st := Status{
		Status:        readinessWord(checks),
		Version:       version.Version,
		Commit:        version.Commit,
		BuildDate:     version.BuildDate,
		GoVersion:     version.GoVersion(),
		StartedAt:     domain.Stamp(s.startedAt),
		UptimeSeconds: int64(time.Since(s.startedAt).Seconds()),
		Database:      s.db.Path(),
		Checks:        checks,
		Migrations:    []store.Migration{},
		CheckedAt:     timestamp(),
	}

	// A degraded database is reported, not fatal: the card should still
	// show the build identity and uptime it already knows.
	if v, err := s.db.SchemaVersion(ctx); err == nil {
		st.SchemaVersion = v
	}
	if applied, err := s.db.Applied(ctx); err != nil {
		loggerFrom(ctx).Error("read applied migrations", "error", err)
	} else if applied != nil {
		st.Migrations = applied
	}

	// The card polls this; a cached answer would show a stopped database
	// as healthy.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, st)
}
