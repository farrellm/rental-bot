package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
	"github.com/farrellm/rental-bot/migrations"
)

// openQueue returns a queue over a real migrated database. The generated code
// and the partial index are only worth trusting against the real schema.
func openQueue(t *testing.T) (*Queue, *store.Repo) {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "jobs.db"), 2)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.Migrate(t.Context(), migrations.FS); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	repo := db.Repo()
	return NewQueue(repo), repo
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

func TestEnqueueAndRun(t *testing.T) {
	queue, _ := openQueue(t)

	type payload struct {
		Address string `json:"address"`
	}

	got := make(chan string, 1)
	runner := NewRunner(queue, RunnerOptions{Workers: 1, PollInterval: 10 * time.Millisecond, Logger: quietLogger()})
	runner.Handle("test.echo", func(_ context.Context, job Job) error {
		var p payload
		if err := job.Decode(&p); err != nil {
			return err
		}
		got <- p.Address
		return nil
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runner.Start(ctx)

	id, err := queue.Enqueue(t.Context(), "test.echo", payload{Address: "412 Elm St"}, Options{})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	runner.Notify()

	select {
	case address := <-got:
		if address != "412 Elm St" {
			t.Errorf("payload address = %q, want %q", address, "412 Elm St")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the job never ran")
	}

	waitForStatus(t, queue, id, "done")
}

// The dedupe key exists so a Pub/Sub redelivery storm produces one sync.
func TestEnqueueOnceCollapsesDuplicates(t *testing.T) {
	queue, _ := openQueue(t)

	added, err := queue.EnqueueOnce(t.Context(), "gmail.sync", "gmail.sync", nil)
	if err != nil {
		t.Fatalf("EnqueueOnce: %v", err)
	}
	if !added {
		t.Fatal("the first EnqueueOnce reported no job added")
	}

	for range 30 {
		added, err := queue.EnqueueOnce(t.Context(), "gmail.sync", "gmail.sync", nil)
		if err != nil {
			t.Fatalf("EnqueueOnce: %v", err)
		}
		if added {
			t.Fatal("a second job was queued under a key that already had one")
		}
	}

	if got := depth(t, queue)["pending"]; got != 1 {
		t.Errorf("pending jobs = %d, want 1", got)
	}
}

// Two workers must not both come away with one job.
func TestClaimHandsOneJobToOneWorker(t *testing.T) {
	queue, _ := openQueue(t)

	const jobCount = 25
	for range jobCount {
		if _, err := queue.Enqueue(t.Context(), "test.count", nil, Options{}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	var (
		mu      sync.Mutex
		claimed = map[int64]int{}
		wg      sync.WaitGroup
	)
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			worker := "worker-" + string(rune('a'+i))
			for {
				job, ok, err := queue.claim(t.Context(), worker)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if !ok {
					return
				}
				mu.Lock()
				claimed[job.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(claimed) != jobCount {
		t.Errorf("claimed %d distinct jobs, want %d", len(claimed), jobCount)
	}
	for id, times := range claimed {
		if times != 1 {
			t.Errorf("job %d was claimed %d times, want once", id, times)
		}
	}
}

func TestFailureRetriesThenGivesUp(t *testing.T) {
	queue, _ := openQueue(t)

	var attempts atomic.Int64
	runner := NewRunner(queue, RunnerOptions{Workers: 1, PollInterval: 10 * time.Millisecond, Logger: quietLogger()})
	runner.Handle("test.fail", func(context.Context, Job) error {
		attempts.Add(1)
		return errors.New("the vendor's server said no")
	})

	// The backoff would hold a retry for half a minute, so this test drives the
	// runner one claim at a time rather than starting the pool.
	id, err := queue.Enqueue(t.Context(), "test.fail", nil, Options{MaxAttempts: 3})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	for range 3 {
		// Pretend the backoff has elapsed. The first pass is a no-op; the two
		// after it pull run_after back to now so the job is claimable again.
		if err := queue.repo.Write().RetryJob(t.Context(), sqlc.RetryJobParams{
			RunAfter: domain.Stamp(time.Now()), UpdatedAt: domain.Stamp(time.Now()), ID: id,
		}); err != nil {
			t.Fatalf("make the job runnable: %v", err)
		}
		ran, err := runner.runOne(t.Context(), "worker-1")
		if err != nil {
			t.Fatalf("runOne: %v", err)
		}
		if !ran {
			t.Fatal("runOne found no job")
		}
	}

	if got := attempts.Load(); got != 3 {
		t.Errorf("handler ran %d times, want 3", got)
	}

	job, err := queue.repo.Read().GetJob(t.Context(), id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != "failed" {
		t.Errorf("status = %q, want failed", job.Status)
	}
	if job.LastError == "" {
		t.Error("a failed job kept no record of what went wrong")
	}
}

// Nothing retries a dead letter and nothing else reads the row, so this
// callback is the only voice it has.
func TestDeadLetterIsReportedOnce(t *testing.T) {
	queue, _ := openQueue(t)

	var (
		mu    sync.Mutex
		dead  []Job
		cause error
	)
	runner := NewRunner(queue, RunnerOptions{
		Workers: 1, PollInterval: 10 * time.Millisecond, Logger: quietLogger(),
		OnDeadLetter: func(_ context.Context, job Job, err error) {
			mu.Lock()
			defer mu.Unlock()
			dead = append(dead, job)
			cause = err
		},
	})
	runner.Handle("test.doomed", func(context.Context, Job) error {
		return errors.New("the vendor's server said no")
	})

	// Two attempts: the first retries and must say nothing, the second is the
	// last and must report.
	id, err := queue.Enqueue(t.Context(), "test.doomed", nil, Options{MaxAttempts: 2})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	for range 2 {
		if err := queue.repo.Write().RetryJob(t.Context(), sqlc.RetryJobParams{
			RunAfter: domain.Stamp(time.Now()), UpdatedAt: domain.Stamp(time.Now()), ID: id,
		}); err != nil {
			t.Fatalf("make the job runnable: %v", err)
		}
		if _, err := runner.runOne(t.Context(), "worker-1"); err != nil {
			t.Fatalf("runOne: %v", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(dead) != 1 {
		t.Fatalf("OnDeadLetter ran %d times, want 1: a retry is not a dead letter", len(dead))
	}
	if dead[0].ID != id || dead[0].Kind != "test.doomed" {
		t.Errorf("OnDeadLetter got job %+v, want id %d of kind test.doomed", dead[0], id)
	}
	if cause == nil {
		t.Error("OnDeadLetter was not told what went wrong")
	}
}

// A runner with no callback is the ordinary case, and must not panic on it.
func TestDeadLetterWithoutACallback(t *testing.T) {
	queue, _ := openQueue(t)

	runner := NewRunner(queue, RunnerOptions{Workers: 1, PollInterval: 10 * time.Millisecond, Logger: quietLogger()})
	runner.Handle("test.doomed", func(context.Context, Job) error {
		return errors.New("no")
	})
	if _, err := queue.Enqueue(t.Context(), "test.doomed", nil, Options{MaxAttempts: 1}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := runner.runOne(t.Context(), "worker-1"); err != nil {
		t.Fatalf("runOne: %v", err)
	}
}

// A handler that panics must fail its job, not take the worker with it.
func TestPanicBecomesAFailure(t *testing.T) {
	queue, _ := openQueue(t)

	runner := NewRunner(queue, RunnerOptions{Workers: 1, PollInterval: 10 * time.Millisecond, Logger: quietLogger()})
	runner.Handle("test.panic", func(context.Context, Job) error {
		panic("nil map write")
	})

	id, err := queue.Enqueue(t.Context(), "test.panic", nil, Options{MaxAttempts: 1})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := runner.runOne(t.Context(), "worker-1"); err != nil {
		t.Fatalf("runOne: %v", err)
	}

	job, err := queue.repo.Read().GetJob(t.Context(), id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != "failed" {
		t.Errorf("status = %q, want failed", job.Status)
	}
}

// A payload for a handler this build does not have fails outright rather than
// spinning: a rolled-back deployment should not retry work it cannot do.
func TestUnknownKindFailsWithoutRetrying(t *testing.T) {
	queue, _ := openQueue(t)
	runner := NewRunner(queue, RunnerOptions{Workers: 1, Logger: quietLogger()})

	id, err := queue.Enqueue(t.Context(), "test.from-the-future", nil, Options{})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := runner.runOne(t.Context(), "worker-1"); err != nil {
		t.Fatalf("runOne: %v", err)
	}

	job, err := queue.repo.Read().GetJob(t.Context(), id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != "failed" {
		t.Errorf("status = %q, want failed", job.Status)
	}
}

// A job held by a process that died must not stay held.
func TestStaleLocksAreReclaimed(t *testing.T) {
	queue, _ := openQueue(t)

	if _, err := queue.Enqueue(t.Context(), "test.stalled", nil, Options{}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	job, ok, err := queue.claim(t.Context(), "worker-that-died")
	if err != nil || !ok {
		t.Fatalf("claim: %v, ok=%v", err, ok)
	}

	// Nothing is claimable while the lease holds.
	if _, ok, err := queue.claim(t.Context(), "worker-2"); err != nil || ok {
		t.Fatalf("a locked job was claimable: ok=%v err=%v", ok, err)
	}

	// The lease has expired: pretend the claim was an hour ago.
	queue.now = func() time.Time { return time.Now().UTC().Add(time.Hour) }
	n, err := queue.reclaim(t.Context(), 10*time.Minute)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d jobs, want 1", n)
	}

	reclaimed, ok, err := queue.claim(t.Context(), "worker-2")
	if err != nil || !ok {
		t.Fatalf("the reclaimed job was not claimable: %v ok=%v", err, ok)
	}
	if reclaimed.ID != job.ID {
		t.Errorf("claimed job %d, want %d", reclaimed.ID, job.ID)
	}
	// The claim spent an attempt each time, which is what stops a job that
	// kills its worker from retrying forever.
	if reclaimed.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", reclaimed.Attempts)
	}
}

// A job scheduled for later is not runnable now.
func TestRunAfterDelaysAJob(t *testing.T) {
	queue, _ := openQueue(t)

	if _, err := queue.Enqueue(t.Context(), "test.later", nil, Options{
		RunAfter: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, ok, err := queue.claim(t.Context(), "worker-1"); err != nil || ok {
		t.Fatalf("a job scheduled for an hour from now was claimable: ok=%v err=%v", ok, err)
	}
}

func TestBackoffGrowsAndStaysCapped(t *testing.T) {
	// Full jitter means the wait is between half the target and the target, so
	// the assertion is on the band rather than on a value.
	for _, attempt := range []int64{1, 2, 3, 8, 20} {
		target := min(time.Duration(float64(backoffBase)*pow2(attempt-1)), backoffCap)
		got := backoff(attempt)
		if got < target/2 || got > target {
			t.Errorf("backoff(%d) = %s, want between %s and %s", attempt, got, target/2, target)
		}
	}
}

func pow2(n int64) float64 {
	out := 1.0
	for range n {
		out *= 2
	}
	return out
}

func depth(t *testing.T, queue *Queue) map[string]int64 {
	t.Helper()
	got, err := queue.Depth(t.Context())
	if err != nil {
		t.Fatalf("Depth: %v", err)
	}
	return got
}

func waitForStatus(t *testing.T, queue *Queue, id int64, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		job, err := queue.repo.Read().GetJob(t.Context(), id)
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		if job.Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %d never reached %q", id, want)
}
