package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/farrellm/rental-bot/internal/alert"
	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/store"
)

// KeyBudgetExceeded names the condition "this month's token budget is spent".
//
// One key, not one per refused call: a budget that has run out refuses every
// call until the month turns, and a message per refusal is the noise §8.3
// exists to prevent.
const KeyBudgetExceeded = "llm.budget.exceeded"

// ErrBudgetExceeded reports that the month's tokens are spent.
//
// It is not a failure of the work. A caller that gets it should stop and come
// back later rather than retry, because retrying spends nothing and fixes
// nothing.
var ErrBudgetExceeded = errors.New("llm: the monthly token budget is spent")

// Budget is §5.3's circuit breaker: a cap that trips and says so, rather than
// quietly running up a bill.
//
// The ledger it reads is `ingest_proposals`. Every call this package makes on
// the pipeline's behalf lands on a proposal row, so one sum over the current
// month is the whole spend — there is no second table to keep in step, and a
// row deleted with its message stops counting, which is the right answer.
//
// A nil *Budget is a budget with no limit. That is what a test gets and what a
// host that set the limit to zero asked for, and it means no call site has to
// check.
type Budget struct {
	repo   *store.Repo
	limit  int64
	alerts alert.Publisher
	log    *slog.Logger
	// now is injectable so a test can cross a month boundary without waiting
	// for one.
	now func() time.Time

	// mu guards the cached reading. The sum is one indexed query, but every
	// worker in the pool would run it on every call, and the answer only
	// changes when this process spends.
	mu      sync.Mutex
	spent   int64
	readAt  time.Time
	tripped bool
}

// NewBudget builds the breaker. A limit of zero or less means no cap.
func NewBudget(repo *store.Repo, limit int64, alerts alert.Publisher, logger *slog.Logger) *Budget {
	if limit <= 0 {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Budget{
		repo:   repo,
		limit:  limit,
		alerts: alerts,
		log:    logger,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// Check reports whether there is budget left to spend.
//
// A read that fails is not a refusal. The breaker exists to stop a runaway
// bill, and a database blip is not one; refusing to ingest because a COUNT did
// not come back would turn a small problem into a stopped pipeline.
func (b *Budget) Check(ctx context.Context) error {
	if b == nil {
		return nil
	}

	spent, err := b.reading(ctx)
	if err != nil {
		b.log.Error("could not read the token spend; letting the call through", "error", err)
		return nil
	}
	if spent < b.limit {
		b.clear(ctx)
		return nil
	}

	b.trip(ctx, spent)
	return fmt.Errorf("%w: %d of %d tokens this month", ErrBudgetExceeded, spent, b.limit)
}

// Spend records what a call cost, so the next Check does not need the database.
func (b *Budget) Spend(ctx context.Context, u Usage) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.spent += u.Total()
	spent := b.spent
	b.mu.Unlock()

	if spent >= b.limit {
		b.trip(ctx, spent)
	}
}

// reading returns the month's spend, from the database at most once a minute.
func (b *Budget) reading(ctx context.Context) (int64, error) {
	b.mu.Lock()
	fresh := !b.readAt.IsZero() &&
		b.now().Sub(b.readAt) < time.Minute &&
		b.readAt.After(monthStart(b.now()))
	spent := b.spent
	b.mu.Unlock()
	if fresh {
		return spent, nil
	}

	since := domain.Stamp(monthStart(b.now()))
	total, err := b.repo.Read().SumProposalTokensSince(ctx, since)
	if err != nil {
		return 0, fmt.Errorf("llm: read the token spend: %w", err)
	}

	b.mu.Lock()
	b.spent = total
	b.readAt = b.now()
	b.mu.Unlock()
	return total, nil
}

// trip raises the condition, once.
func (b *Budget) trip(ctx context.Context, spent int64) {
	b.mu.Lock()
	already := b.tripped
	b.tripped = true
	b.mu.Unlock()
	if already {
		return
	}

	alert.Publish(ctx, b.alerts, alert.Alert{
		Key:      KeyBudgetExceeded,
		Severity: alert.Critical,
		Title:    "The monthly LLM budget is spent",
		Detail: alert.Errorf(
			"%d of %d tokens used this month. Forwarded mail is being archived but not read; it will be read on the sweep once the month turns or the budget is raised.",
			spent, b.limit),
	})
}

// clear says the condition has passed, which happens at the turn of a month or
// when somebody raises the limit.
func (b *Budget) clear(ctx context.Context) {
	b.mu.Lock()
	tripped := b.tripped
	b.tripped = false
	b.mu.Unlock()
	if !tripped {
		return
	}
	alert.Resolve(ctx, b.alerts, KeyBudgetExceeded, "The monthly LLM budget is spent")
}

// monthStart is the first instant of t's UTC month, which is the window the
// budget is measured over.
func monthStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}
