package telegram

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// unauthorizedLogEvery throttles the line for an update from a chat that is
// not the paired one.
//
// Bot usernames get discovered and probed, and one log line per probe turns
// the journal into the prober's output. §8.2 asks for these to be counted and
// logged at a throttled rate, which is the same shape the Pub/Sub push
// endpoint already uses for a refused token.
const unauthorizedLogEvery = 100

// Poller is the inbound half, and at M3.5 it does exactly one thing: accept
// the `/start <code>` that pairs a chat.
//
// It runs even after pairing, for two reasons. Something has to read and
// discard what arrives, or Telegram holds a growing backlog of unconfirmed
// updates. And the count of updates from chats that are not the paired one is
// worth having — it is the only evidence that the bot has been found.
//
// M6 turns this into §8.5's command set. The authorization check it makes here
// is the one those commands will run, unchanged: a button and a slash command
// must not become two code paths with different checks.
type Poller struct {
	store  *Store
	bot    *bot.Bot
	log    *slog.Logger
	onPair func(ctx context.Context, chatID int64)

	mu           sync.Mutex
	unauthorized int

	wg      sync.WaitGroup
	startMu sync.Mutex
	started bool
}

// PollerOptions configure the loop.
type PollerOptions struct {
	// BaseURL is the origin the reply's deep link points at.
	BaseURL string
	// PollTimeout is how long Telegram holds a getUpdates open. It closes one
	// at 50s regardless.
	PollTimeout time.Duration
	Logger      *slog.Logger
	// OnPair runs after a successful pairing, so the process can say hello on
	// the channel it just gained.
	OnPair func(ctx context.Context, chatID int64)
}

// NewPoller builds the loop. baseURL empty means Telegram's own API root.
//
// It builds a second bot instance rather than sharing the sender's: this one
// carries a handler and calls Start, and the sender's makes outbound calls
// only. Two instances over one token cost one extra HTTP client and keep the
// long poll's timeout from being the send timeout.
func NewPoller(st *Store, token, apiBaseURL string, opts PollerOptions) (*Poller, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.PollTimeout <= 0 || opts.PollTimeout > 50*time.Second {
		opts.PollTimeout = 30 * time.Second
	}

	p := &Poller{store: st, log: opts.Logger, onPair: opts.OnPair}

	options := []bot.Option{
		bot.WithSkipGetMe(),
		// The client timeout has to outlast the long poll, or every poll ends
		// as a client-side timeout and the loop spins.
		bot.WithHTTPClient(opts.PollTimeout, &http.Client{Timeout: opts.PollTimeout + 10*time.Second}),
		// Messages only. Callback queries arrive with M6's inline buttons, and
		// asking for update types nothing handles is asking Telegram to keep
		// them for us.
		bot.WithAllowedUpdates(bot.AllowedUpdates{"message"}),
		bot.WithDefaultHandler(p.handle),
		bot.WithErrorsHandler(func(err error) {
			// The library logs to the standard logger otherwise, which would
			// escape the configured format.
			opts.Logger.Warn("telegram long poll", "error", err)
		}),
	}
	if apiBaseURL != "" {
		options = append(options, bot.WithServerURL(apiBaseURL))
	}

	b, err := bot.New(token, options...)
	if err != nil {
		return nil, err
	}
	p.bot = b
	return p, nil
}

// Start runs the long poll until ctx is done.
func (p *Poller) Start(ctx context.Context) {
	p.startMu.Lock()
	if p.started {
		p.startMu.Unlock()
		return
	}
	p.started = true
	p.startMu.Unlock()

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.bot.Start(ctx)
	}()
}

// Stop waits for the loop, bounded by ctx.
func (p *Poller) Stop(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		// The poll is parked on a socket read that Telegram holds for up to
		// fifty seconds. Not worth waiting out, and nothing is lost: the
		// cursor is on disk.
		return errors.New("telegram: the long poll did not stop before the shutdown deadline")
	}
}

// Unauthorized is how many updates have been dropped for coming from a chat
// that is not the paired one. It is the evidence that the bot has been found.
func (p *Poller) Unauthorized() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.unauthorized
}

// handle is the whole of the inbound surface at M3.5.
func (p *Poller) handle(ctx context.Context, _ *bot.Bot, update *models.Update) {
	if update == nil || update.Message == nil {
		return
	}

	state, err := p.store.State(ctx)
	if err != nil {
		p.log.Error("read the channel while handling an update", "error", err)
		return
	}

	// The library keeps its own offset in memory and starts from zero after a
	// restart, so Telegram redelivers anything it has not had confirmed. The
	// persisted cursor is what turns that back into §8.1's promise: a restart
	// neither replays a command nor drops one that arrived while down.
	if update.ID <= state.LastUpdateID {
		return
	}
	if err := p.store.SetCursor(ctx, update.ID); err != nil {
		p.log.Error("advance the update cursor", "error", err)
	}

	chatID := update.Message.Chat.ID
	text := strings.TrimSpace(update.Message.Text)

	// Paired: a single chat_id is the entire authorization model, and every
	// update is checked against it (§8.2).
	if state.Paired() {
		if chatID != *state.ChatID {
			p.dropped(chatID)
			return
		}
		// M3.5 is outbound only. Saying so beats silence: an operator who
		// tries /status should learn that it does not exist yet rather than
		// conclude the bot is dead.
		if strings.HasPrefix(text, "/") {
			p.reply(ctx, chatID, "This channel sends alerts and does not take commands yet. Use the web app.")
		}
		return
	}

	// Unpaired: the only thing worth reading is a pairing attempt.
	code, ok := pairingCommand(text)
	if !ok {
		p.dropped(chatID)
		return
	}

	switch err := p.store.Pair(ctx, code, chatID); {
	case errors.Is(err, ErrBadPairingCode):
		// One message for wrong, used, and expired. Telling a prober which of
		// the three it got is telling it how to get closer.
		p.log.Warn("refused a pairing attempt", "chat_id", chatID)
		p.reply(ctx, chatID, "That code is not valid. Get a fresh one from the Intake screen.")
	case err != nil:
		p.log.Error("pair the chat", "error", err, "chat_id", chatID)
	default:
		p.log.Info("telegram paired", "chat_id", chatID)
		p.reply(ctx, chatID, "Paired. Operational alerts will arrive here.\n\n"+
			"Unpairing needs server access: run rental-bot -unpair-telegram on the host.")
		if p.onPair != nil {
			p.onPair(ctx, chatID)
		}
	}
}

// dropped counts an update nobody asked for, and says so occasionally.
func (p *Poller) dropped(chatID int64) {
	p.mu.Lock()
	p.unauthorized++
	count := p.unauthorized
	p.mu.Unlock()

	if count == 1 || count%unauthorizedLogEvery == 0 {
		p.log.Warn("dropped an update from an unauthorized chat", "chat_id", chatID, "count", count)
	}
}

// reply answers on the chat the update came from.
//
// It goes directly rather than through the Sender, because a reply is a reply:
// it belongs to the chat that just spoke, which during pairing is not yet the
// paired chat, and it is not an alert and must not touch the cooldown.
func (p *Poller) reply(ctx context.Context, chatID int64, text string) {
	if _, err := p.bot.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: text}); err != nil {
		p.log.Warn("reply on the telegram channel", "error", err, "chat_id", chatID)
	}
}

// pairingCommand reads `/start <code>`, in the forms Telegram delivers it.
//
// A client sending to a named bot in a group writes `/start@rental_bot`, and
// the operator retyping the code will get the case wrong at least once.
func pairingCommand(text string) (string, bool) {
	head, rest, found := strings.Cut(text, " ")
	if !found {
		return "", false
	}
	if name, _, ok := strings.Cut(head, "@"); ok {
		head = name
	}
	if !strings.EqualFold(head, "/start") {
		return "", false
	}
	code := strings.ToUpper(strings.TrimSpace(rest))
	if code == "" {
		return "", false
	}
	return code, true
}
