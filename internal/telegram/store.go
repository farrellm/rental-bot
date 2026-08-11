package telegram

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// pairingAlphabet is what a pairing code is drawn from.
//
// Crockford's set, without I, L, O, U: the operator reads this off one screen
// and types it into another, usually on a phone, and a code where 0 and O are
// both possible is a code that gets typed wrong. U is out because four random
// letters occasionally spell something.
const pairingAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// pairingCodeLength is 8 characters from a 32-symbol alphabet: 40 bits, over a
// ten-minute window, against a bot nobody has the username of. The single-use
// guard in SQL is what actually holds; this is what makes guessing pointless.
const pairingCodeLength = 8

// errorLimit is what a persisted error is cut to, the same 500 characters the
// queue and the Gmail store use.
const errorLimit = 500

// State is the channel as a screen reads it.
type State struct {
	// ChatID is nil until somebody pairs.
	ChatID     *int64
	PairedAt   *time.Time
	MutedUntil *time.Time
	LastSentAt *time.Time
	LastError  string
	Status     string
	// PairingExpiresAt is when the outstanding code lapses, if there is one.
	// The code itself is never here: only its hash is stored.
	PairingExpiresAt *time.Time
	LastUpdateID     int64
}

// Paired reports whether a chat is receiving.
func (s State) Paired() bool { return s.ChatID != nil }

// Muted reports whether everything below critical is being suppressed.
func (s State) Muted(now time.Time) bool {
	return s.MutedUntil != nil && s.MutedUntil.After(now)
}

// PairingOpen reports whether a code is outstanding and still good.
func (s State) PairingOpen(now time.Time) bool {
	return s.PairingExpiresAt != nil && s.PairingExpiresAt.After(now)
}

// Store owns the telegram_state row.
type Store struct {
	repo *store.Repo
	ttl  time.Duration
	now  func() time.Time
}

// NewStore builds the store. ttl bounds a pairing code's life.
func NewStore(repo *store.Repo, ttl time.Duration) *Store {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &Store{
		repo: repo,
		ttl:  ttl,
		now:  func() time.Time { return time.Now().UTC() },
	}
}

// State reads the channel, or ErrNotPaired when nothing has been set up at all.
//
// A row that exists with a null chat is not ErrNotPaired from here — it is a
// pairing in progress, and the screen wants to see the code's expiry. Callers
// that need a chat check State.Paired.
func (s *Store) State(ctx context.Context) (State, error) {
	row, err := s.repo.Read().GetTelegramState(ctx)
	if store.NotFound(err) {
		return State{Status: "unpaired"}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("telegram: read the channel: %w", err)
	}
	return State{
		ChatID:           row.ChatID,
		PairedAt:         optionalTime(row.PairedAt),
		MutedUntil:       optionalTime(row.MutedUntil),
		LastSentAt:       optionalTime(row.LastSentAt),
		LastError:        row.LastError,
		Status:           row.Status,
		PairingExpiresAt: optionalTime(row.PairingExpiresAt),
		LastUpdateID:     row.LastUpdateID,
	}, nil
}

// Chat is the paired chat, or ErrNotPaired.
func (s *Store) Chat(ctx context.Context) (int64, error) {
	state, err := s.State(ctx)
	if err != nil {
		return 0, err
	}
	if !state.Paired() {
		return 0, ErrNotPaired
	}
	return *state.ChatID, nil
}

// IssuePairingCode mints a code and returns it once.
//
// Once, literally: only the SHA-256 goes in the database, so this is the only
// moment the code exists in a readable form. It is written to the log and
// handed to the intake screen, and after that it is unrecoverable — which is
// the same property sessions have, for the same reason.
//
// Issuing over an existing pairing is refused. §8.2 puts re-pairing behind
// server access, and an endpoint that could mint a code for an already-paired
// bot would be a way around that.
func (s *Store) IssuePairingCode(ctx context.Context) (code string, expires time.Time, err error) {
	at := s.now()

	if err := s.ensure(ctx, at); err != nil {
		return "", time.Time{}, err
	}
	state, err := s.State(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	if state.Paired() {
		return "", time.Time{}, ErrAlreadyPaired
	}

	code, err = newPairingCode()
	if err != nil {
		return "", time.Time{}, err
	}
	expires = at.Add(s.ttl)

	if err := s.repo.Write().SetTelegramPairingCode(ctx, sqlc.SetTelegramPairingCodeParams{
		PairingCodeHash:  hashCode(code),
		PairingExpiresAt: domain.Stamp(expires),
		UpdatedAt:        domain.Stamp(at),
	}); err != nil {
		return "", time.Time{}, fmt.Errorf("telegram: store the pairing code: %w", err)
	}
	return code, expires, nil
}

// Pair binds a chat to a code, once.
//
// The guard is in the UPDATE, not here: the hash has to match, the expiry has
// to be in the future, and the chat has to be unset, all evaluated by the
// statement that clears them. A read-then-write would let two updates arriving
// together both pass the read.
func (s *Store) Pair(ctx context.Context, code string, chatID int64) error {
	at := s.now()

	paired, err := s.repo.Write().PairTelegram(ctx, sqlc.PairTelegramParams{
		ChatID:          chatID,
		PairedAt:        domain.Stamp(at),
		UpdatedAt:       domain.Stamp(at),
		PairingCodeHash: hashCode(code),
		Now:             domain.Stamp(at),
	})
	if err != nil {
		return fmt.Errorf("telegram: pair the chat: %w", err)
	}
	if paired == 0 {
		return ErrBadPairingCode
	}
	return nil
}

// Unpair forgets the chat and everything about it.
//
// Reachable only from the command line (§8.2). It drops the row rather than
// nulling the chat, because a half-cleared pairing is a state nothing else
// knows how to read.
func (s *Store) Unpair(ctx context.Context) error {
	removed, err := s.repo.Write().DeleteTelegramState(ctx)
	if err != nil {
		return fmt.Errorf("telegram: unpair: %w", err)
	}
	if removed == 0 {
		return ErrNotPaired
	}
	return nil
}

// Mute suppresses everything below critical until until. A zero time unmutes.
func (s *Store) Mute(ctx context.Context, until time.Time) error {
	at := s.now()
	if err := s.ensure(ctx, at); err != nil {
		return err
	}

	var value *string
	if !until.IsZero() {
		v := domain.Stamp(until)
		value = &v
	}
	if err := s.repo.Write().SetTelegramMute(ctx, sqlc.SetTelegramMuteParams{
		MutedUntil: value,
		UpdatedAt:  domain.Stamp(at),
	}); err != nil {
		return fmt.Errorf("telegram: set the mute: %w", err)
	}
	return nil
}

// SetCursor records the last update seen, so a restart does not replay it.
func (s *Store) SetCursor(ctx context.Context, updateID int64) error {
	if err := s.repo.Write().SetTelegramCursor(ctx, sqlc.SetTelegramCursorParams{
		LastUpdateID: updateID,
		UpdatedAt:    domain.Stamp(s.now()),
	}); err != nil {
		return fmt.Errorf("telegram: advance the update cursor: %w", err)
	}
	return nil
}

// RecordSent notes a delivery, which clears any degradation in the same
// statement — recovery costs no second write and cannot be forgotten.
func (s *Store) RecordSent(ctx context.Context) error {
	at := s.now()
	if err := s.repo.Write().RecordTelegramSent(ctx, sqlc.RecordTelegramSentParams{
		LastSentAt: domain.Stamp(at),
		UpdatedAt:  domain.Stamp(at),
	}); err != nil {
		return fmt.Errorf("telegram: record the delivery: %w", err)
	}
	return nil
}

// RecordFailure marks the channel degraded with what went wrong.
//
// Not fatal to the process: the last known state stays on the card, annotated,
// the way §6.1 argues for valuations and the Gmail store already does.
func (s *Store) RecordFailure(ctx context.Context, cause error) error {
	at := s.now()
	if err := s.repo.Write().SetTelegramStatus(ctx, sqlc.SetTelegramStatusParams{
		Status:    "degraded",
		LastError: domain.Truncate(cause.Error(), errorLimit),
		UpdatedAt: domain.Stamp(at),
	}); err != nil {
		return fmt.Errorf("telegram: record the failure: %w", err)
	}
	return nil
}

// ensure creates the singleton row so every narrow UPDATE has something to
// update.
func (s *Store) ensure(ctx context.Context, at time.Time) error {
	if err := s.repo.Write().EnsureTelegramState(ctx, sqlc.EnsureTelegramStateParams{
		CreatedAt: domain.Stamp(at),
		UpdatedAt: domain.Stamp(at),
	}); err != nil {
		return fmt.Errorf("telegram: create the channel row: %w", err)
	}
	return nil
}

// newPairingCode draws a code from crypto/rand.
//
// rand.Text would be shorter, but it uses its own alphabet, and the point of
// pairingAlphabet is that a person reads this off a screen and types it into a
// phone.
func newPairingCode() (string, error) {
	buf := make([]byte, pairingCodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("telegram: generate a pairing code: %w", err)
	}
	out := make([]byte, pairingCodeLength)
	for i, b := range buf {
		out[i] = pairingAlphabet[int(b)%len(pairingAlphabet)]
	}
	// A dash in the middle, because eight characters in one run is eight
	// characters somebody loses their place in.
	return string(out[:4]) + "-" + string(out[4:]), nil
}

func hashCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

func optionalTime(value *string) *time.Time {
	if value == nil || *value == "" {
		return nil
	}
	at, err := time.Parse(time.RFC3339, *value)
	if err != nil {
		return nil
	}
	return &at
}

// NotPaired reports whether err is the "nobody has set this up" answer, which
// every caller treats as a working state rather than a failure.
func NotPaired(err error) bool { return errors.Is(err, ErrNotPaired) }
