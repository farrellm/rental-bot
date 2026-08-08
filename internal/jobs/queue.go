// Package jobs is the SQLite-backed work queue and the pool that drains it.
//
// Everything the webhook and the scheduler want done is enqueued rather than
// run inline (docs/DESIGN.md §2). A Pub/Sub push has a delivery deadline and a
// Gmail history walk does not fit inside it; enqueueing means the push is
// answered in milliseconds and the walk happens on a worker.
//
// The queue is a table rather than a channel because the work has to survive a
// restart. A forwarded receipt that arrived while the process was being
// upgraded is not allowed to disappear.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"time"

	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// Defaults for an enqueue that does not say otherwise.
const (
	defaultMaxAttempts = 5
	// The first retry waits this long; each one after doubles it.
	backoffBase = 30 * time.Second
	// No retry waits longer than this. A Gmail outage should be retried every
	// ten minutes, not every six hours.
	backoffCap = 10 * time.Minute
)

// ErrDuplicate reports that a job with this dedupe key is already pending.
//
// It is not a failure. A caller that enqueues "gmail.sync" thirty times during
// a Pub/Sub redelivery storm wanted one sync, and got one.
var ErrDuplicate = errors.New("jobs: a job with that dedupe key is already queued")

// Job is one unit of work as the queue holds it.
type Job struct {
	ID          int64
	Kind        string
	Payload     string
	Attempts    int64
	MaxAttempts int64
	LastError   string
}

// Decode unmarshals the payload into v.
func (j Job) Decode(v any) error {
	if err := json.Unmarshal([]byte(j.Payload), v); err != nil {
		return fmt.Errorf("jobs: decode %s payload: %w", j.Kind, err)
	}
	return nil
}

// Options tune one enqueue.
type Options struct {
	// RunAfter delays the job. Zero means now.
	RunAfter time.Time
	// DedupeKey, when set, allows at most one pending job under it. The
	// database enforces this with a partial unique index, so two goroutines
	// racing produce one job and one ErrDuplicate rather than two jobs.
	DedupeKey string
	// MaxAttempts overrides the default of five.
	MaxAttempts int
}

// Queue enqueues work and is the only thing that writes the jobs table outside
// the runner.
type Queue struct {
	repo *store.Repo
	// now is injectable so a test can assert a backoff schedule without
	// sleeping through it.
	now func() time.Time
}

// NewQueue binds a queue to a repository.
func NewQueue(repo *store.Repo) *Queue {
	return &Queue{repo: repo, now: func() time.Time { return time.Now().UTC() }}
}

// Enqueue adds a job, returning ErrDuplicate when its dedupe key is taken.
func (q *Queue) Enqueue(ctx context.Context, kind string, payload any, opts Options) (int64, error) {
	body := "{}"
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return 0, fmt.Errorf("jobs: encode %s payload: %w", kind, err)
		}
		body = string(encoded)
	}

	runAfter := opts.RunAfter
	if runAfter.IsZero() {
		runAfter = q.now()
	}
	maxAttempts := int64(opts.MaxAttempts)
	if maxAttempts < 1 {
		maxAttempts = defaultMaxAttempts
	}

	var dedupe *string
	if opts.DedupeKey != "" {
		dedupe = &opts.DedupeKey
	}

	stampNow := stamp(q.now())
	job, err := q.repo.Write().EnqueueJob(ctx, sqlc.EnqueueJobParams{
		Kind:        kind,
		Payload:     body,
		DedupeKey:   dedupe,
		RunAfter:    stamp(runAfter),
		MaxAttempts: maxAttempts,
		CreatedAt:   stampNow,
		UpdatedAt:   stampNow,
	})
	if store.Conflict(err) {
		return 0, fmt.Errorf("%w: %s", ErrDuplicate, opts.DedupeKey)
	}
	if err != nil {
		return 0, fmt.Errorf("jobs: enqueue %s: %w", kind, err)
	}
	return job.ID, nil
}

// EnqueueOnce is Enqueue for a job that is pointless to queue twice, and
// reports whether it added one.
//
// The caller almost always wants this rather than to treat a duplicate as an
// error: "a sync is already queued" is the desired outcome, not a problem.
func (q *Queue) EnqueueOnce(ctx context.Context, kind, dedupeKey string, payload any) (bool, error) {
	_, err := q.Enqueue(ctx, kind, payload, Options{DedupeKey: dedupeKey})
	if errors.Is(err, ErrDuplicate) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// claim takes the oldest runnable job, or reports none.
func (q *Queue) claim(ctx context.Context, worker string) (Job, bool, error) {
	row, err := q.repo.Write().ClaimJob(ctx, sqlc.ClaimJobParams{
		Now:    stamp(q.now()),
		Worker: worker,
	})
	if store.NotFound(err) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("jobs: claim: %w", err)
	}
	return Job{
		ID: row.ID, Kind: row.Kind, Payload: row.Payload,
		Attempts: row.Attempts, MaxAttempts: row.MaxAttempts, LastError: row.LastError,
	}, true, nil
}

// complete marks a job done.
func (q *Queue) complete(ctx context.Context, id int64) error {
	err := q.repo.Write().CompleteJob(ctx, sqlc.CompleteJobParams{UpdatedAt: stamp(q.now()), ID: id})
	if err != nil {
		return fmt.Errorf("jobs: complete %d: %w", id, err)
	}
	return nil
}

// fail either schedules a retry or gives up, depending on attempts left.
func (q *Queue) fail(ctx context.Context, job Job, cause error) (retrying bool, err error) {
	detail := cause.Error()
	// The column is small on purpose: it is a breadcrumb to the logs, not the
	// logs. A 40 KB error from a rejected HTTP body does not belong in a row
	// every screen reads.
	if len(detail) > 500 {
		detail = detail[:500] + "..."
	}
	stampNow := stamp(q.now())

	if job.Attempts >= job.MaxAttempts {
		if err := q.repo.Write().FailJob(ctx, sqlc.FailJobParams{
			LastError: detail, UpdatedAt: stampNow, ID: job.ID,
		}); err != nil {
			return false, fmt.Errorf("jobs: fail %d: %w", job.ID, err)
		}
		return false, nil
	}

	if err := q.repo.Write().RetryJob(ctx, sqlc.RetryJobParams{
		RunAfter:  stamp(q.now().Add(backoff(job.Attempts))),
		LastError: detail,
		UpdatedAt: stampNow,
		ID:        job.ID,
	}); err != nil {
		return false, fmt.Errorf("jobs: retry %d: %w", job.ID, err)
	}
	return true, nil
}

// reclaim returns jobs whose worker stopped reporting to the pending pool.
func (q *Queue) reclaim(ctx context.Context, lease time.Duration) (int64, error) {
	n, err := q.repo.Write().ReclaimStaleJobs(ctx, sqlc.ReclaimStaleJobsParams{
		UpdatedAt: stamp(q.now()),
		OlderThan: stamp(q.now().Add(-lease)),
	})
	if err != nil {
		return 0, fmt.Errorf("jobs: reclaim: %w", err)
	}
	return n, nil
}

// Depth reports how many jobs sit in each state, for the status endpoint.
func (q *Queue) Depth(ctx context.Context) (map[string]int64, error) {
	rows, err := q.repo.Read().CountJobsByStatus(ctx)
	if err != nil {
		return nil, fmt.Errorf("jobs: depth: %w", err)
	}
	out := make(map[string]int64, len(rows))
	for _, row := range rows {
		out[row.Status] = row.Count
	}
	return out, nil
}

// backoff is the wait before retry number `attempts`.
//
// Exponential with full jitter: two jobs that failed against the same outage
// must not come back at the same instant and fail against it together.
func backoff(attempts int64) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	// 2^30 backoffBase overflows nothing here, but the cap makes the shift
	// pointless past a handful of attempts anyway.
	shift := min(attempts-1, 20)
	wait := time.Duration(math.Min(
		float64(backoffCap),
		float64(backoffBase)*math.Pow(2, float64(shift)),
	))
	return wait/2 + time.Duration(rand.Int64N(int64(wait/2)+1))
}

// stamp renders a timestamp the way every other column in this schema holds
// one: RFC3339, UTC.
//
// run_after and locked_at are compared with < and <= in SQL rather than parsed
// first, which works only because RFC3339 at a fixed UTC offset sorts
// lexicographically the way it sorts chronologically. Changing this format is
// not a formatting change; it is a change to how the queue picks its next job.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }
