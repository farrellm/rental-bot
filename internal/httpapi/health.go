package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/farrellm/rental-bot/internal/gmail"
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

	checks = append(checks, s.ingestionChecks(ctx)...)
	return append(checks, s.channelChecks(ctx)...)
}

// channelChecks reports on the alert channel.
//
// Nothing here fails readiness, and that is the point. A channel nobody asked
// for is not a fault; an unpaired one is a setup step, not an outage; and a
// degraded one is the alerting subsystem being unwell, which the API serving
// the ledger has no business refusing traffic over. The condition is in the
// detail, where a person reads it.
func (s *server) channelChecks(ctx context.Context) []Check {
	if !s.cfg.Telegram.Enabled() || s.telegram == nil {
		return []Check{{
			Name: "telegram", OK: true,
			Detail: "Not configured. Set telegram.bot_username to enable alerting.",
		}}
	}

	state, err := s.telegram.State(ctx)
	if err != nil {
		loggerFrom(ctx).Error("telegram check failed", "error", err)
		return []Check{{Name: "telegram", Detail: "The channel could not be read. " + err.Error()}}
	}

	switch {
	case !state.Paired():
		return []Check{{
			Name: "telegram", OK: true,
			Detail: "Configured but no chat is paired. Pair one on the Intake screen.",
		}}
	case state.Status == "degraded":
		return []Check{{
			Name: "telegram", OK: true,
			Detail: "The last message could not be delivered: " + state.LastError,
		}}
	case state.Muted(time.Now().UTC()):
		return []Check{{
			Name: "telegram", OK: true,
			Detail: "Paired, muted until " + state.MutedUntil.Format(time.RFC3339) + ".",
		}}
	default:
		return []Check{{Name: "telegram", OK: true, Detail: "Paired."}}
	}
}

// ingestionChecks reports on the Gmail connection, its watch, and its sync
// recency (docs/DESIGN.md §9.3).
//
// M3 is the first milestone where this system can fail silently: a revoked
// grant, a lapsed watch, a poller that stopped. Every one of those looks
// exactly like "no mail arrived today" from the outside, which is why they are
// checks rather than something to notice.
func (s *server) ingestionChecks(ctx context.Context) []Check {
	// Not configured is not a fault. A fresh clone has no Google project, and
	// /readyz reporting 503 over an unwanted subsystem is a check that teaches
	// its reader to ignore it.
	if !s.cfg.Gmail.Enabled() || s.gmail == nil {
		return []Check{{
			Name: "gmail", OK: true,
			Detail: "Not configured. Set gmail.client_id to enable email ingestion.",
		}}
	}

	account, err := s.gmail.Account(ctx)
	switch {
	case errors.Is(err, gmail.ErrNotConnected):
		return []Check{{
			Name: "gmail", OK: true,
			Detail: "Configured but no mailbox is connected. Connect one on the Intake screen.",
		}}
	case err != nil:
		loggerFrom(ctx).Error("gmail check failed", "error", err)
		return []Check{{
			Name:   "gmail",
			Detail: "The connected account could not be read. " + err.Error(),
		}}
	}

	checks := make([]Check, 0, 3)
	switch account.Status {
	case "revoked":
		// The one condition here that no amount of waiting fixes.
		checks = append(checks, Check{
			Name:   "gmail",
			Detail: "The Google authorization was revoked. Reconnect the mailbox on the Intake screen.",
		})
	case "degraded":
		checks = append(checks, Check{
			Name:   "gmail",
			Detail: "The last sync failed: " + account.LastError,
		})
	default:
		checks = append(checks, Check{Name: "gmail", OK: true, Detail: account.Address})
	}

	now := time.Now().UTC()
	if account.WatchLapsed(now) {
		// A warning rather than a fault: the poller carries ingestion on its
		// own, so this is slower, not stopped, and it says so.
		checks = append(checks, Check{
			Name: "gmail_watch", OK: true,
			Detail: "No live push registration. Mail still arrives on the " +
				s.cfg.Gmail.PollInterval.String() + " poll.",
		})
	} else {
		checks = append(checks, Check{
			Name: "gmail_watch", OK: true,
			Detail: "renews before " + account.WatchExpiresAt.Format(time.RFC3339),
		})
	}

	checks = append(checks, s.lastSyncCheck(account, now))
	return checks
}

// lastSyncCheck reports whether the poller is still running.
//
// The threshold is three poll intervals: one missed tick is a slow Gmail, three
// in a row is something that is not coming back on its own.
func (s *server) lastSyncCheck(account gmail.Account, now time.Time) Check {
	stale := 3 * s.cfg.Gmail.PollInterval.Duration

	if account.LastSyncAt == nil {
		return Check{
			Name: "gmail_sync", OK: true,
			Detail: "No sync has run yet.",
		}
	}
	since := now.Sub(*account.LastSyncAt)
	if since > stale {
		return Check{
			Name: "gmail_sync",
			Detail: "The last sync was " + since.Round(time.Minute).String() +
				" ago, past the " + stale.String() + " threshold. Check the job queue and the logs.",
		}
	}
	return Check{
		Name: "gmail_sync", OK: true,
		Detail: since.Round(time.Second).String() + " ago",
	}
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
