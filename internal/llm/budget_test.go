package llm

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/farrellm/rental-bot/internal/alert"
	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
	"github.com/farrellm/rental-bot/migrations"
)

// recorder stands in for the alert bus. The breaker's whole job is to say
// something rather than to run up a bill quietly, so what it says is the thing
// worth asserting.
type recorder struct {
	raised   []alert.Alert
	resolved []string
}

func (r *recorder) Publish(_ context.Context, a alert.Alert) { r.raised = append(r.raised, a) }
func (r *recorder) Resolve(_ context.Context, key, _ string) {
	r.resolved = append(r.resolved, key)
}

func openRepo(t *testing.T) *store.Repo {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "rental.db"), 2)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(t.Context(), migrations.FS); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db.Repo()
}

// spend files a proposal carrying tokens, which is the only ledger the breaker
// reads.
func spend(t *testing.T, repo *store.Repo, at time.Time, tokens int64) {
	t.Helper()
	stamp := domain.Stamp(at)
	msg, err := repo.Write().CreateEmailMessage(t.Context(), sqlc.CreateEmailMessageParams{
		GmailMessageID: stamp + "-" + domain.Stamp(time.Now()),
		ReceivedAt:     stamp,
		Status:         "received",
		CreatedAt:      stamp,
		UpdatedAt:      stamp,
	})
	if err != nil {
		t.Fatalf("CreateEmailMessage: %v", err)
	}
	if _, err := repo.Write().CreateProposal(t.Context(), sqlc.CreateProposalParams{
		EmailMessageID: msg.ID,
		Kind:           "receipt",
		Status:         "pending",
		PromptTokens:   tokens,
		CreatedAt:      stamp,
		UpdatedAt:      stamp,
	}); err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
}

// A limit of zero is no limit, and no *Budget at all is no limit either. Both
// have to be safe to call, because that is what a test and an unconfigured
// host get.
func TestNoLimitIsNoBreaker(t *testing.T) {
	if b := NewBudget(openRepo(t), 0, nil, nil); b != nil {
		t.Fatalf("NewBudget with no limit = %v, want nil", b)
	}

	var none *Budget
	if err := none.Check(t.Context()); err != nil {
		t.Fatalf("Check on a nil budget = %v, want nil", err)
	}
	none.Spend(t.Context(), Usage{PromptTokens: 1_000_000})
}

// Under the limit the call goes through; at it the call is refused and the
// condition is raised once, however many calls hit it.
func TestTheBreakerTripsOnceAndRefuses(t *testing.T) {
	repo := openRepo(t)
	bus := &recorder{}
	budget := NewBudget(repo, 1000, bus, nil)

	spend(t, repo, time.Now().UTC(), 400)
	if err := budget.Check(t.Context()); err != nil {
		t.Fatalf("Check under the limit = %v, want nil", err)
	}

	// Spending past the limit trips it without waiting for the next read.
	budget.Spend(t.Context(), Usage{PromptTokens: 500, CompletionTokens: 200})

	err := budget.Check(t.Context())
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("Check over the limit = %v, want ErrBudgetExceeded", err)
	}
	if err := budget.Check(t.Context()); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("second Check = %v, want ErrBudgetExceeded", err)
	}

	if len(bus.raised) != 1 {
		t.Fatalf("raised %d alerts, want exactly one: a spent budget is one condition, not one per refused call", len(bus.raised))
	}
	if got := bus.raised[0]; got.Key != KeyBudgetExceeded || got.Severity != alert.Critical {
		t.Fatalf("alert = %+v, want the budget key at critical", got)
	}
}

// The window is the calendar month, so last month's spend does not hold this
// month's pipeline shut.
func TestTheBudgetIsMeasuredOverTheCurrentMonth(t *testing.T) {
	repo := openRepo(t)
	bus := &recorder{}
	budget := NewBudget(repo, 1000, bus, nil)

	lastMonth := monthStart(time.Now().UTC()).Add(-24 * time.Hour)
	spend(t, repo, lastMonth, 5000)

	if err := budget.Check(t.Context()); err != nil {
		t.Fatalf("Check = %v, want nil: last month's spend is not this month's", err)
	}
	if len(bus.raised) != 0 {
		t.Fatalf("raised %v, want nothing", bus.raised)
	}
}

// A cleared condition is said out loud, once. That is what turns the register
// line from an open matter into one that has been ruled off.
func TestRaisingTheLimitClearsTheCondition(t *testing.T) {
	repo := openRepo(t)
	bus := &recorder{}
	budget := NewBudget(repo, 1000, bus, nil)

	spend(t, repo, time.Now().UTC(), 1500)
	if err := budget.Check(t.Context()); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("Check = %v, want ErrBudgetExceeded", err)
	}

	budget.limit = 5000
	// Past the cached reading's minute, so the next Check goes to the database.
	budget.readAt = time.Time{}

	if err := budget.Check(t.Context()); err != nil {
		t.Fatalf("Check after raising the limit = %v, want nil", err)
	}
	if len(bus.resolved) != 1 || bus.resolved[0] != KeyBudgetExceeded {
		t.Fatalf("resolved = %v, want the budget key once", bus.resolved)
	}
}

// A database blip is not a runaway bill. The breaker exists to stop the
// second, and refusing to read the mail because a SUM did not come back turns
// a small problem into a stopped pipeline.
func TestAnUnreadableLedgerLetsTheCallThrough(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "rental.db"), 2)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := db.Migrate(t.Context(), migrations.FS); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	budget := NewBudget(db.Repo(), 1000, &recorder{}, nil)
	db.Close()

	if err := budget.Check(t.Context()); err != nil {
		t.Fatalf("Check against a closed database = %v, want nil", err)
	}
}
