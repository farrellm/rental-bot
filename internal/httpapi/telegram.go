package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/farrellm/rental-bot/internal/alert"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
	"github.com/farrellm/rental-bot/internal/telegram"
)

// channelStanding is the alert channel as the intake screen reads it.
//
// Configured and Paired are separate questions with different answers, the
// same way the mailbox's Configured and Connected are. Not configured means
// nobody asked for a channel, which is a working state; not paired means
// somebody did and has not finished. Collapsing them would show a fresh clone
// a broken bot.
type channelStanding struct {
	Configured bool `json:"configured"`
	Paired     bool `json:"paired"`
	// State is the word the stamp says: paired, muted, no-contact,
	// not-connected, not-configured.
	State string `json:"state"`
	// BotUsername is where the operator sends /start, without the @.
	BotUsername string `json:"bot_username,omitempty"`
	ChatID      *int64 `json:"chat_id,omitempty"`
	PairedAt    string `json:"paired_at,omitempty"`
	MutedUntil  string `json:"muted_until,omitempty"`
	LastSentAt  string `json:"last_sent_at,omitempty"`
	LastError   string `json:"last_error,omitempty"`
	// PairingCode is set only in the response to issuing one. The code is not
	// recoverable afterwards — only its hash is stored — so the standing
	// carries the expiry and never the code.
	PairingCode      string `json:"pairing_code,omitempty"`
	PairingExpiresAt string `json:"pairing_expires_at,omitempty"`
	// CooldownSeconds is how long a condition stays quiet after being stated.
	CooldownSeconds int64 `json:"cooldown_seconds"`
	// Missing names the configuration keys that are not set, when the channel
	// is off. An empty screen that says which keys to fill beats one that says
	// "not configured".
	Missing []string `json:"missing,omitempty"`
	// Sent and Cleared are the dispatch register's tally.
	Sent      int64  `json:"sent"`
	Cleared   int64  `json:"cleared"`
	CheckedAt string `json:"checked_at"`
}

// noticeResponse is one line of the dispatch register.
type noticeResponse struct {
	ID       int64  `json:"id"`
	Key      string `json:"dedupe_key"`
	Channel  string `json:"channel"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Detail   string `json:"detail,omitempty"`
	// FirstSeenAt is when the condition was first recorded. A restated
	// condition keeps it, which is what makes the register a register rather
	// than a log.
	FirstSeenAt string `json:"first_seen_at"`
	LastSentAt  string `json:"last_sent_at,omitempty"`
	// SendCount is how many times this one condition has gone out. The tally
	// in the margin of the register is this number.
	SendCount  int64  `json:"send_count"`
	ResolvedAt string `json:"resolved_at,omitempty"`
}

type noticeList struct {
	Items []noticeResponse `json:"items"`
	// NextCursor is absent on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

func (s *server) routeTelegram(mux *http.ServeMux) {
	route(mux, "/api/v1/telegram", methods{
		http.MethodGet: s.guarded(s.handleChannelStanding),
	})
	route(mux, "/api/v1/telegram/pairing-code", methods{
		http.MethodPost: s.guarded(s.handleIssuePairingCode),
	})
	route(mux, "/api/v1/telegram/test", methods{
		http.MethodPost: s.guarded(s.handleTestNotice),
	})

	route(mux, "/api/v1/notifications", methods{
		http.MethodGet: s.guarded(s.handleListNotifications),
	})
}

// handleChannelStanding answers with the state of the alert channel.
func (s *server) handleChannelStanding(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now().UTC()

	out := channelStanding{
		Configured:      s.cfg.Telegram.Enabled(),
		BotUsername:     s.cfg.Telegram.BotUsername,
		CooldownSeconds: int64(s.cfg.Telegram.Cooldown.Duration.Seconds()),
		State:           "not-configured",
		CheckedAt:       timestamp(),
	}
	if !out.Configured {
		out.Missing = s.missingTelegramConfig()
	}

	if s.telegram != nil {
		state, err := s.telegram.State(ctx)
		if err != nil {
			loggerFrom(ctx).Error("read the alert channel", "error", err)
			out.State = "no-contact"
			out.LastError = "The channel could not be read."
		} else {
			out.Paired = state.Paired()
			out.ChatID = state.ChatID
			out.PairedAt = formatTime(state.PairedAt)
			out.MutedUntil = formatTime(state.MutedUntil)
			out.LastSentAt = formatTime(state.LastSentAt)
			out.LastError = state.LastError
			if state.PairingOpen(now) {
				out.PairingExpiresAt = formatTime(state.PairingExpiresAt)
			}
			out.State = channelState(state, now)
		}
	}

	// The register's tally comes from notifications rather than from the
	// channel, because the log sink writes there whether or not a bot exists:
	// a host with no Telegram still has a dispatch register worth reading.
	if tally, err := s.repo.Read().CountNotifications(ctx); err != nil {
		loggerFrom(ctx).Error("count notifications", "error", err)
	} else {
		out.Sent = tally.Total
		out.Cleared = tally.Total - tally.Outstanding
	}

	// The screen polls this; a cached answer would show a dead channel as
	// working.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, out)
}

// channelState is the word the stamp says.
//
// Worst-first, like the mailbox's: a channel whose last send failed is not
// "paired" merely because a chat id is on file.
func channelState(state telegram.State, now time.Time) string {
	switch {
	case !state.Paired():
		return "not-connected"
	case state.Status == "degraded":
		return "no-contact"
	case state.Muted(now):
		return "muted"
	default:
		return "paired"
	}
}

// missingTelegramConfig names what is not set.
func (s *server) missingTelegramConfig() []string {
	missing := []string{}
	if s.cfg.Telegram.BotUsername == "" {
		missing = append(missing, "telegram.bot_username")
	}
	if s.cfg.Secrets.TelegramBotToken == "" {
		missing = append(missing, "RENTAL_BOT_TELEGRAM_BOT_TOKEN")
	}
	return missing
}

type pairingCodeResponse struct {
	// Code is shown once and never again. Only its hash is stored.
	Code      string `json:"code"`
	ExpiresAt string `json:"expires_at"`
	// Send is the exact line to send the bot, so the operator copies rather
	// than assembles it.
	Send string `json:"send"`
	// BotUsername is who to send it to, without the @.
	BotUsername string `json:"bot_username"`
}

// handleIssuePairingCode mints a code for an unpaired channel.
//
// It refuses once a chat is paired. §8.2 puts re-pairing behind server access,
// and an endpoint that could mint a code for a paired bot would be the way
// around that: a hijacked session would be able to move the alert channel to
// the attacker's own chat, which is precisely the channel that would have
// reported the hijack.
func (s *server) handleIssuePairingCode(w http.ResponseWriter, r *http.Request) {
	if !s.channelReady(w, r) {
		return
	}
	ctx := r.Context()

	code, expires, err := s.telegram.IssuePairingCode(ctx)
	switch {
	case errors.Is(err, telegram.ErrAlreadyPaired):
		WriteProblem(w, r, http.StatusConflict,
			"A chat is already paired. Run rental-bot -unpair-telegram on the host to change it.")
		return
	case err != nil:
		loggerFrom(ctx).Error("issue a pairing code", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not issue a pairing code.")
		return
	}

	loggerFrom(ctx).Info("issued a telegram pairing code", "expires", formatTime(&expires))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusCreated, pairingCodeResponse{
		Code:        code,
		ExpiresAt:   formatTime(&expires),
		Send:        "/start " + code,
		BotUsername: s.cfg.Telegram.BotUsername,
	})
}

type testNoticeResponse struct {
	// Sent is false when nothing is paired, which the screen already knows and
	// reports rather than treating as a failure.
	Sent bool `json:"sent"`
}

// handleTestNotice puts one message through the whole path.
//
// The operator's only other way to find out whether the channel works is to
// wait for something to go wrong, which is the worst possible moment to
// discover that it does not.
func (s *server) handleTestNotice(w http.ResponseWriter, r *http.Request) {
	if !s.channelReady(w, r) {
		return
	}
	ctx := r.Context()

	state, err := s.telegram.State(ctx)
	if err != nil {
		loggerFrom(ctx).Error("read the alert channel", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not read the channel.")
		return
	}
	if !state.Paired() {
		WriteProblem(w, r, http.StatusConflict, "No chat is paired yet. Get a pairing code first.")
		return
	}

	// A key per test, so a second test is not swallowed by the first one's
	// cooldown. This is the one place a unique key is right: a test notice is
	// an event the operator just asked for, not a condition that persists.
	key := "telegram.test." + timestamp()
	alert.Publish(ctx, s.alerts, alert.Alert{
		Key:      key,
		Severity: alert.Info,
		Title:    "Test notice",
		Detail:   "The alert channel is working. Nothing is wrong.",
	})
	// Resolved immediately, so the register does not carry a test as an open
	// condition forever.
	alert.Resolve(ctx, s.alerts, key, "Test notice")

	writeJSON(w, r, http.StatusAccepted, testNoticeResponse{Sent: true})
}

// handleListNotifications answers with the dispatch register, newest first.
func (s *server) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	size, ok := pageSize(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	rows, err := s.notificationPage(ctx, r.URL.Query().Get("cursor"), size+1)
	if err != nil {
		if errors.Is(err, errBadCursor) {
			WriteProblem(w, r, http.StatusBadRequest, "The cursor is not one this endpoint issued.")
			return
		}
		loggerFrom(ctx).Error("list notifications", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not read the register.")
		return
	}

	out := noticeList{Items: make([]noticeResponse, 0, size)}
	for i, row := range rows {
		if i == size {
			last := rows[i-1]
			out.NextCursor = encodeCursor(last.FirstSeenAt, last.ID)
			break
		}
		out.Items = append(out.Items, newNoticeResponse(row))
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, out)
}

func (s *server) notificationPage(ctx context.Context, cursor string, limit int) ([]sqlc.Notification, error) {
	if cursor == "" {
		return s.repo.Read().ListNotificationsFirstPage(ctx, int64(limit))
	}
	firstSeenAt, id, err := decodeCursor(cursor)
	if err != nil {
		return nil, err
	}
	return s.repo.Read().ListNotificationsAfter(ctx, sqlc.ListNotificationsAfterParams{
		AfterFirstSeenAt: firstSeenAt,
		AfterID:          id,
		PageSize:         int64(limit),
	})
}

func newNoticeResponse(row sqlc.Notification) noticeResponse {
	out := noticeResponse{
		ID:          row.ID,
		Key:         row.DedupeKey,
		Channel:     row.Channel,
		Severity:    row.Severity,
		Title:       row.Title,
		Detail:      row.Detail,
		FirstSeenAt: row.FirstSeenAt,
		SendCount:   row.SendCount,
	}
	if row.LastSentAt != nil {
		out.LastSentAt = *row.LastSentAt
	}
	if row.ResolvedAt != nil {
		out.ResolvedAt = *row.ResolvedAt
	}
	return out
}

// channelReady reports whether the Telegram routes can do anything, answering
// the request when they cannot.
func (s *server) channelReady(w http.ResponseWriter, r *http.Request) bool {
	if s.telegram == nil {
		WriteProblem(w, r, http.StatusServiceUnavailable,
			"The alert channel is not configured. Set telegram.bot_username and restart.")
		return false
	}
	return true
}
