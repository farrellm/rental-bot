package gmail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/farrellm/rental-bot/internal/config"
	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/secret"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// Scopes are what the grant asks for (docs/DESIGN.md §4.1).
//
// readonly to fetch, modify to label and archive, send for the confirmation
// reply M4 adds. Asking for send now rather than later means the operator
// consents once instead of being sent back through a consent screen by a
// version upgrade.
var Scopes = []string{
	"https://www.googleapis.com/auth/gmail.readonly",
	"https://www.googleapis.com/auth/gmail.modify",
	"https://www.googleapis.com/auth/gmail.send",
}

// ErrNotConnected reports that no mailbox has been connected.
var ErrNotConnected = errors.New("gmail: no account is connected")

// googleEndpoint is Google's OAuth 2.0 endpoint, spelled out rather than taken
// from golang.org/x/oauth2/google.
//
// That subpackage exists to find credentials on a GCE instance and pulls in
// cloud.google.com/go/compute/metadata to do it. This process runs on a VPS
// with a refresh token in its own database, so the whole module would be
// carried for the two URLs below.
var googleEndpoint = oauth2.Endpoint{
	AuthURL:   "https://accounts.google.com/o/oauth2/auth",
	TokenURL:  "https://oauth2.googleapis.com/token",
	AuthStyle: oauth2.AuthStyleInParams,
}

// revokeURL retires a grant from our side when the operator disconnects.
const revokeURL = "https://oauth2.googleapis.com/revoke"

// CallbackPath is where Google sends the operator back after consent.
//
// It lives here rather than in httpapi because the OAuth client registration at
// Google names this exact path: the route and the redirect URL have to agree,
// and a constant is cheaper than a comment asking someone to remember.
const CallbackPath = "/api/v1/gmail/callback"

// Account is the connected mailbox as the rest of the application sees it.
//
// The refresh token is deliberately absent: nothing outside this file needs it,
// and a struct that carries it is a struct that can be logged.
type Account struct {
	Address        string
	Scopes         []string
	ConnectedAt    time.Time
	HistoryID      uint64
	WatchExpiresAt *time.Time
	LastSyncAt     *time.Time
	LastSyncCount  int64
	LastError      string
	Status         string
}

// WatchLapsed reports whether the push registration has expired.
//
// An account with no expiry has never registered one, which is also lapsed:
// both mean pushes are not arriving and the poller is carrying ingestion alone.
func (a Account) WatchLapsed(now time.Time) bool {
	return a.WatchExpiresAt == nil || a.WatchExpiresAt.Before(now)
}

// Store owns the account row and the token it holds.
type Store struct {
	repo   *store.Repo
	box    *secret.Box
	oauth  *oauth2.Config
	client *http.Client
	// baseURL is Gmail's API root and revokeURL is Google's revocation
	// endpoint. Both are overridden in tests; nothing in production sets them.
	baseURL   string
	revokeURL string
	now       func() time.Time
}

// NewStore builds the token store. redirectURL is where Google sends the
// operator back, and has to match what the OAuth client registers.
func NewStore(repo *store.Repo, box *secret.Box, cfg config.Config, redirectURL string) *Store {
	return &Store{
		repo: repo,
		box:  box,
		oauth: &oauth2.Config{
			ClientID:     cfg.Gmail.ClientID,
			ClientSecret: cfg.Secrets.GmailClientSecret,
			Endpoint:     googleEndpoint,
			RedirectURL:  redirectURL,
			Scopes:       Scopes,
		},
		client:    http.DefaultClient,
		baseURL:   DefaultBaseURL,
		revokeURL: revokeURL,
		now:       func() time.Time { return time.Now().UTC() },
	}
}

// SetBaseURL points the store's clients at a different API root, including the
// OAuth endpoints. Tests use it; nothing in production does.
func (s *Store) SetBaseURL(root string) {
	s.baseURL = root
	s.revokeURL = root + "/revoke"
	s.oauth.Endpoint = oauth2.Endpoint{
		AuthURL:   root + "/o/oauth2/auth",
		TokenURL:  root + "/token",
		AuthStyle: oauth2.AuthStyleInParams,
	}
}

// SetHTTPClient replaces the transport the OAuth exchange uses.
func (s *Store) SetHTTPClient(c *http.Client) { s.client = c }

// AuthCodeURL is where the operator is sent to grant access.
//
// access_type=offline is what produces a refresh token at all, and
// prompt=consent forces Google to issue a new one even when the operator has
// granted before — without it, reconnecting an account that was already
// connected returns an access token and no refresh token, and the grant dies
// an hour later.
func (s *Store) AuthCodeURL(state string) string {
	return s.oauth.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
	)
}

// Connect exchanges an authorization code and stores the account.
//
// The cursor is seeded from the profile rather than from zero, so the first
// sync does not walk history that predates the grant and file a year of old
// mail as though it just arrived.
func (s *Store) Connect(ctx context.Context, code string) (Account, error) {
	ctx = context.WithValue(ctx, oauth2.HTTPClient, s.client)

	token, err := s.oauth.Exchange(ctx, code)
	if err != nil {
		return Account{}, fmt.Errorf("gmail: exchange the authorization code: %w", err)
	}
	if token.RefreshToken == "" {
		return Account{}, errors.New("gmail: Google returned no refresh token; the grant would expire in an hour")
	}

	profile, err := NewClient(s.oauth.Client(ctx, token), s.baseURL).Profile(ctx)
	if err != nil {
		return Account{}, fmt.Errorf("gmail: read the profile: %w", err)
	}

	sealed, err := s.box.SealString(token.RefreshToken)
	if err != nil {
		return Account{}, err
	}

	stamp := domain.Stamp(s.now())
	row, err := s.repo.Write().SaveGmailAccount(ctx, sqlc.SaveGmailAccountParams{
		Address:         profile.EmailAddress,
		RefreshTokenEnc: sealed,
		Scopes:          strings.Join(Scopes, " "),
		ConnectedAt:     stamp,
		HistoryID:       formatHistoryID(profile.HistoryID),
		CreatedAt:       stamp,
		UpdatedAt:       stamp,
	})
	if err != nil {
		return Account{}, fmt.Errorf("gmail: save the account: %w", err)
	}
	return toAccount(row), nil
}

// Disconnect revokes the grant at Google and forgets the account.
//
// It does not touch the messages already on file: those are the record of what
// arrived, and they outlive the grant that fetched them.
//
// The revocation has to succeed first. Deleting the row is the last moment the
// token exists anywhere, so forgetting it after a failed revocation would leave
// a live grant on the mailbox that nobody can retire — the operator would have
// to go to Google's account page to do by hand what this button said it did. A
// Google outage is worth a "try again" instead.
func (s *Store) Disconnect(ctx context.Context) error {
	if token, err := s.refreshToken(ctx); err == nil && token != "" {
		if err := s.revoke(ctx, token); err != nil {
			return fmt.Errorf("gmail: revoke at Google (the account is still connected): %w", err)
		}
	}
	if _, err := s.repo.Write().DeleteGmailAccount(ctx); err != nil {
		return fmt.Errorf("gmail: disconnect: %w", err)
	}
	return nil
}

// refreshToken reads and decrypts the stored token.
func (s *Store) refreshToken(ctx context.Context) (string, error) {
	row, err := s.repo.Read().GetGmailAccount(ctx)
	if store.NotFound(err) {
		return "", ErrNotConnected
	}
	if err != nil {
		return "", err
	}
	return s.box.OpenString(row.RefreshTokenEnc)
}

// revoke asks Google to retire the grant.
func (s *Store) revoke(ctx context.Context, token string) error {
	form := strings.NewReader("token=" + url.QueryEscape(token))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.revokeURL, form)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	// A token Google has already forgotten comes back 400, and that is the
	// state the caller wanted.
	if resp.StatusCode >= 500 {
		return fmt.Errorf("revoke returned %d", resp.StatusCode)
	}
	return nil
}

// Account reads the connected mailbox, or ErrNotConnected.
func (s *Store) Account(ctx context.Context) (Account, error) {
	row, err := s.repo.Read().GetGmailAccount(ctx)
	if store.NotFound(err) {
		return Account{}, ErrNotConnected
	}
	if err != nil {
		return Account{}, fmt.Errorf("gmail: read the account: %w", err)
	}
	return toAccount(row), nil
}

// Client returns a Gmail client authorized as the connected account.
//
// The token source refreshes on its own and persists a rotated refresh token,
// because Google rotates one occasionally and losing the new one kills the
// grant at the next refresh — silently, days later.
func (s *Store) Client(ctx context.Context) (*Client, Account, error) {
	row, err := s.repo.Read().GetGmailAccount(ctx)
	if store.NotFound(err) {
		return nil, Account{}, ErrNotConnected
	}
	if err != nil {
		return nil, Account{}, fmt.Errorf("gmail: read the account: %w", err)
	}

	refresh, err := s.box.OpenString(row.RefreshTokenEnc)
	if err != nil {
		// The row is there and the key does not open it: the key changed, or
		// the row was edited. Either way this is not a retry.
		return nil, Account{}, fmt.Errorf("gmail: the stored refresh token cannot be decrypted with the configured key: %w", err)
	}

	base := s.oauth.TokenSource(
		context.WithValue(ctx, oauth2.HTTPClient, s.client),
		&oauth2.Token{RefreshToken: refresh},
	)
	source := &persistingSource{store: s, base: base, refresh: refresh}

	return NewClient(oauth2.NewClient(context.WithValue(ctx, oauth2.HTTPClient, s.client), source), s.baseURL),
		toAccount(row), nil
}

// persistingSource writes a rotated refresh token back before handing the
// token to the caller.
type persistingSource struct {
	store   *Store
	base    oauth2.TokenSource
	refresh string
}

func (p *persistingSource) Token() (*oauth2.Token, error) {
	token, err := p.base.Token()
	if err != nil {
		return nil, err
	}
	if token.RefreshToken == "" || token.RefreshToken == p.refresh {
		return token, nil
	}

	sealed, sealErr := p.store.box.SealString(token.RefreshToken)
	if sealErr != nil {
		// The token in hand still works. Failing the request over a failure to
		// store its replacement would be trading a working call for a future
		// problem.
		return token, nil
	}
	stamp := domain.Stamp(p.store.now())
	if err := p.store.repo.Write().SetGmailRefreshToken(context.Background(), sqlc.SetGmailRefreshTokenParams{
		RefreshTokenEnc: sealed, UpdatedAt: stamp,
	}); err == nil {
		p.refresh = token.RefreshToken
	}
	return token, nil
}

// RecordSync advances the cursor after a successful walk.
func (s *Store) RecordSync(ctx context.Context, historyID uint64, count int64) error {
	stamp := domain.Stamp(s.now())
	err := s.repo.Write().SetGmailCursor(ctx, sqlc.SetGmailCursorParams{
		HistoryID:     formatHistoryID(historyID),
		LastSyncAt:    stamp,
		LastSyncCount: count,
		UpdatedAt:     stamp,
	})
	if err != nil {
		return fmt.Errorf("gmail: record the sync: %w", err)
	}
	return nil
}

// RecordWatch stores when the push registration expires.
func (s *Store) RecordWatch(ctx context.Context, expires time.Time) error {
	stamp := domain.Stamp(s.now())
	var expiry *string
	if !expires.IsZero() {
		formatted := domain.Stamp(expires)
		expiry = &formatted
	}
	err := s.repo.Write().SetGmailWatch(ctx, sqlc.SetGmailWatchParams{
		WatchExpiresAt: expiry, UpdatedAt: stamp,
	})
	if err != nil {
		return fmt.Errorf("gmail: record the watch: %w", err)
	}
	return nil
}

// RecordFailure marks the account degraded, or revoked when the grant is gone.
//
// Neither is fatal to the process: the last known state stays on the screen,
// annotated with what went wrong, the same way §6.1 argues for valuations.
func (s *Store) RecordFailure(ctx context.Context, cause error) error {
	status := "degraded"
	if errors.Is(cause, ErrRevoked) {
		status = "revoked"
	}
	detail := cause.Error()
	if len(detail) > 500 {
		detail = detail[:500] + "..."
	}

	stamp := domain.Stamp(s.now())
	err := s.repo.Write().SetGmailStatus(ctx, sqlc.SetGmailStatusParams{
		Status: status, LastError: detail, UpdatedAt: stamp,
	})
	if err != nil {
		return fmt.Errorf("gmail: record the failure: %w", err)
	}
	return nil
}

func toAccount(row sqlc.GmailAccount) Account {
	acct := Account{
		Address:       row.Address,
		Scopes:        strings.Fields(row.Scopes),
		HistoryID:     parseHistoryID(row.HistoryID),
		LastSyncCount: row.LastSyncCount,
		LastError:     row.LastError,
		Status:        row.Status,
	}
	if at, err := time.Parse(time.RFC3339, row.ConnectedAt); err == nil {
		acct.ConnectedAt = at
	}
	acct.WatchExpiresAt = parseOptionalTime(row.WatchExpiresAt)
	acct.LastSyncAt = parseOptionalTime(row.LastSyncAt)
	return acct
}

func parseOptionalTime(value *string) *time.Time {
	if value == nil || *value == "" {
		return nil
	}
	at, err := time.Parse(time.RFC3339, *value)
	if err != nil {
		return nil
	}
	return &at
}

// formatHistoryID renders the cursor for the TEXT column. Zero is stored as
// empty, because "no cursor yet" and "cursor zero" are different claims.
func formatHistoryID(id uint64) string {
	if id == 0 {
		return ""
	}
	return fmt.Sprintf("%d", id)
}

func parseHistoryID(value string) uint64 {
	var id uint64
	if _, err := fmt.Sscanf(value, "%d", &id); err != nil {
		return 0
	}
	return id
}
