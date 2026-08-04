package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// Check is one named readiness condition.
//
// A failing check says what is wrong and what to do about it. "not ready" on
// its own sends the operator to the logs; naming the file and the fix does
// not.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// checkTimeout bounds a readiness probe. A hung database should report as
// not ready quickly, not hold the probe open.
const checkTimeout = 3 * time.Second

// handleHealthz reports that the process is alive.
//
// It deliberately touches nothing else. Liveness answers "should this be
// restarted"; readiness answers "should this receive traffic", and conflating
// them turns a database blip into a restart loop.
func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz reports whether the process can serve requests, returning 503
// with the failing checks when it cannot.
func (s *server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	checks := s.runChecks(r.Context())

	status := http.StatusOK
	if !allOK(checks) {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, r, status, struct {
		Status string  `json:"status"`
		Checks []Check `json:"checks"`
	}{
		Status: readinessWord(checks),
		Checks: checks,
	})
}

// runChecks evaluates every readiness condition.
//
// M0 knows about the database. The Gmail token and last-sync recency checks
// (docs/DESIGN.md §9.3) join this list when M3 gives them something to read.
func (s *server) runChecks(ctx context.Context) []Check {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	if err := s.db.Ping(ctx); err != nil {
		loggerFrom(ctx).Error("database check failed", "error", err, "path", s.db.Path())
		return []Check{{
			Name:   "database",
			Detail: "Cannot reach " + s.db.Path() + ". Check that the file and its directory are writable by the service user.",
		}}
	}
	checks := []Check{{Name: "database", OK: true, Detail: s.db.Path()}}

	version, err := s.db.SchemaVersion(ctx)
	switch {
	case err != nil:
		loggerFrom(ctx).Error("schema check failed", "error", err)
		checks = append(checks, Check{
			Name:   "schema",
			Detail: "Cannot read the applied migrations. The database may be from a newer version of rental-bot.",
		})
	case version == 0:
		checks = append(checks, Check{
			Name:   "schema",
			Detail: "No migrations have been applied. Run `make migrate`.",
		})
	default:
		checks = append(checks, Check{Name: "schema", OK: true, Detail: "version " + strconv.Itoa(version)})
	}
	return checks
}

func allOK(checks []Check) bool {
	for _, c := range checks {
		if !c.OK {
			return false
		}
	}
	return true
}

// readinessWord names the overall state in the same vocabulary the status
// card stamps, so the API and the UI say the same word.
func readinessWord(checks []Check) string {
	if allOK(checks) {
		return "operational"
	}
	return "degraded"
}
