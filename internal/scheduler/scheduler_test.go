package scheduler

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/farrellm/rental-bot/internal/jobs"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/migrations"
)

func openQueue(t *testing.T) *jobs.Queue {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "sched.db"), 2)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(t.Context(), migrations.FS); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return jobs.NewQueue(db.Repo())
}

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// A process that has been down for a week has a lapsed watch, and should not
// wait a day to find that out.
func TestAtStartFiresBeforeTheFirstTick(t *testing.T) {
	queue := openQueue(t)
	woken := make(chan struct{}, 4)

	s := New(queue, func() { woken <- struct{}{} }, quiet())
	s.Add(Task{Name: "gmail.watch", Kind: "gmail.watch.renew", Every: time.Hour, AtStart: true})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.Start(ctx)

	select {
	case <-woken:
	case <-time.After(3 * time.Second):
		t.Fatal("the at-start task never enqueued anything")
	}

	if got := depth(t, queue)["pending"]; got != 1 {
		t.Errorf("pending jobs = %d, want 1", got)
	}
}

// Ticks that overlap a job the runner has not reached yet must queue one job,
// not a backlog of them.
func TestOverlappingTicksQueueOneJob(t *testing.T) {
	queue := openQueue(t)

	s := New(queue, nil, quiet())
	s.Add(Task{Name: "gmail.sync", Kind: "gmail.sync", Every: 10 * time.Millisecond, AtStart: true})

	ctx, cancel := context.WithCancel(t.Context())
	s.Start(ctx)
	time.Sleep(150 * time.Millisecond)
	cancel()

	if err := s.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := depth(t, queue)["pending"]; got != 1 {
		t.Errorf("pending jobs = %d after a dozen ticks, want 1", got)
	}
}

func TestATaskWithNoPeriodIsSkipped(t *testing.T) {
	queue := openQueue(t)

	s := New(queue, nil, quiet())
	s.Add(Task{Name: "misconfigured", Kind: "gmail.sync", Every: 0, AtStart: true})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.Start(ctx)
	time.Sleep(50 * time.Millisecond)

	if got := depth(t, queue)["pending"]; got != 0 {
		t.Errorf("pending jobs = %d, want 0: a task with no period should not run", got)
	}
	if err := s.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func depth(t *testing.T, queue *jobs.Queue) map[string]int64 {
	t.Helper()
	got, err := queue.Depth(t.Context())
	if err != nil {
		t.Fatalf("Depth: %v", err)
	}
	return got
}
