// Package scheduler runs the periodic work: the Gmail watch renewal, the
// fallback poller, and the queue's own housekeeping.
//
// It enqueues rather than doing anything itself. A tick that ran the work
// inline would get its retries, its backoff, and its crash recovery from
// nowhere; a tick that enqueues inherits all three from the queue.
package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/farrellm/rental-bot/internal/jobs"
)

// Task is one periodic entry.
type Task struct {
	// Name appears in the logs and is the dedupe key, so two ticks that overlap
	// a slow job queue one job rather than a backlog of them.
	Name string
	// Kind is the job kind to enqueue.
	Kind string
	// Every is the period. A task with a zero or negative period is skipped
	// with a warning rather than spinning.
	Every time.Duration
	// AtStart runs the task once when the scheduler starts, before the first
	// tick. The watch renewal wants this: a process that has been down for a
	// week has a lapsed watch and should not wait a day to find out.
	AtStart bool
}

// Scheduler ticks a set of tasks.
type Scheduler struct {
	queue *jobs.Queue
	// notify wakes the runner when something is enqueued, so a poll that comes
	// due starts now rather than at the runner's next poll.
	notify func()
	log    *slog.Logger
	tasks  []Task

	wg sync.WaitGroup
}

// New builds a scheduler. notify may be nil.
func New(queue *jobs.Queue, notify func(), logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	if notify == nil {
		notify = func() {}
	}
	return &Scheduler{queue: queue, notify: notify, log: logger}
}

// Add registers a periodic task. Adding after Start has no effect.
func (s *Scheduler) Add(t Task) {
	s.tasks = append(s.tasks, t)
}

// Start launches one goroutine per task and returns.
func (s *Scheduler) Start(ctx context.Context) {
	for _, task := range s.tasks {
		if task.Every <= 0 {
			s.log.Warn("scheduled task has no period; skipping", "task", task.Name)
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.run(ctx, task)
		}()
	}
}

// Stop waits for the tickers to return, bounded by ctx.
func (s *Scheduler) Stop(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return errors.New("scheduler: tasks did not stop before the shutdown deadline")
	}
}

func (s *Scheduler) run(ctx context.Context, task Task) {
	ticker := time.NewTicker(task.Every)
	defer ticker.Stop()

	if task.AtStart {
		s.fire(ctx, task)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.fire(ctx, task)
		}
	}
}

// fire enqueues the task's job, treating an already-queued one as success.
func (s *Scheduler) fire(ctx context.Context, task Task) {
	added, err := s.queue.EnqueueOnce(ctx, task.Kind, task.Name, nil)
	switch {
	case err != nil && ctx.Err() != nil:
		// Shutting down. Not worth a line.
	case err != nil:
		s.log.Error("enqueue scheduled task", "task", task.Name, "kind", task.Kind, "error", err)
	case added:
		s.log.Debug("enqueued scheduled task", "task", task.Name, "kind", task.Kind)
		s.notify()
	default:
		// The previous tick's job has not run yet. That is the queue holding
		// the line, not a problem: one sync is what was wanted.
		s.log.Debug("scheduled task already queued", "task", task.Name)
	}
}
