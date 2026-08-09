package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/telegram"
)

// unpairTelegram forgets the paired chat.
//
// This is the only way to change who Telegram trusts, and it deliberately
// needs a shell on the host. docs/DESIGN.md §8.2: nothing reachable from
// Telegram can re-pair, because a bot token that leaks grants the ability to
// send *as* the bot — and if a chat command could hand the channel to a new
// chat, that leak would become the ability to command it.
//
// The web API cannot do this either. A hijacked session is a lower bar than a
// shell, and the alerting channel is the thing that would report the hijack.
func unpairTelegram(ctx context.Context, db *store.DB) error {
	channel := telegram.NewStore(db.Repo(), time.Minute)

	state, err := channel.State(ctx)
	if err != nil {
		return fmt.Errorf("unpair-telegram: %w", err)
	}

	switch err := channel.Unpair(ctx); {
	case errors.Is(err, telegram.ErrNotPaired):
		fmt.Println("no chat was paired; nothing to do")
		return nil
	case err != nil:
		return fmt.Errorf("unpair-telegram: %w", err)
	}

	if state.Paired() {
		fmt.Printf("unpaired chat %d; alerts will stop until a new chat pairs\n", *state.ChatID)
	} else {
		fmt.Println("cleared the outstanding pairing code")
	}
	fmt.Println("get a new code from the Intake screen, or restart to have one logged")
	return nil
}
