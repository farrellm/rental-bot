// Package gmail is the intake path: the OAuth grant, the watch, the history
// walk, the raw archive, and the attachments that become documents.
//
// It stops at the door of the LLM. A message that lands here is archived,
// recorded, and its attachments filed; nothing is classified, extracted, or
// proposed. That gate is M4's (docs/DESIGN.md §5.4), and forwarded email is
// untrusted input that should reach a model with no capability to act on it.
//
// The Gmail REST surface this needs is six endpoints, so it is called directly
// rather than through the generated Google client: golang.org/x/oauth2 handles
// the grant, and net/http handles the rest. That keeps the binary static and
// the dependency list short enough to read.
package gmail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is Gmail's API root. Tests point a client at an httptest
// server instead.
const DefaultBaseURL = "https://gmail.googleapis.com"

// maxResponseBytes bounds a JSON response body. A raw message is fetched
// through a separate path with its own cap; nothing else Gmail returns here is
// legitimately large.
const maxResponseBytes = 8 << 20

// ErrHistoryTooOld reports that the stored cursor predates what Gmail still
// keeps, which happens after any multi-day outage.
//
// It is a typed error because the response is a different code path, not a
// retry: fall back to listing by timestamp and raise the condition (§4.3).
var ErrHistoryTooOld = errors.New("gmail: the stored historyId is older than Gmail's history")

// ErrTooLarge reports a message past the size cap.
//
// Typed because the caller's answer is to record the message as failed rather
// than to retry: the message will be exactly as large next time.
var ErrTooLarge = errors.New("gmail: the message is past the size cap")

// ErrRevoked reports that the grant is gone — the operator removed access, or
// the refresh token was invalidated.
//
// Retrying cannot fix it. Ingestion halts and says so, rather than failing
// quietly every ten minutes forever.
var ErrRevoked = errors.New("gmail: the authorization has been revoked")

// APIError is a Gmail response that was not a success.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gmail: %d %s", e.Status, e.Message)
}

// Client talks to one mailbox.
type Client struct {
	http    *http.Client
	baseURL string
}

// NewClient builds a client over an HTTP client that already carries the
// OAuth token — see TokenStore.HTTPClient.
func NewClient(httpClient *http.Client, baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{http: httpClient, baseURL: strings.TrimSuffix(baseURL, "/")}
}

// Profile is what users.getProfile returns.
type Profile struct {
	EmailAddress string `json:"emailAddress"`
	// HistoryID seeds the cursor at connect time, so the first sync does not
	// walk history that predates the grant.
	HistoryID uint64 `json:"historyId,string"`
}

// Profile identifies the connected mailbox.
func (c *Client) Profile(ctx context.Context) (Profile, error) {
	var out Profile
	err := c.do(ctx, http.MethodGet, "/gmail/v1/users/me/profile", nil, nil, &out)
	return out, err
}

// WatchResult is what users.watch returns.
type WatchResult struct {
	HistoryID  uint64    `json:"historyId,string"`
	Expiration time.Time `json:"-"`
	// expirationMs is Gmail's field: milliseconds since the epoch, as a string.
	expirationMs string
}

// UnmarshalJSON reads Gmail's stringly-typed expiration into a time.
func (w *WatchResult) UnmarshalJSON(data []byte) error {
	var raw struct {
		HistoryID  string `json:"historyId"`
		Expiration string `json:"expiration"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.HistoryID != "" {
		id, err := strconv.ParseUint(raw.HistoryID, 10, 64)
		if err != nil {
			return fmt.Errorf("gmail: watch historyId %q: %w", raw.HistoryID, err)
		}
		w.HistoryID = id
	}
	if raw.Expiration != "" {
		ms, err := strconv.ParseInt(raw.Expiration, 10, 64)
		if err != nil {
			return fmt.Errorf("gmail: watch expiration %q: %w", raw.Expiration, err)
		}
		w.Expiration = time.UnixMilli(ms).UTC()
	}
	w.expirationMs = raw.Expiration
	return nil
}

// Watch registers a Pub/Sub push for the mailbox. Google expires it after
// seven days regardless of what it returns, so the scheduler renews daily.
func (c *Client) Watch(ctx context.Context, topic string, labelIDs []string) (WatchResult, error) {
	body := map[string]any{"topicName": topic}
	if len(labelIDs) > 0 {
		body["labelIds"] = labelIDs
	}
	var out WatchResult
	err := c.do(ctx, http.MethodPost, "/gmail/v1/users/me/watch", nil, body, &out)
	return out, err
}

// StopWatch cancels the push registration.
func (c *Client) StopWatch(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/gmail/v1/users/me/stop", nil, struct{}{}, nil)
}

// History is one page of users.history.list.
type History struct {
	// MessageIDs is every message added since the cursor, deduplicated and in
	// the order Gmail reported them.
	MessageIDs []string
	// NextPageToken is empty on the last page.
	NextPageToken string
	// HistoryID is the cursor to store once the whole walk succeeds.
	HistoryID uint64
}

// ListHistory walks forward from startHistoryID.
//
// A 404 means the cursor is older than the history Gmail keeps, and comes back
// as ErrHistoryTooOld rather than as a generic failure — the caller has a
// different answer for it.
func (c *Client) ListHistory(ctx context.Context, startHistoryID uint64, pageToken string) (History, error) {
	q := url.Values{}
	q.Set("startHistoryId", strconv.FormatUint(startHistoryID, 10))
	q.Set("historyTypes", "messageAdded")
	if pageToken != "" {
		q.Set("pageToken", pageToken)
	}

	var raw struct {
		History []struct {
			MessagesAdded []struct {
				Message struct {
					ID string `json:"id"`
				} `json:"message"`
			} `json:"messagesAdded"`
		} `json:"history"`
		NextPageToken string `json:"nextPageToken"`
		HistoryID     string `json:"historyId"`
	}

	if err := c.do(ctx, http.MethodGet, "/gmail/v1/users/me/history", q, nil, &raw); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return History{}, fmt.Errorf("%w: %d", ErrHistoryTooOld, startHistoryID)
		}
		return History{}, err
	}

	out := History{NextPageToken: raw.NextPageToken}
	if raw.HistoryID != "" {
		id, err := strconv.ParseUint(raw.HistoryID, 10, 64)
		if err != nil {
			return History{}, fmt.Errorf("gmail: history id %q: %w", raw.HistoryID, err)
		}
		out.HistoryID = id
	}

	// One message can appear in several history records. Filing it twice is
	// harmless — every write downstream is keyed — but fetching it twice is
	// pointless.
	seen := map[string]bool{}
	for _, record := range raw.History {
		for _, added := range record.MessagesAdded {
			id := added.Message.ID
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			out.MessageIDs = append(out.MessageIDs, id)
		}
	}
	return out, nil
}

// ListMessagesSince is the fallback when the history cursor is too old.
//
// Gmail's `after:` search takes a Unix timestamp and is inclusive to the day in
// some clients, so the window is deliberately generous: re-listing a message
// already on file costs one skipped insert.
func (c *Client) ListMessagesSince(ctx context.Context, since time.Time, pageToken string) (History, error) {
	q := url.Values{}
	q.Set("q", "after:"+strconv.FormatInt(since.UTC().Unix(), 10))
	q.Set("maxResults", "100")
	if pageToken != "" {
		q.Set("pageToken", pageToken)
	}

	var raw struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := c.do(ctx, http.MethodGet, "/gmail/v1/users/me/messages", q, nil, &raw); err != nil {
		return History{}, err
	}

	out := History{NextPageToken: raw.NextPageToken}
	for _, m := range raw.Messages {
		if m.ID != "" {
			out.MessageIDs = append(out.MessageIDs, m.ID)
		}
	}
	return out, nil
}

// RawMessage is one message as Gmail stores it, plus the metadata that is not
// in the MIME.
type RawMessage struct {
	ID       string
	ThreadID string
	// InternalDate is Gmail's own receipt time, which is more trustworthy than
	// the Date header a forwarding client may have rewritten.
	InternalDate time.Time
	Snippet      string
	LabelIDs     []string
	// Raw is the full RFC 822 message.
	Raw []byte
}

// GetRaw fetches a whole message, capped at maxBytes.
//
// format=raw is deliberate: the archive has to be the bytes Gmail holds, not a
// reassembly of a parsed structure. A parser fix later should be replayable
// against what actually arrived.
func (c *Client) GetRaw(ctx context.Context, id string, maxBytes int64) (RawMessage, error) {
	q := url.Values{}
	q.Set("format", "raw")

	var raw struct {
		ID           string   `json:"id"`
		ThreadID     string   `json:"threadId"`
		Snippet      string   `json:"snippet"`
		LabelIDs     []string `json:"labelIds"`
		InternalDate string   `json:"internalDate"`
		SizeEstimate int64    `json:"sizeEstimate"`
		Raw          string   `json:"raw"`
	}
	if err := c.do(ctx, http.MethodGet, "/gmail/v1/users/me/messages/"+url.PathEscape(id), q, nil, &raw); err != nil {
		return RawMessage{}, err
	}

	// base64url without padding is what Gmail sends.
	decoded, err := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(strings.TrimRight(raw.Raw, "="))
	if err != nil {
		return RawMessage{}, fmt.Errorf("gmail: decode raw message %s: %w", id, err)
	}
	if maxBytes > 0 && int64(len(decoded)) > maxBytes {
		return RawMessage{}, fmt.Errorf("%w: %s is %d bytes, past the %d byte cap",
			ErrTooLarge, id, len(decoded), maxBytes)
	}

	out := RawMessage{
		ID: raw.ID, ThreadID: raw.ThreadID, Snippet: raw.Snippet,
		LabelIDs: raw.LabelIDs, Raw: decoded,
	}
	if out.ID == "" {
		out.ID = id
	}
	if raw.InternalDate != "" {
		ms, err := strconv.ParseInt(raw.InternalDate, 10, 64)
		if err != nil {
			return RawMessage{}, fmt.Errorf("gmail: internalDate %q: %w", raw.InternalDate, err)
		}
		out.InternalDate = time.UnixMilli(ms).UTC()
	}
	return out, nil
}

// Label is a Gmail label.
type Label struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// EnsureLabel returns the id of a label, creating it if it does not exist.
//
// The labels are how the mailbox itself shows what this process did, which is
// the only view of ingestion available from a phone's mail app.
func (c *Client) EnsureLabel(ctx context.Context, name string) (string, error) {
	var list struct {
		Labels []Label `json:"labels"`
	}
	if err := c.do(ctx, http.MethodGet, "/gmail/v1/users/me/labels", nil, nil, &list); err != nil {
		return "", err
	}
	for _, l := range list.Labels {
		if strings.EqualFold(l.Name, name) {
			return l.ID, nil
		}
	}

	var created Label
	err := c.do(ctx, http.MethodPost, "/gmail/v1/users/me/labels", nil, map[string]string{
		"name":                  name,
		"labelListVisibility":   "labelShow",
		"messageListVisibility": "show",
	}, &created)
	if err != nil {
		return "", err
	}
	return created.ID, nil
}

// ModifyLabels adds and removes labels on one message.
func (c *Client) ModifyLabels(ctx context.Context, messageID string, add, remove []string) error {
	body := map[string]any{}
	if len(add) > 0 {
		body["addLabelIds"] = add
	}
	if len(remove) > 0 {
		body["removeLabelIds"] = remove
	}
	if len(body) == 0 {
		return nil
	}
	return c.do(ctx, http.MethodPost,
		"/gmail/v1/users/me/messages/"+url.PathEscape(messageID)+"/modify", nil, body, nil)
}

// do performs one API call, decoding into out when out is not nil.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("gmail: encode request: %w", err)
		}
		reader = strings.NewReader(string(encoded))
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return fmt.Errorf("gmail: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// A refresh that fails against a revoked grant surfaces here, from the
		// oauth2 transport rather than from a Gmail status code.
		if isRevoked(err) {
			return fmt.Errorf("%w: %v", ErrRevoked, err)
		}
		return fmt.Errorf("gmail: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return c.statusError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); err != nil {
		return fmt.Errorf("gmail: decode %s response: %w", path, err)
	}
	return nil
}

// statusError turns a failure response into a typed error.
func (c *Client) statusError(resp *http.Response) error {
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))

	// Google's error body carries a message worth keeping; anything else is
	// reported as the raw text, truncated.
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	message := strings.TrimSpace(string(detail))
	if err := json.Unmarshal(detail, &envelope); err == nil && envelope.Error.Message != "" {
		message = envelope.Error.Message
	}
	if len(message) > 300 {
		message = message[:300] + "..."
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%w: %s", ErrRevoked, message)
	}
	return &APIError{Status: resp.StatusCode, Message: message}
}

// isRevoked recognises the oauth2 package's report of a dead grant.
//
// x/oauth2 returns a *RetrieveError for a token endpoint failure, but wraps it
// in the transport's own error, and the useful signal is the invalid_grant code
// in the body. Matching on the text is unlovely and is the only thing available
// without importing the package's internals.
func isRevoked(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return strings.Contains(text, "invalid_grant") || strings.Contains(text, "unauthorized_client")
}
