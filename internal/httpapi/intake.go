package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/farrellm/rental-bot/internal/auth"
	"github.com/farrellm/rental-bot/internal/gmail"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// oauthPurpose names this flow in the signed state, so a state issued for
// connecting Gmail cannot be replayed into a later milestone's flow.
const oauthPurpose = "gmail-connect"

// unauthenticatedPushLogEvery throttles the log line for a refused push.
//
// Bot operators discover public endpoints and probe them. One line per probe
// turns the log into their traffic; one line per hundred still says the
// endpoint is being hit.
const unauthenticatedPushLogEvery = 100

// intakeStanding is the connected mailbox as the intake screen reads it.
//
// Configured and Connected are separate questions with different answers. Not
// configured means nobody asked for ingestion, which is a working state; not
// connected means somebody did and has not finished. Collapsing them would show
// a fresh clone a broken mailbox.
type intakeStanding struct {
	Configured bool `json:"configured"`
	Connected  bool `json:"connected"`
	// State is the word the stamp says: watching, lapsed, degraded, revoked,
	// not-connected, not-configured.
	State   string `json:"state"`
	Address string `json:"address,omitempty"`
	// ForwardTo is the address to forward mail to, which is the account's own.
	ForwardTo      string   `json:"forward_to,omitempty"`
	ConnectedAt    string   `json:"connected_at,omitempty"`
	HistoryID      string   `json:"history_id,omitempty"`
	WatchExpiresAt string   `json:"watch_expires_at,omitempty"`
	LastSyncAt     string   `json:"last_sync_at,omitempty"`
	LastSyncCount  int64    `json:"last_sync_count"`
	LastError      string   `json:"last_error,omitempty"`
	AllowedSenders []string `json:"allowed_senders"`
	// PollInterval is how long the operator waits at worst if the push never
	// arrives, in seconds.
	PollIntervalSeconds int64 `json:"poll_interval_seconds"`
	// Missing names the configuration keys that are not set, when ingestion is
	// off. An empty screen that says which keys to fill beats one that says
	// "not configured".
	Missing []string `json:"missing,omitempty"`
	// Counts is how many messages sit in each disposition.
	Counts map[string]int64 `json:"counts"`
	// QueueDepth is how much work is waiting, by job status.
	QueueDepth map[string]int64 `json:"queue_depth"`
	CheckedAt  string           `json:"checked_at"`
}

type emailMessageResponse struct {
	ID             int64                     `json:"id"`
	GmailMessageID string                    `json:"gmail_message_id"`
	ThreadID       string                    `json:"thread_id"`
	From           string                    `json:"from_addr"`
	To             string                    `json:"to_addr"`
	Subject        string                    `json:"subject"`
	ReceivedAt     string                    `json:"received_at"`
	Snippet        string                    `json:"snippet"`
	Status         string                    `json:"status"`
	Error          string                    `json:"error,omitempty"`
	HasRaw         bool                      `json:"has_raw"`
	Attachments    []emailAttachmentResponse `json:"attachments"`
}

type emailAttachmentResponse struct {
	ID            int64  `json:"id"`
	Filename      string `json:"filename"`
	Mime          string `json:"mime"`
	SizeBytes     int64  `json:"size_bytes"`
	DocumentID    *int64 `json:"document_id"`
	SkippedReason string `json:"skipped_reason,omitempty"`
}

type emailMessageList struct {
	Items      []emailMessageResponse `json:"items"`
	NextCursor string                 `json:"next_cursor,omitempty"`
}

func (s *server) routeIntake(mux *http.ServeMux) {
	// The push endpoint is outside /api/v1 and outside the session: Pub/Sub has
	// no cookie, and its OIDC token is the whole of its authorization.
	route(mux, "/webhooks/gmail", methods{http.MethodPost: s.handleGmailPush})

	route(mux, "/api/v1/gmail", methods{
		http.MethodGet:    s.guarded(s.handleGmailStanding),
		http.MethodDelete: s.guarded(s.handleGmailDisconnect),
	})
	route(mux, "/api/v1/gmail/connect", methods{
		http.MethodPost: s.guarded(s.handleGmailConnect),
	})
	route(mux, gmail.CallbackPath, methods{
		http.MethodGet: s.guarded(s.handleGmailCallback),
	})
	route(mux, "/api/v1/gmail/sync", methods{
		http.MethodPost: s.guarded(s.handleGmailSyncNow),
	})

	route(mux, "/api/v1/email-messages", methods{
		http.MethodGet: s.guarded(s.handleListEmailMessages),
	})
	route(mux, "/api/v1/email-messages/{id}", methods{
		http.MethodGet: s.guarded(s.handleGetEmailMessage),
	})
	route(mux, "/api/v1/email-messages/{id}/raw", methods{
		http.MethodGet: s.guarded(s.handleEmailMessageRaw),
	})
}

// handleGmailPush takes a Pub/Sub push and enqueues a sync.
//
// It answers in milliseconds and does no work of its own. The push carries only
// a historyId, and even that is not read: believing it would let anyone who
// captured a token choose where in history this process resumes from. The sync
// takes its cursor from the database.
func (s *server) handleGmailPush(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if s.pushVerifier == nil || !s.pushVerifier.Configured() {
		WriteProblem(w, r, http.StatusServiceUnavailable, "Push ingestion is not configured.")
		return
	}
	if err := s.pushVerifier.Verify(ctx, r.Header.Get("Authorization")); err != nil {
		s.logRefusedPush(ctx, err)
		// The reason is deliberately not in the body. A verifier that explains
		// which check failed is a verifier that helps whoever is probing it.
		WriteProblem(w, r, http.StatusUnauthorized, "This request did not authenticate.")
		return
	}

	// The body is decoded only for the log line, so a shape we do not recognise
	// is answered 200: Pub/Sub retries a non-2xx until the message expires, and
	// retrying a body we will never understand helps nobody.
	var envelope gmail.PushEnvelope
	_ = decodeJSONQuietly(r, &envelope)

	added, err := s.enqueue(ctx, gmail.KindSync, gmail.KindSync, nil)
	if err != nil {
		// A 500 is honest here and Pub/Sub will redeliver, which is what we
		// want: the push is the only signal that this particular change
		// happened, and the poller is minutes away.
		loggerFrom(ctx).Error("enqueue a sync from the push", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not queue the sync.")
		return
	}

	loggerFrom(ctx).Info("gmail push",
		"pubsub_message_id", envelope.Message.MessageID,
		"queued", added,
	)
	w.WriteHeader(http.StatusNoContent)
}

// logRefusedPush records a failed verification at a throttled rate.
func (s *server) logRefusedPush(ctx context.Context, cause error) {
	s.pushRefusals.Lock()
	s.pushRefusalCount++
	count := s.pushRefusalCount
	s.pushRefusals.Unlock()

	if count == 1 || count%unauthenticatedPushLogEvery == 0 {
		loggerFrom(ctx).Warn("refused an unauthenticated Gmail push",
			"error", cause, "refusals", count)
	}
}

// handleGmailStanding answers with the state of ingestion.
func (s *server) handleGmailStanding(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	out := intakeStanding{
		Configured:          s.cfg.Gmail.Enabled(),
		AllowedSenders:      s.cfg.Gmail.AllowedSenders,
		PollIntervalSeconds: int64(s.cfg.Gmail.PollInterval.Duration.Seconds()),
		State:               "not-configured",
		Counts:              map[string]int64{},
		QueueDepth:          map[string]int64{},
		CheckedAt:           timestamp(),
	}
	if out.AllowedSenders == nil {
		out.AllowedSenders = []string{}
	}
	if !out.Configured {
		out.Missing = s.missingGmailConfig()
	}

	if s.gmail != nil {
		account, err := s.gmail.Account(ctx)
		switch {
		case errors.Is(err, gmail.ErrNotConnected):
			out.State = "not-connected"
		case err != nil:
			loggerFrom(ctx).Error("read the gmail account", "error", err)
			out.State = "degraded"
			out.LastError = "The connected account could not be read."
		default:
			out.Connected = true
			out.Address = account.Address
			out.ForwardTo = account.Address
			out.ConnectedAt = formatTime(&account.ConnectedAt)
			out.HistoryID = formatHistoryID(account.HistoryID)
			out.WatchExpiresAt = formatTime(account.WatchExpiresAt)
			out.LastSyncAt = formatTime(account.LastSyncAt)
			out.LastSyncCount = account.LastSyncCount
			out.LastError = account.LastError
			out.State = intakeState(account, time.Now().UTC())
		}
	}

	if counts, err := s.repo.Read().CountEmailMessagesByStatus(ctx); err != nil {
		loggerFrom(ctx).Error("count email messages", "error", err)
	} else {
		for _, row := range counts {
			out.Counts[row.Status] = row.Count
		}
	}
	if s.queue != nil {
		if depth, err := s.queue.Depth(ctx); err != nil {
			loggerFrom(ctx).Error("read the queue depth", "error", err)
		} else {
			out.QueueDepth = depth
		}
	}

	// The screen polls this; a cached answer would show a stopped mailbox as
	// working.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, http.StatusOK, out)
}

// intakeState is the word the stamp says.
//
// The order is worst-first: a revoked grant is not "watching" merely because
// the watch has days left on it.
func intakeState(account gmail.Account, now time.Time) string {
	switch {
	case account.Status == "revoked":
		return "revoked"
	case account.Status == "degraded":
		return "degraded"
	case account.WatchLapsed(now):
		return "lapsed"
	default:
		return "watching"
	}
}

// missingGmailConfig names what is not set, so an empty screen can say which
// keys to fill rather than only that something is missing.
func (s *server) missingGmailConfig() []string {
	missing := []string{}
	if s.cfg.Gmail.ClientID == "" {
		missing = append(missing, "gmail.client_id")
	}
	if s.cfg.Secrets.GmailClientSecret == "" {
		missing = append(missing, "RENTAL_BOT_GMAIL_CLIENT_SECRET")
	}
	if s.cfg.Gmail.Topic == "" {
		missing = append(missing, "gmail.topic")
	}
	if len(s.cfg.Gmail.AllowedSenders) == 0 {
		missing = append(missing, "gmail.allowed_senders")
	}
	if len(s.cfg.Secrets.Key) == 0 {
		missing = append(missing, "RENTAL_BOT_SECRET_KEY")
	}
	return missing
}

type connectResponse struct {
	// AuthorizeURL is where the browser goes next. The SPA navigates to it
	// rather than the server redirecting, because the request that asks for it
	// carries the CSRF header and a redirect would strip the flow of that.
	AuthorizeURL string `json:"authorize_url"`
}

// handleGmailConnect starts the OAuth flow.
func (s *server) handleGmailConnect(w http.ResponseWriter, r *http.Request) {
	if !s.ingestionReady(w, r) {
		return
	}

	session, ok := auth.SessionFrom(r.Context())
	if !ok {
		WriteProblem(w, r, http.StatusUnauthorized, "Sign in first.")
		return
	}

	state, err := s.guard.IssueState(oauthPurpose, session.TokenHash)
	if err != nil {
		loggerFrom(r.Context()).Error("issue the oauth state", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not start the connection.")
		return
	}
	writeJSON(w, r, http.StatusOK, connectResponse{AuthorizeURL: s.gmail.AuthCodeURL(state)})
}

// handleGmailCallback finishes the OAuth flow and sends the operator back.
//
// This is reachable with the session cookie because SameSite=Lax admits a
// top-level GET, which is exactly what a redirect back from Google is.
func (s *server) handleGmailCallback(w http.ResponseWriter, r *http.Request) {
	if !s.ingestionReady(w, r) {
		return
	}
	ctx := r.Context()
	query := r.URL.Query()

	if reason := query.Get("error"); reason != "" {
		// The operator declined, or Google refused. Neither is this server's
		// failure, and the screen says so rather than the browser showing JSON.
		s.redirectToIntake(w, r, "denied")
		return
	}
	session, ok := auth.SessionFrom(ctx)
	if !ok {
		WriteProblem(w, r, http.StatusUnauthorized, "Sign in first.")
		return
	}
	if err := s.guard.CheckState(oauthPurpose, session.TokenHash, query.Get("state")); err != nil {
		// A callback whose state we did not issue is a login-CSRF attempt:
		// somebody trying to attach their mailbox to this operator's account.
		loggerFrom(ctx).Warn("refused an oauth callback with a bad state", "error", err)
		WriteProblem(w, r, http.StatusBadRequest, "That sign-in did not start here. Try connecting again.")
		return
	}

	code := query.Get("code")
	if code == "" {
		WriteProblem(w, r, http.StatusBadRequest, "Google sent no authorization code.")
		return
	}

	account, err := s.gmail.Connect(ctx, code)
	if err != nil {
		loggerFrom(ctx).Error("connect the gmail account", "error", err)
		s.redirectToIntake(w, r, "failed")
		return
	}
	loggerFrom(ctx).Info("connected a gmail account", "address", account.Address)

	// Register the watch and take a first pass now rather than at the next
	// tick: the operator just pressed a button and is watching the screen.
	if _, err := s.enqueue(ctx, gmail.KindRenewWatch, gmail.KindRenewWatch, nil); err != nil {
		loggerFrom(ctx).Error("queue the watch registration", "error", err)
	}
	if _, err := s.enqueue(ctx, gmail.KindSync, gmail.KindSync, nil); err != nil {
		loggerFrom(ctx).Error("queue the first sync", "error", err)
	}

	s.redirectToIntake(w, r, "connected")
}

// redirectToIntake sends the browser back to the screen that asked.
func (s *server) redirectToIntake(w http.ResponseWriter, r *http.Request, outcome string) {
	http.Redirect(w, r, "/intake?gmail="+outcome, http.StatusSeeOther)
}

// handleGmailDisconnect revokes the grant and forgets the account.
func (s *server) handleGmailDisconnect(w http.ResponseWriter, r *http.Request) {
	if !s.ingestionReady(w, r) {
		return
	}
	ctx := r.Context()

	// Stopping the watch first means Google stops pushing to an endpoint that
	// will answer "not connected" to everything.
	if err := gmail.NewWatcher(s.gmail, s.cfg.Gmail.Topic).Stop(ctx); err != nil {
		loggerFrom(ctx).Warn("could not stop the gmail watch", "error", err)
	}

	if err := s.gmail.Disconnect(ctx); err != nil {
		if errors.Is(err, gmail.ErrNotConnected) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		loggerFrom(ctx).Error("disconnect the gmail account", "error", err)
		WriteProblem(w, r, http.StatusBadGateway,
			"Google would not revoke the connection. Nothing was changed; try again.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type syncResponse struct {
	// Queued is false when a sync was already waiting, which is the answer the
	// caller wanted rather than a failure.
	Queued bool `json:"queued"`
}

// handleGmailSyncNow queues a sync on demand.
func (s *server) handleGmailSyncNow(w http.ResponseWriter, r *http.Request) {
	if !s.ingestionReady(w, r) {
		return
	}
	added, err := s.enqueue(r.Context(), gmail.KindSync, gmail.KindSync, nil)
	if err != nil {
		loggerFrom(r.Context()).Error("queue a sync", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not queue the sync.")
		return
	}
	writeJSON(w, r, http.StatusAccepted, syncResponse{Queued: added})
}

// ingestionReady reports whether the Gmail routes can do anything, answering
// the request when they cannot.
func (s *server) ingestionReady(w http.ResponseWriter, r *http.Request) bool {
	if s.gmail == nil || s.queue == nil {
		WriteProblem(w, r, http.StatusServiceUnavailable,
			"Email ingestion is not configured. Set gmail.client_id and restart.")
		return false
	}
	return true
}

func (s *server) handleListEmailMessages(w http.ResponseWriter, r *http.Request) {
	size, ok := pageSize(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	rows, err := s.emailMessagePage(ctx, r.URL.Query().Get("cursor"), size+1)
	if err != nil {
		if errors.Is(err, errBadCursor) {
			WriteProblem(w, r, http.StatusBadRequest, "The cursor is not one this endpoint issued.")
			return
		}
		loggerFrom(ctx).Error("list email messages", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not read the register.")
		return
	}

	out := emailMessageList{Items: make([]emailMessageResponse, 0, size)}
	for i, row := range rows {
		if i == size {
			last := rows[i-1]
			out.NextCursor = encodeCursor(last.ReceivedAt, last.ID)
			break
		}
		item := newEmailMessageResponse(row)
		// The list carries attachments too: the register shows an enclosure
		// count on every line, and a request per line to get it would be worse
		// than one join's worth of work here.
		attachments, err := s.repo.Read().ListEmailAttachments(ctx, row.ID)
		if err != nil {
			loggerFrom(ctx).Error("list email attachments", "error", err, "email_message_id", row.ID)
		} else {
			item.Attachments = newAttachmentResponses(attachments)
		}
		out.Items = append(out.Items, item)
	}
	writeJSON(w, r, http.StatusOK, out)
}

func (s *server) emailMessagePage(ctx context.Context, cursor string, limit int) ([]sqlc.EmailMessage, error) {
	if cursor == "" {
		return s.repo.Read().ListEmailMessagesFirstPage(ctx, int64(limit))
	}
	receivedAt, id, err := decodeCursor(cursor)
	if err != nil {
		return nil, err
	}
	return s.repo.Read().ListEmailMessagesAfter(ctx, sqlc.ListEmailMessagesAfterParams{
		AfterReceivedAt: receivedAt,
		AfterID:         id,
		PageSize:        int64(limit),
	})
}

func (s *server) handleGetEmailMessage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ctx := r.Context()

	msg, err := s.repo.Read().GetEmailMessage(ctx, id)
	if err != nil {
		s.emailMessageReadError(w, r, err)
		return
	}
	attachments, err := s.repo.Read().ListEmailAttachments(ctx, id)
	if err != nil {
		loggerFrom(ctx).Error("list email attachments", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not read what came attached.")
		return
	}

	out := newEmailMessageResponse(msg)
	out.Attachments = newAttachmentResponses(attachments)
	writeJSON(w, r, http.StatusOK, out)
}

// handleEmailMessageRaw serves the archived .eml.
//
// Always as an attachment, never inline: an .eml is HTML plus whatever the
// sender put in it, and this application's origin holds the operator's session.
// The same three headers the document handler sets apply for the same reasons.
func (s *server) handleEmailMessageRaw(w http.ResponseWriter, r *http.Request) {
	if s.archive == nil {
		WriteProblem(w, r, http.StatusServiceUnavailable, "The raw email archive is not configured.")
		return
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ctx := r.Context()

	msg, err := s.repo.Read().GetEmailMessage(ctx, id)
	if err != nil {
		s.emailMessageReadError(w, r, err)
		return
	}
	if msg.RawPath == "" {
		WriteProblem(w, r, http.StatusNotFound,
			"This message was never downloaded, so there is no original to show.")
		return
	}

	f, err := s.archive.Open(msg.RawPath)
	if err != nil {
		loggerFrom(ctx).Error("open the archived message", "error", err, "email_message_id", id)
		WriteProblem(w, r, http.StatusNotFound, "This message's original is missing from the archive.")
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Disposition",
		`attachment; filename="`+msg.GmailMessageID+`.eml"`)

	http.ServeContent(w, r, msg.GmailMessageID+".eml", modifiedAt(msg.UpdatedAt), f)
}

func (s *server) emailMessageReadError(w http.ResponseWriter, r *http.Request, err error) {
	if store.NotFound(err) {
		WriteProblem(w, r, http.StatusNotFound, "No such message.")
		return
	}
	loggerFrom(r.Context()).Error("read email message", "error", err)
	WriteProblem(w, r, http.StatusInternalServerError, "Could not read the message.")
}

func newEmailMessageResponse(row sqlc.EmailMessage) emailMessageResponse {
	return emailMessageResponse{
		ID: row.ID, GmailMessageID: row.GmailMessageID, ThreadID: row.ThreadID,
		From: row.FromAddr, To: row.ToAddr, Subject: row.Subject,
		ReceivedAt: row.ReceivedAt, Snippet: row.Snippet,
		Status: row.Status, Error: row.Error,
		HasRaw:      row.RawPath != "",
		Attachments: []emailAttachmentResponse{},
	}
}

func newAttachmentResponses(rows []sqlc.EmailAttachment) []emailAttachmentResponse {
	out := make([]emailAttachmentResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, emailAttachmentResponse{
			ID: row.ID, Filename: row.Filename, Mime: row.Mime,
			SizeBytes: row.SizeBytes, DocumentID: row.DocumentID,
			SkippedReason: row.SkippedReason,
		})
	}
	return out
}

// enqueue puts work on the queue and wakes the pool.
//
// Every handler that enqueues wants both, and forgetting the second half is a
// bug that looks like "the sync took five seconds to start" rather than like a
// failure.
func (s *server) enqueue(ctx context.Context, kind, dedupeKey string, payload any) (bool, error) {
	if s.queue == nil {
		return false, errors.New("httpapi: no job queue is configured")
	}
	added, err := s.queue.EnqueueOnce(ctx, kind, dedupeKey, payload)
	if err != nil {
		return false, err
	}
	if added && s.runner != nil {
		s.runner.Notify()
	}
	return added, nil
}

// decodeJSONQuietly reads a body without answering the request on failure.
//
// The push handler wants the body for a log line and nothing else, so a body it
// cannot read is not worth a response — see handleGmailPush.
func decodeJSONQuietly(r *http.Request, dst any) error {
	return json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(dst)
}

func formatTime(at *time.Time) string {
	if at == nil || at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

// formatHistoryID renders the cursor for the wire. Zero is empty, because "no
// cursor yet" and "cursor zero" are different claims.
func formatHistoryID(id uint64) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatUint(id, 10)
}
