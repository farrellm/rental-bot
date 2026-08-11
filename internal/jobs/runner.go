package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

// Handler runs one job. Returning an error schedules a retry until the job runs
// out of attempts.
//
// A handler must be idempotent. The queue is at-least-once by construction: a
// process killed after the work but before the row is marked done will run the
// job again, and that is the correct trade — losing a forwarded receipt is
// worse than filing it twice, and every write in the ingestion path is already
// keyed so that filing it twice costs nothing.
type Handler func(ctx context.Context, job Job) error

// RunnerOptions configure the pool. Enqueue's Options tune one job; these tune
// the thing that runs them.
type RunnerOptions struct {
	// Workers bounds concurrency. Below one becomes one.
	Workers int
	// PollInterval is how often an idle worker looks for work. An enqueue
	// through the runner's Queue wakes it immediately, so this only bounds how
	// late a job scheduled for the future starts.
	PollInterval time.Duration
	// LeaseTimeout is how long a claimed job may stay locked before it is
	// assumed abandoned.
	LeaseTimeout time.Duration
	// OnDeadLetter is called once for a job that has run out of attempts.
	//
	// It is a callback rather than an alert.Publisher because this package is
	// the queue, not an alert client: the caller decides that a dead letter is
	// worth waking somebody for, and this only decides when one happened. It
	// may be nil, and it must not block — the worker that calls it is holding
	// a slot in the pool.
	OnDeadLetter func(ctx context.Context, job Job, cause error)
	Logger       *slog.Logger
}

// Runner drains the queue with a bounded pool.
type Runner struct {
	queue    *Queue
	handlers map[string]Handler
	opts     RunnerOptions
	log      *slog.Logger

	// wake carries "there might be work now". It is buffered to one and sent
	// without blocking, because a nudge that arrives while the runner is
	// already awake is a nudge it does not need.
	wake chan struct{}

	mu      sync.Mutex
	started bool
	wg      sync.WaitGroup
}

// NewRunner builds a runner over a queue.
func NewRunner(queue *Queue, opts RunnerOptions) *Runner {
	if opts.Workers < 1 {
		opts.Workers = 1
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 5 * time.Second
	}
	if opts.LeaseTimeout <= 0 {
		opts.LeaseTimeout = 10 * time.Minute
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Runner{
		queue:    queue,
		handlers: map[string]Handler{},
		opts:     opts,
		log:      opts.Logger,
		wake:     make(chan struct{}, 1),
	}
}

// Handle registers the handler for a kind. It panics on a duplicate
// registration, which is a wiring mistake and cannot be one at runtime.
func (r *Runner) Handle(kind string, h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		panic("jobs: Handle after Start")
	}
	if _, exists := r.handlers[kind]; exists {
		panic("jobs: two handlers registered for " + kind)
	}
	r.handlers[kind] = h
}

// Notify tells the runner that work may be waiting, without blocking.
//
// Enqueueing does not go through the runner, so this is how a webhook's
// enqueue becomes a sync that starts now rather than at the next tick.
func (r *Runner) Notify() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Start launches the pool and returns. Stop waits for it.
//
// A run that a cancelled ctx interrupts leaves its job locked; the next
// process's reclaim sweep returns it to pending, which is the same path a
// killed process takes.
func (r *Runner) Start(ctx context.Context) {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return
	}
	r.started = true
	r.mu.Unlock()

	// Anything still locked belongs to a previous process. Sweeping at startup
	// is what makes a restart mid-job cost one lease rather than forever.
	if n, err := r.queue.reclaim(ctx, r.opts.LeaseTimeout); err != nil {
		r.log.Error("reclaim stale jobs", "error", err)
	} else if n > 0 {
		r.log.Warn("reclaimed jobs from a previous run", "count", n)
	}

	for i := range r.opts.Workers {
		name := fmt.Sprintf("worker-%d", i+1)
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.work(ctx, name)
		}()
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.sweep(ctx)
	}()
}

// Stop waits for the workers to finish, bounded by ctx.
func (r *Runner) Stop(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return errors.New("jobs: workers did not finish before the shutdown deadline")
	}
}

// work is one worker: claim, run, record, repeat.
func (r *Runner) work(ctx context.Context, name string) {
	ticker := time.NewTicker(r.opts.PollInterval)
	defer ticker.Stop()

	for {
		// Drain rather than take one per tick, so a burst of thirty forwarded
		// emails does not take thirty ticks to clear.
		for {
			ran, err := r.runOne(ctx, name)
			if err != nil {
				r.log.Error("claim job", "error", err, "worker", name)
				break
			}
			if !ran || ctx.Err() != nil {
				break
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-r.wake:
		}
	}
}

// runOne claims and runs a single job, reporting whether there was one.
func (r *Runner) runOne(ctx context.Context, worker string) (bool, error) {
	job, ok, err := r.queue.claim(ctx, worker)
	if err != nil || !ok {
		return false, err
	}

	log := r.log.With("job_id", job.ID, "kind", job.Kind, "attempt", job.Attempts)

	handler, known := r.handlers[job.Kind]
	if !known {
		// A payload for a handler this build does not have. Failing it outright
		// rather than retrying is right: a rolled-back deployment should not
		// spin on work it cannot do, and the row stays as the record.
		if _, err := r.queue.fail(ctx, Job{ID: job.ID, Attempts: job.MaxAttempts, MaxAttempts: job.MaxAttempts},
			fmt.Errorf("no handler registered for %q", job.Kind)); err != nil {
			log.Error("record unknown job kind", "error", err)
		}
		log.Error("no handler for job kind")
		return true, nil
	}

	start := time.Now()
	err = run(ctx, handler, job)
	switch {
	case err == nil:
		if err := r.queue.complete(ctx, job.ID); err != nil {
			log.Error("mark job done", "error", err)
		}
		log.Info("job done", "duration", time.Since(start))
	default:
		retrying, failErr := r.queue.fail(ctx, job, err)
		if failErr != nil {
			log.Error("record job failure", "error", failErr)
		}
		if retrying {
			log.Warn("job failed, will retry", "error", err, "duration", time.Since(start))
		} else {
			// Out of attempts. Nothing retries the row and nothing else reads
			// it, so this callback is the only voice a dead letter has.
			log.Error("job failed for the last time", "error", err, "duration", time.Since(start))
			if r.opts.OnDeadLetter != nil {
				r.opts.OnDeadLetter(ctx, job, err)
			}
		}
	}
	return true, nil
}

// run calls a handler, turning a panic into an error.
//
// A handler that panics must fail its job, not take the worker with it. Without
// this the pool silently shrinks by one every time an extraction hits a nil
// pointer, and the queue quietly stops draining.
func run(ctx context.Context, h Handler, job Job) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("panic: %v\n%s", v, debug.Stack())
		}
	}()
	return h(ctx, job)
}

// sweep returns abandoned jobs to the pending pool while the process runs.
//
// The startup reclaim covers a restart. This covers the case that has no
// restart: a worker wedged on a network call that never returns, holding a job
// nobody else can take.
func (r *Runner) sweep(ctx context.Context) {
	interval := max(r.opts.LeaseTimeout/2, time.Minute)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := r.queue.reclaim(ctx, r.opts.LeaseTimeout)
			if err != nil {
				r.log.Error("reclaim stale jobs", "error", err)
				continue
			}
			if n > 0 {
				r.log.Warn("reclaimed stalled jobs", "count", n)
				r.Notify()
			}
		}
	}
}
