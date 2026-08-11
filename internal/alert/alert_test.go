package alert

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
	"github.com/farrellm/rental-bot/migrations"
)

// recorder is a sink that keeps what it was handed.
type recorder struct {
	name string
	// fail makes the next Deliver return an error, which must not stop the bus
	// recording the condition.
	fail bool

	mu      sync.Mutex
	notices []Notice
}

func (r *recorder) Name() string { return r.name }

func (r *recorder) Deliver(_ context.Context, n Notice) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notices = append(r.notices, n)
	if r.fail {
		return errors.New("the channel is unreachable")
	}
	return nil
}

func (r *recorder) all() []Notice {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Notice(nil), r.notices...)
}

// openBus returns a bus over a real migrated database. The cooldown rests on a
// partial unique index, and an index is only worth trusting against the real
// schema.
func openBus(t testing.TB) (*Bus, *recorder) {
	t.Helper()

	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "alert.db"), 2)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(t.Context(), migrations.FS); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	bus := New(db.Repo(), Options{
		Cooldown:         6 * time.Hour,
		CriticalCooldown: time.Hour,
		Logger:           quiet(),
	})
	sink := &recorder{name: "telegram"}
	bus.Subscribe(sink)
	return bus, sink
}

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

func condition() Alert {
	return Alert{
		Key:      "gmail.watch.lapsed",
		Severity: Warning,
		Title:    "The Gmail watch has lapsed",
		Detail:   "Mail still arrives on the fallback poll.",
	}
}

// The whole point of the bus: a condition that keeps being true says one thing
// and then goes quiet.
func TestCooldownSaysItOnce(t *testing.T) {
	bus, sink := openBus(t)
	ctx := t.Context()

	for range 5 {
		bus.Publish(ctx, condition())
	}

	if got := len(sink.all()); got != 1 {
		t.Fatalf("delivered %d notices, want 1: five reports of one condition are one message", got)
	}
}

func TestCooldownExpiryRestatesWithTheTally(t *testing.T) {
	bus, sink := openBus(t)
	ctx := t.Context()

	start := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	bus.now = func() time.Time { return start }
	bus.Publish(ctx, condition())

	// Inside the cooldown: nothing.
	bus.now = func() time.Time { return start.Add(5 * time.Hour) }
	bus.Publish(ctx, condition())

	// Past it: said again, and the message knows it is a repeat.
	bus.now = func() time.Time { return start.Add(7 * time.Hour) }
	bus.Publish(ctx, condition())

	notices := sink.all()
	if len(notices) != 2 {
		t.Fatalf("delivered %d notices, want 2", len(notices))
	}
	if notices[0].Restated() {
		t.Error("the first notice reported itself as a restatement")
	}
	if !notices[1].Restated() {
		t.Error("the second notice did not report itself as a restatement")
	}
	if got := notices[1].SendCount; got != 1 {
		t.Errorf("SendCount on the restatement = %d, want 1", got)
	}
	if !notices[1].FirstSeenAt.Equal(start) {
		t.Errorf("FirstSeenAt = %s, want the first sighting %s", notices[1].FirstSeenAt, start)
	}
}

// A critical condition is restated sooner, because nobody fixes a thing they
// have forgotten about.
func TestCriticalUsesItsOwnCooldown(t *testing.T) {
	bus, sink := openBus(t)
	ctx := t.Context()

	start := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	bus.now = func() time.Time { return start }

	critical := Alert{Key: "gmail.grant.revoked", Severity: Critical, Title: "Access was revoked"}
	bus.Publish(ctx, critical)
	bus.Publish(ctx, condition())

	// Ninety minutes: past the critical cooldown, well inside the ordinary one.
	bus.now = func() time.Time { return start.Add(90 * time.Minute) }
	bus.Publish(ctx, critical)
	bus.Publish(ctx, condition())

	var criticals, warnings int
	for _, n := range sink.all() {
		if n.Severity == Critical {
			criticals++
		} else {
			warnings++
		}
	}
	if criticals != 2 {
		t.Errorf("delivered %d critical notices, want 2", criticals)
	}
	if warnings != 1 {
		t.Errorf("delivered %d warning notices, want 1", warnings)
	}
}

func TestResolveSaysSoOnceAndFreesTheKey(t *testing.T) {
	bus, sink := openBus(t)
	ctx := t.Context()

	bus.Publish(ctx, condition())
	bus.Resolve(ctx, condition().Key, "The Gmail watch has been renewed")
	// A probe calls Resolve on every sweep. The ones that find nothing open
	// owe nobody a message.
	bus.Resolve(ctx, condition().Key, "The Gmail watch has been renewed")
	bus.Resolve(ctx, condition().Key, "The Gmail watch has been renewed")

	notices := sink.all()
	if len(notices) != 2 {
		t.Fatalf("delivered %d notices, want 2: the condition and its recovery", len(notices))
	}
	if !notices[1].Recovered {
		t.Error("the second notice is not marked as a recovery")
	}
	if notices[1].Restated() {
		t.Error("a recovery reported itself as a restatement")
	}

	// The condition recurring after it cleared is news again, immediately --
	// the cooldown belongs to the occurrence, not to the key forever.
	bus.Publish(ctx, condition())
	if got := len(sink.all()); got != 3 {
		t.Errorf("delivered %d notices, want 3: a recurrence after a recovery is news", got)
	}
}

// A channel that is down must not cost the record. The sink owns retrying;
// the bus owns knowing the condition happened.
func TestADeadSinkStillLeavesARecord(t *testing.T) {
	bus, sink := openBus(t)
	sink.fail = true
	ctx := t.Context()

	bus.Publish(ctx, condition())
	bus.Publish(ctx, condition())

	if got := len(sink.all()); got != 1 {
		t.Fatalf("delivered %d notices, want 1: a failed delivery is still a stated condition", got)
	}
}

// Each channel gets its own row, so adding one later does not silence it with
// the first one's cooldown.
func TestEachChannelHasItsOwnCooldown(t *testing.T) {
	bus, first := openBus(t)
	second := &recorder{name: "log"}
	bus.Subscribe(second)
	ctx := t.Context()

	bus.Publish(ctx, condition())
	bus.Publish(ctx, condition())

	if got := len(first.all()); got != 1 {
		t.Errorf("telegram received %d notices, want 1", got)
	}
	if got := len(second.all()); got != 1 {
		t.Errorf("log received %d notices, want 1", got)
	}
}

func TestAKeylessAlertIsRefused(t *testing.T) {
	bus, sink := openBus(t)
	bus.Publish(t.Context(), Alert{Severity: Warning, Title: "something happened"})

	if got := len(sink.all()); got != 0 {
		t.Errorf("delivered %d notices for an alert with no key, want 0", got)
	}
}

// A long detail belongs in the log, not in a column and not on a phone.
func TestDetailIsTruncated(t *testing.T) {
	bus, sink := openBus(t)
	a := condition()
	a.Detail = string(make([]byte, 2000))
	bus.Publish(t.Context(), a)

	notices := sink.all()
	if len(notices) != 1 {
		t.Fatalf("delivered %d notices, want 1", len(notices))
	}
	if got := len(notices[0].Detail); got > detailLimit+3 {
		t.Errorf("Detail is %d characters, want it cut to %d plus an ellipsis", got, detailLimit)
	}
}

// The register is what the dispatch card renders, so the row has to carry the
// tally and not a second line.
func TestTheRegisterKeepsOneLinePerCondition(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "alert.db"), 2)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(t.Context(), migrations.FS); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	repo := db.Repo()

	bus := New(repo, Options{Cooldown: time.Hour, CriticalCooldown: time.Minute, Logger: quiet()})
	bus.Subscribe(&recorder{name: "telegram"})
	ctx := t.Context()

	start := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	for i := range 3 {
		bus.now = func() time.Time { return start.Add(time.Duration(i) * 2 * time.Hour) }
		bus.Publish(ctx, condition())
	}

	rows, err := repo.Read().ListChannelNotificationsFirstPage(ctx, sqlc.ListChannelNotificationsFirstPageParams{
		Channel: "telegram", Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListNotificationsFirstPage: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the register holds %d rows, want 1", len(rows))
	}
	if rows[0].SendCount != 3 {
		t.Errorf("SendCount = %d, want 3", rows[0].SendCount)
	}
}

func TestPublishAndResolveTolerateANilPublisher(t *testing.T) {
	// Every caller would otherwise write this check, and the one that forgot
	// would panic on a host that never configured a channel.
	Publish(t.Context(), nil, condition())
	Resolve(t.Context(), nil, "k", "t")
}

func TestSeverityMatchesTheSchema(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "alert.db"), 2)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(t.Context(), migrations.FS); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	repo := db.Repo()

	// If a severity constant and the CHECK ever drift, this is where it shows.
	for _, s := range []Severity{Info, Warning, Critical} {
		if !s.Valid() {
			t.Errorf("%q is not Valid", s)
		}
		if _, err := repo.Write().InsertNotification(t.Context(), sqlc.InsertNotificationParams{
			DedupeKey:   "check." + string(s),
			Channel:     "telegram",
			Severity:    string(s),
			Title:       "a condition",
			FirstSeenAt: stamp(time.Now()),
			CreatedAt:   stamp(time.Now()),
			UpdatedAt:   stamp(time.Now()),
		}); err != nil {
			t.Errorf("the schema refused severity %q: %v", s, err)
		}
	}
	if Severity("panic").Valid() {
		t.Error("an unknown severity reported itself as valid")
	}
}
