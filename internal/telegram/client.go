package telegram

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// DefaultBaseURL is Telegram's own API root. A test points a client at an
// httptest server instead.
const DefaultBaseURL = "https://api.telegram.org"

// sendTimeout bounds one outbound call. Telegram is usually fast, and an alert
// that hangs for a minute is an alert that arrives after the operator has
// already noticed the problem some other way.
const sendTimeout = 15 * time.Second

// Client is the outbound half of the Bot API: the three methods this milestone
// needs and nothing else.
//
// It wraps github.com/go-telegram/bot rather than the raw endpoints, which is
// the library docs/DESIGN.md §8 names. The wrapper is thin on purpose — the
// library's handler registry and middleware chain are for M6's inbound
// commands, and exposing them now would spread a dependency across the package
// for the sake of three calls.
type Client struct {
	bot *bot.Bot
}

// NewClient builds a client. baseURL empty means Telegram's own.
//
// Construction makes no network call. bot.New would otherwise verify the token
// with getMe, which would put a call to a third party in the startup path of a
// process that has to come up whether or not Telegram is reachable.
func NewClient(token, baseURL string) (*Client, error) {
	opts := []bot.Option{
		bot.WithSkipGetMe(),
		bot.WithHTTPClient(sendTimeout, &http.Client{Timeout: sendTimeout}),
	}
	if baseURL != "" {
		opts = append(opts, bot.WithServerURL(baseURL))
	}

	b, err := bot.New(token, opts...)
	if err != nil {
		return nil, fmt.Errorf("telegram: build the client: %w", err)
	}
	return &Client{bot: b}, nil
}

// SendMessage delivers one message to one chat, as plain text.
//
// No parse mode: an alert body carries an address, a filename, or an error
// string, and any of those can contain a character Markdown would read as
// markup. A message that fails to send because a lender's name has an
// underscore in it is a message lost at the worst moment.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string) error {
	if _, err := c.bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text:   text,
		// The deep link at the end of an alert is a link to this app, and a
		// preview card for it says nothing the line above did not.
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: ptr(true)},
	}); err != nil {
		return fmt.Errorf("telegram: send to chat %d: %w", chatID, err)
	}
	return nil
}

// Username asks Telegram who this token belongs to.
//
// Used once, at pairing, to check that the configured telegram.bot_username is
// the bot the token actually opens — an operator with two bots will paste the
// wrong pair eventually, and the failure without this check is an @name on the
// screen that nobody is listening to.
func (c *Client) Username(ctx context.Context) (string, error) {
	me, err := c.bot.GetMe(ctx)
	if err != nil {
		return "", fmt.Errorf("telegram: getMe: %w", err)
	}
	return me.Username, nil
}

func ptr[T any](v T) *T { return &v }
