package alert

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

const (
	// titleLimit and detailLimit bound what reaches a column and a message.
	titleLimit  = 200
	detailLimit = 500
)

// Options tune the bus. A zero value is usable: the cooldowns fall back to
// §8.3's defaults rather than to zero, because a zero cooldown is an alert
// storm and nobody means to ask for one.
type Options struct {
	// Cooldown is how long a condition stays quiet after being stated.
	Cooldown time.Duration
	// CriticalCooldown is the same for critical conditions, which are worth
	// restating sooner.
	CriticalCooldown time.Duration
	Logger           *slog.Logger
}

// Bus records conditions and hands them to the sinks that are due one.
//
// It is safe for concurrent use: the job runner, the HTTP handlers, and the
// scheduler's sweep all publish, and they do it from different goroutines.
type Bus struct {
	repo *store.Repo
	log  *slog.Logger

	cooldown         time.Duration
	criticalCooldown time.Duration

	// now is injectable so a test can drive a cooldown forward rather than
	// waiting six hours for it.
	now func() time.Time

	mu    sync.RWMutex
	sinks []Sink
}

// New builds a bus over the store.
func New(repo *store.Repo, opts Options) *Bus {
	if opts.Cooldown <= 0 {
		opts.Cooldown = 6 * time.Hour
	}
	if opts.CriticalCooldown <= 0 {
		opts.CriticalCooldown = time.Hour
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Bus{
		repo:             repo,
		log:              opts.Logger,
		cooldown:         opts.Cooldown,
		criticalCooldown: opts.CriticalCooldown,
		now:              func() time.Time { return time.Now().UTC() },
	}
}

// Subscribe adds a channel. Subscribing after the process is running is fine;
// it just means earlier conditions were never offered to it.
func (b *Bus) Subscribe(s Sink) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sinks = append(b.sinks, s)
}

// Publish states a condition on every subscribed channel that is due one.
//
// It does not return an error, and that is deliberate. Every caller is
// something that has just noticed a problem and has its own work to get on
// with; a caller that has to decide what to do about a failed alert will
// either ignore it or lose the thing it was actually doing. Failures are
// logged here and nowhere else.
func (b *Bus) Publish(ctx context.Context, a Alert) {
	if a.Key == "" {
		b.log.Error("an alert was published with no key; it cannot be deduplicated", "title", a.Title)
		return
	}
	if !a.Severity.Valid() {
		// Better a loud unknown than a silent one: the schema would refuse the
		// row and the condition would vanish.
		b.log.Error("an alert was published with an unknown severity", "key", a.Key, "severity", a.Severity)
		a.Severity = Warning
	}
	a.Title = domain.Truncate(a.Title, titleLimit)
	a.Detail = domain.Truncate(a.Detail, detailLimit)

	for _, sink := range b.snapshot() {
		b.publishTo(ctx, sink, a)
	}
}

// Resolve closes a condition and says so, once.
//
// Doing nothing when there was no open row is the whole point: a probe calls
// this on every sweep, and only the sweep that finds the condition still open
// owes anybody a recovery message.
func (b *Bus) Resolve(ctx context.Context, key, title string) {
	if key == "" {
		return
	}
	at := b.now()

	for _, sink := range b.snapshot() {
		closed, err := b.repo.Write().ResolveNotification(ctx, sqlc.ResolveNotificationParams{
			ResolvedAt: domain.Stamp(at),
			UpdatedAt:  domain.Stamp(at),
			DedupeKey:  key,
			Channel:    sink.Name(),
		})
		if err != nil {
			b.log.Error("resolve a notification", "error", err, "key", key, "channel", sink.Name())
			continue
		}
		if closed == 0 {
			continue
		}

		b.deliver(ctx, sink, Notice{
			Alert:       Alert{Key: key, Severity: Info, Title: domain.Truncate(title, titleLimit)},
			FirstSeenAt: at,
			Recovered:   true,
		})
	}
}

// publishTo applies the cooldown for one channel and delivers when it is due.
func (b *Bus) publishTo(ctx context.Context, sink Sink, a Alert) {
	at := b.now()
	channel := sink.Name()

	open, err := b.repo.Read().GetOpenNotification(ctx, sqlc.GetOpenNotificationParams{
		DedupeKey: a.Key,
		Channel:   channel,
	})
	switch {
	case store.NotFound(err):
		b.stateFresh(ctx, sink, a, at)
		return
	case err != nil:
		// The record is unreadable. Say it anyway: losing an alert because the
		// database that was going to deduplicate it is unwell is exactly the
		// wrong trade, and a database that is unwell is itself worth hearing
		// about.
		b.log.Error("read the open notification", "error", err, "key", a.Key, "channel", channel)
		b.deliver(ctx, sink, Notice{Alert: a, FirstSeenAt: at})
		return
	}

	if last := open.LastSentAt; last != nil {
		if at.Sub(domain.ParseStamp(*last)) < b.cooldownFor(a.Severity) {
			// Still quiet. This is the branch that does the work of §8.3.
			b.log.Debug("alert suppressed by its cooldown",
				"key", a.Key, "channel", channel, "send_count", open.SendCount)
			return
		}
	}

	if err := b.repo.Write().RecordNotificationSent(ctx, sqlc.RecordNotificationSentParams{
		LastSentAt: domain.Stamp(at),
		Severity:   string(a.Severity),
		Title:      a.Title,
		Detail:     a.Detail,
		UpdatedAt:  domain.Stamp(at),
		ID:         open.ID,
	}); err != nil {
		b.log.Error("record a restated notification", "error", err, "key", a.Key, "channel", channel)
	}

	b.deliver(ctx, sink, Notice{
		Alert:       a,
		FirstSeenAt: domain.ParseStamp(open.FirstSeenAt),
		SendCount:   open.SendCount,
	})
}

// stateFresh records a condition nobody has an open row for and delivers it.
func (b *Bus) stateFresh(ctx context.Context, sink Sink, a Alert, at time.Time) {
	channel := sink.Name()

	row, err := b.repo.Write().InsertNotification(ctx, sqlc.InsertNotificationParams{
		DedupeKey:   a.Key,
		Channel:     channel,
		Severity:    string(a.Severity),
		Title:       a.Title,
		Detail:      a.Detail,
		FirstSeenAt: domain.Stamp(at),
		CreatedAt:   domain.Stamp(at),
		UpdatedAt:   domain.Stamp(at),
	})
	if err != nil {
		if store.Conflict(err) {
			// Two goroutines noticed the same condition at once and the index
			// held the line. The one that lost says nothing, which is the
			// answer it wanted.
			b.log.Debug("an alert raced another under the same key", "key", a.Key, "channel", channel)
			return
		}
		// Same trade as an unreadable record: say it, and log why it is not on
		// file.
		b.log.Error("record a notification", "error", err, "key", a.Key, "channel", channel)
		b.deliver(ctx, sink, Notice{Alert: a, FirstSeenAt: at})
		return
	}

	if err := b.repo.Write().RecordNotificationSent(ctx, sqlc.RecordNotificationSentParams{
		LastSentAt: domain.Stamp(at),
		Severity:   string(a.Severity),
		Title:      a.Title,
		Detail:     a.Detail,
		UpdatedAt:  domain.Stamp(at),
		ID:         row.ID,
	}); err != nil {
		b.log.Error("record the first send of a notification", "error", err, "key", a.Key)
	}

	b.deliver(ctx, sink, Notice{Alert: a, FirstSeenAt: at})
}

// deliver hands a notice to one sink, logging what it cannot do about a
// failure.
func (b *Bus) deliver(ctx context.Context, sink Sink, n Notice) {
	if err := sink.Deliver(ctx, n); err != nil {
		b.log.Error("deliver an alert",
			"error", err, "channel", sink.Name(), "key", n.Key, "severity", n.Severity)
	}
}

func (b *Bus) cooldownFor(s Severity) time.Duration {
	if s == Critical {
		return b.criticalCooldown
	}
	return b.cooldown
}

// snapshot copies the sink list so a delivery does not hold the lock.
func (b *Bus) snapshot() []Sink {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return slices.Clone(b.sinks)
}
