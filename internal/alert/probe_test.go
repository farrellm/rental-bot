package alert

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/farrellm/rental-bot/internal/jobs"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/migrations"
)

// A probe reports both directions, so the watchdog turns one into a message
// and the other into a recovery — and neither into anything when nothing has
// changed.
func TestSweepPublishesAndResolves(t *testing.T) {
	bus, sink := openBus(t)
	watchdog := NewWatchdog(bus, quiet())

	lapsed := true
	watchdog.Add("watch", func(context.Context) []Reading {
		if lapsed {
			return []Reading{Watching("gmail.watch.lapsed", Warning, "The Gmail watch has lapsed", "")}
		}
		return []Reading{Clear("gmail.watch.lapsed", "The Gmail watch has been renewed")}
	})

	ctx := t.Context()
	// Two sweeps with the condition true: the cooldown makes that one message.
	if err := watchdog.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if err := watchdog.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	lapsed = false
	// Two sweeps with it false: the first owes a recovery, the second owes
	// nothing.
	if err := watchdog.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if err := watchdog.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	notices := sink.all()
	if len(notices) != 2 {
		t.Fatalf("delivered %d notices, want 2: the condition and its recovery", len(notices))
	}
	if notices[0].Recovered || !notices[1].Recovered {
		t.Errorf("got recovered flags %v and %v, want false then true", notices[0].Recovered, notices[1].Recovered)
	}
}

// A watchdog that stops watching after one bad reading is worse than no
// watchdog, because the screen still says it is running.
func TestOneBadProbeDoesNotCostTheOthersTheirTurn(t *testing.T) {
	bus, sink := openBus(t)
	watchdog := NewWatchdog(bus, quiet())

	watchdog.Add("panics", func(context.Context) []Reading { panic("a probe read a nil pointer") })
	watchdog.Add("works", func(context.Context) []Reading {
		return []Reading{Watching("jobs.backlog", Warning, "The job queue is not draining", "")}
	})

	if err := watchdog.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got := len(sink.all()); got != 1 {
		t.Errorf("delivered %d notices, want 1: the working probe still reported", got)
	}
}

func TestQueueDepthProbe(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "alert.db"), 2)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(t.Context(), migrations.FS); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	queue := jobs.NewQueue(db.Repo())
	ctx := t.Context()

	probe := QueueDepthProbe(queue, 3)
	if got := probe(ctx); len(got) != 1 || !got[0].Cleared {
		t.Fatalf("an empty queue read as %+v, want one cleared reading", got)
	}

	for i := range 4 {
		if _, err := queue.Enqueue(ctx, "gmail.sync", nil, jobs.Options{
			DedupeKey: "sync-" + string(rune('a'+i)),
		}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	readings := probe(ctx)
	if len(readings) != 1 || readings[0].Cleared {
		t.Fatalf("a backlogged queue read as %+v, want one outstanding reading", readings)
	}
	if readings[0].Key != KeyQueueBacklog {
		t.Errorf("Key = %q, want %q", readings[0].Key, KeyQueueBacklog)
	}
	if readings[0].Severity != Warning {
		t.Errorf("Severity = %q, want %q", readings[0].Severity, Warning)
	}
}

// "Tell me when there is any work queued" is not a thing anybody wants at
// three in the morning.
func TestQueueDepthProbeIsOffAtZero(t *testing.T) {
	if QueueDepthProbe(nil, 10) != nil {
		t.Error("a probe was built over a nil queue")
	}
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "alert.db"), 2)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if QueueDepthProbe(jobs.NewQueue(db.Repo()), 0) != nil {
		t.Error("a probe was built with a threshold of zero")
	}
}

// The sweep is a job like any other, so it inherits the queue's retries rather
// than needing its own.
func TestRegisterSweepRunsOnTheQueue(t *testing.T) {
	bus, sink := openBus(t)
	watchdog := NewWatchdog(bus, quiet())
	watchdog.Add("test", func(context.Context) []Reading {
		return []Reading{Watching("test.condition", Info, "A condition", "")}
	})

	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "runner.db"), 2)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(t.Context(), migrations.FS); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	queue := jobs.NewQueue(db.Repo())
	runner := jobs.NewRunner(queue, jobs.RunnerOptions{
		Workers: 1, PollInterval: 10 * time.Millisecond, Logger: quiet(),
	})
	RegisterSweep(runner, watchdog)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runner.Start(ctx)
	t.Cleanup(func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		_ = runner.Stop(stopCtx)
	})

	if _, err := queue.EnqueueOnce(ctx, KindSweep, KindSweep, nil); err != nil {
		t.Fatalf("EnqueueOnce: %v", err)
	}
	runner.Notify()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(sink.all()) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the sweep job did not run within 3s")
}
