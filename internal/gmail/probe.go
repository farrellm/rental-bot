package gmail

import (
	"context"
	"errors"
	"time"

	"github.com/farrellm/rental-bot/internal/alert"
)

// Keys for the conditions this package raises. A key names the condition, not
// the occurrence, so these are constants rather than strings built from a
// message.
const (
	// KeyGrantRevoked is the one condition here that no amount of waiting
	// fixes.
	KeyGrantRevoked = "gmail.grant.revoked"
	// KeySyncDegraded is the last sync having failed.
	KeySyncDegraded = "gmail.sync.degraded"
	// KeyWatchLapsed is the push registration having expired.
	KeyWatchLapsed = "gmail.watch.lapsed"
	// KeySyncStalled is the sync loop having stopped running.
	//
	// §8.3 words this one as "no email processed in N days". It is worded here
	// as the mailbox not being *checked*, because that is what last_sync_at
	// actually records: a poll that finds nothing still advances it. Alerting
	// on a quiet week of mail would fire every Christmas; alerting on a poll
	// that stopped happening catches the thing §12 lists as a risk.
	KeySyncStalled = "gmail.sync.stalled"
)

// Probe watches the connected mailbox for the failures §4.3 lists.
//
// These are exactly the conditions M3 made possible and nothing reports: a
// grant revoked days before the ledger visibly stops growing, a watch that
// lapsed while the process was down, a mailbox that has quietly delivered
// nothing since Tuesday. §11 puts M3.5 before the LLM work for this reason.
//
// silentAfter of zero turns the stalled-sync check off. now is injectable so a
// test can age the account rather than waiting two days.
func Probe(tokens *Store, silentAfter time.Duration, now func() time.Time) alert.Probe {
	if tokens == nil {
		return nil
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	return func(ctx context.Context) []alert.Reading {
		account, err := tokens.Account(ctx)
		if errors.Is(err, ErrNotConnected) {
			// Nobody asked for ingestion, or nobody has finished asking.
			// Neither is a fault, and both clear anything said earlier: a
			// disconnected mailbox has no revoked grant to complain about.
			return allClear()
		}
		if err != nil {
			// The store is what would record the alert. Nothing useful to say.
			return nil
		}

		at := now()
		readings := []alert.Reading{
			revokedReading(account),
			degradedReading(account),
			watchReading(account, at),
		}
		if silentAfter > 0 {
			readings = append(readings, stalledReading(account, at, silentAfter))
		}
		return readings
	}
}

func allClear() []alert.Reading {
	return []alert.Reading{
		alert.Clear(KeyGrantRevoked, titleRevoked),
		alert.Clear(KeySyncDegraded, titleDegraded),
		alert.Clear(KeyWatchLapsed, titleLapsed),
		alert.Clear(KeySyncStalled, titleStalled),
	}
}

const (
	titleRevoked  = "Google has revoked access to the mailbox"
	titleDegraded = "The last mailbox sync failed"
	titleLapsed   = "The Gmail watch has lapsed"
	titleStalled  = "The mailbox has not been checked recently"
)

func revokedReading(a Account) alert.Reading {
	if a.Status != "revoked" {
		return alert.Clear(KeyGrantRevoked, titleRevoked)
	}
	return alert.Watching(KeyGrantRevoked, alert.Critical, titleRevoked,
		"Ingestion has stopped and will not restart on its own. Reconnect the mailbox on the Intake screen.")
}

func degradedReading(a Account) alert.Reading {
	// A revoked grant is already being reported as critical; saying the sync
	// also failed adds a second message about one condition.
	if a.Status != "degraded" {
		return alert.Clear(KeySyncDegraded, titleDegraded)
	}
	return alert.Watching(KeySyncDegraded, alert.Warning, titleDegraded,
		alert.Errorf("%s Mail already on file is unaffected.", a.LastError))
}

// A lapsed watch is a warning rather than a fault, and the message says why:
// the poller carries ingestion on its own, so mail still arrives, just later.
// Saying only "lapsed" sends the operator looking for an outage that is not
// there.
func watchReading(a Account, at time.Time) alert.Reading {
	if a.Status == "revoked" || !a.WatchLapsed(at) {
		return alert.Clear(KeyWatchLapsed, titleLapsed)
	}
	return alert.Watching(KeyWatchLapsed, alert.Warning, titleLapsed,
		"Push delivery has stopped. Mail still arrives on the fallback poll, so this is slower rather than stopped.")
}

func stalledReading(a Account, at time.Time, after time.Duration) alert.Reading {
	// A mailbox that has never synced is one connected a moment ago, not a
	// stalled one; the callback enqueues the first sync. A revoked grant is
	// already being reported, and reporting the stall it causes as well is one
	// condition arriving as two messages.
	if a.LastSyncAt == nil || a.Status == "revoked" {
		return alert.Clear(KeySyncStalled, titleStalled)
	}
	quiet := at.Sub(*a.LastSyncAt)
	if quiet < after {
		return alert.Clear(KeySyncStalled, titleStalled)
	}
	return alert.Watching(KeySyncStalled, alert.Warning, titleStalled,
		alert.Errorf("The last sync of %s was %s ago, and the poller runs far more often than that. Anything forwarded since then is not on file.",
			a.Address, quiet.Round(time.Hour)))
}
