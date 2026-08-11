package store

import (
	"testing"
	"time"

	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// Cooldown rests on this index: one open row per condition per channel, and the
// key free again once the condition clears. Both halves are worth asserting
// against the real schema rather than being read off the DDL.
func TestOneOpenNotificationPerCondition(t *testing.T) {
	repo := openRepo(t)
	ctx := t.Context()

	params := sqlc.InsertNotificationParams{
		DedupeKey:   "gmail.watch.lapsed",
		Channel:     "telegram",
		Severity:    "warning",
		Title:       "The Gmail watch has lapsed",
		FirstSeenAt: now(),
		CreatedAt:   now(),
		UpdatedAt:   now(),
	}
	first, err := repo.Write().InsertNotification(ctx, params)
	if err != nil {
		t.Fatalf("InsertNotification: %v", err)
	}

	if _, err := repo.Write().InsertNotification(ctx, params); !Conflict(err) {
		t.Fatalf("second open notification under the same key = %v, want a uniqueness conflict", err)
	}

	// A different channel is a different delivery, not a duplicate.
	other := params
	other.Channel = "log"
	if _, err := repo.Write().InsertNotification(ctx, other); err != nil {
		t.Fatalf("InsertNotification on another channel: %v", err)
	}

	closed, err := repo.Write().ResolveNotification(ctx, sqlc.ResolveNotificationParams{
		ResolvedAt: now(),
		UpdatedAt:  now(),
		DedupeKey:  params.DedupeKey,
		Channel:    params.Channel,
	})
	if err != nil {
		t.Fatalf("ResolveNotification: %v", err)
	}
	if closed != 1 {
		t.Fatalf("ResolveNotification closed %d rows, want 1", closed)
	}

	// Resolving again closes nothing, which is what tells a sweep it owes no
	// second recovery message.
	if closed, err := repo.Write().ResolveNotification(ctx, sqlc.ResolveNotificationParams{
		ResolvedAt: now(),
		UpdatedAt:  now(),
		DedupeKey:  params.DedupeKey,
		Channel:    params.Channel,
	}); err != nil || closed != 0 {
		t.Fatalf("second ResolveNotification closed %d rows (err %v), want 0", closed, err)
	}

	// The condition can recur, because the key is free again.
	again, err := repo.Write().InsertNotification(ctx, params)
	if err != nil {
		t.Fatalf("InsertNotification after the condition cleared: %v", err)
	}
	if again.ID == first.ID {
		t.Error("the recurrence reused the resolved row; the register would lose the first occurrence")
	}
}

// A restatement bumps the tally rather than writing a second line.
func TestRestatingANotificationCountsUp(t *testing.T) {
	repo := openRepo(t)
	ctx := t.Context()

	row, err := repo.Write().InsertNotification(ctx, sqlc.InsertNotificationParams{
		DedupeKey:   "jobs.dead_letter",
		Channel:     "telegram",
		Severity:    "critical",
		Title:       "A job exhausted its attempts",
		FirstSeenAt: now(),
		CreatedAt:   now(),
		UpdatedAt:   now(),
	})
	if err != nil {
		t.Fatalf("InsertNotification: %v", err)
	}

	for range 3 {
		if err := repo.Write().RecordNotificationSent(ctx, sqlc.RecordNotificationSentParams{
			LastSentAt: now(),
			Severity:   "critical",
			Title:      "A job exhausted its attempts",
			UpdatedAt:  now(),
			ID:         row.ID,
		}); err != nil {
			t.Fatalf("RecordNotificationSent: %v", err)
		}
	}

	got, err := repo.Read().GetOpenNotification(ctx, sqlc.GetOpenNotificationParams{
		DedupeKey: "jobs.dead_letter",
		Channel:   "telegram",
	})
	if err != nil {
		t.Fatalf("GetOpenNotification: %v", err)
	}
	if got.SendCount != 3 {
		t.Errorf("SendCount = %d, want 3", got.SendCount)
	}
	if got.LastSentAt == nil {
		t.Error("LastSentAt is nil after three sends")
	}

	tally, err := repo.Read().CountNotifications(ctx)
	if err != nil {
		t.Fatalf("CountNotifications: %v", err)
	}
	if tally.Total != 1 || tally.Outstanding != 1 {
		t.Errorf("CountNotifications = %+v, want one row, outstanding", tally)
	}
}

// A pairing code is single-use, and the guard that makes it so lives in the
// UPDATE rather than in a read-then-write a second update could race.
func TestPairingCodeIsSingleUse(t *testing.T) {
	repo := openRepo(t)
	ctx := t.Context()

	if err := repo.Write().EnsureTelegramState(ctx, sqlc.EnsureTelegramStateParams{
		CreatedAt: now(), UpdatedAt: now(),
	}); err != nil {
		t.Fatalf("EnsureTelegramState: %v", err)
	}

	const hash = "0f1e2d3c"
	expires := time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339)
	if err := repo.Write().SetTelegramPairingCode(ctx, sqlc.SetTelegramPairingCodeParams{
		PairingCodeHash:  hash,
		PairingExpiresAt: expires,
		UpdatedAt:        now(),
	}); err != nil {
		t.Fatalf("SetTelegramPairingCode: %v", err)
	}

	pair := sqlc.PairTelegramParams{
		ChatID:          4471,
		PairedAt:        now(),
		UpdatedAt:       now(),
		PairingCodeHash: hash,
		Now:             now(),
	}
	paired, err := repo.Write().PairTelegram(ctx, pair)
	if err != nil {
		t.Fatalf("PairTelegram: %v", err)
	}
	if paired != 1 {
		t.Fatalf("PairTelegram updated %d rows, want 1", paired)
	}

	// The same code again, from a second chat: the hash was cleared by the
	// statement that consumed it, so there is nothing left to match.
	pair.ChatID = 9999
	if paired, err := repo.Write().PairTelegram(ctx, pair); err != nil || paired != 0 {
		t.Fatalf("replaying the pairing code updated %d rows (err %v), want 0", paired, err)
	}

	state, err := repo.Read().GetTelegramState(ctx)
	if err != nil {
		t.Fatalf("GetTelegramState: %v", err)
	}
	if state.ChatID == nil || *state.ChatID != 4471 {
		t.Errorf("ChatID = %v, want the chat that used the code first", state.ChatID)
	}
	if state.Status != "paired" {
		t.Errorf("Status = %q, want paired", state.Status)
	}
	if state.PairingCodeHash != "" {
		t.Error("the pairing code hash survived being used")
	}
}

// An expired code is refused, in SQL, without the caller having to remember to
// check.
func TestExpiredPairingCodeIsRefused(t *testing.T) {
	repo := openRepo(t)
	ctx := t.Context()

	if err := repo.Write().EnsureTelegramState(ctx, sqlc.EnsureTelegramStateParams{
		CreatedAt: now(), UpdatedAt: now(),
	}); err != nil {
		t.Fatalf("EnsureTelegramState: %v", err)
	}

	const hash = "beefcafe"
	if err := repo.Write().SetTelegramPairingCode(ctx, sqlc.SetTelegramPairingCodeParams{
		PairingCodeHash:  hash,
		PairingExpiresAt: time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
		UpdatedAt:        now(),
	}); err != nil {
		t.Fatalf("SetTelegramPairingCode: %v", err)
	}

	paired, err := repo.Write().PairTelegram(ctx, sqlc.PairTelegramParams{
		ChatID:          4471,
		PairedAt:        now(),
		UpdatedAt:       now(),
		PairingCodeHash: hash,
		Now:             now(),
	})
	if err != nil {
		t.Fatalf("PairTelegram: %v", err)
	}
	if paired != 0 {
		t.Errorf("PairTelegram accepted an expired code, updating %d rows", paired)
	}
}
