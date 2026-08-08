package gmail

import (
	"context"
	"crypto"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GoogleCertsURL serves the public keys Google's OIDC tokens are signed with.
const GoogleCertsURL = "https://www.googleapis.com/oauth2/v3/certs"

// clockSkew is how far a token's timestamps may disagree with ours.
const clockSkew = 2 * time.Minute

// minCacheTTL keeps a JWKS fetch off the path of every push, even if a response
// arrives with no cache directive at all.
const minCacheTTL = 5 * time.Minute

// ErrUnauthenticated reports a push that did not prove it came from the
// subscription.
//
// The webhook answers 401 and says nothing else. Bot operators probe public
// endpoints, and a verifier that explains which check failed is a verifier that
// helps them.
var ErrUnauthenticated = errors.New("gmail: the push did not authenticate")

// Verifier checks the OIDC token on a Pub/Sub push.
//
// The push payload itself carries only a historyId and is never trusted past
// "something changed" (docs/DESIGN.md §4.2). This is what decides whether even
// that much is believed.
type Verifier struct {
	audience       string
	serviceAccount string
	certsURL       string
	client         *http.Client
	now            func() time.Time

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	keysUntil time.Time
}

// NewVerifier builds a verifier for one subscription.
func NewVerifier(audience, serviceAccount string) *Verifier {
	return &Verifier{
		audience:       audience,
		serviceAccount: strings.ToLower(strings.TrimSpace(serviceAccount)),
		certsURL:       GoogleCertsURL,
		client:         &http.Client{Timeout: 10 * time.Second},
		now:            func() time.Time { return time.Now().UTC() },
	}
}

// SetCertsURL points the verifier at a different JWKS. Tests use it.
func (v *Verifier) SetCertsURL(url string) { v.certsURL = url }

// SetHTTPClient replaces the transport used to fetch the JWKS.
func (v *Verifier) SetHTTPClient(c *http.Client) { v.client = c }

// Configured reports whether there is anything to verify against.
//
// An unconfigured verifier refuses everything. Failing open here would put an
// unauthenticated enqueue endpoint on the public internet, which is worse than
// a webhook that does not work.
func (v *Verifier) Configured() bool {
	return v.audience != "" && v.serviceAccount != ""
}

// Verify checks the bearer token on a push request.
func (v *Verifier) Verify(ctx context.Context, authorization string) error {
	if !v.Configured() {
		return fmt.Errorf("%w: no Pub/Sub audience or service account is configured", ErrUnauthenticated)
	}

	raw := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer"))
	if raw == "" {
		return fmt.Errorf("%w: no bearer token", ErrUnauthenticated)
	}

	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return fmt.Errorf("%w: not a JWT", ErrUnauthenticated)
	}

	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &header); err != nil {
		return fmt.Errorf("%w: unreadable header", ErrUnauthenticated)
	}
	// The algorithm comes from the token, which is written by whoever sent it.
	// Accepting what it names is the classic JWT hole -- "none" and HMAC-with-
	// the-public-key both walk straight through it.
	if header.Alg != "RS256" {
		return fmt.Errorf("%w: signed with %q, not RS256", ErrUnauthenticated, header.Alg)
	}

	var claims struct {
		Issuer        string `json:"iss"`
		Audience      string `json:"aud"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		ExpiresAt     int64  `json:"exp"`
		IssuedAt      int64  `json:"iat"`
	}
	if err := decodeSegment(parts[1], &claims); err != nil {
		return fmt.Errorf("%w: unreadable claims", ErrUnauthenticated)
	}

	key, err := v.key(ctx, header.Kid)
	if err != nil {
		return err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("%w: unreadable signature", ErrUnauthenticated)
	}

	digest := crypto.SHA256.New()
	digest.Write([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest.Sum(nil), signature); err != nil {
		return fmt.Errorf("%w: the signature does not verify", ErrUnauthenticated)
	}

	// Signature first, claims second: an unsigned token's claims are fiction
	// and are not worth reasoning about.
	switch {
	case claims.Issuer != "accounts.google.com" && claims.Issuer != "https://accounts.google.com":
		return fmt.Errorf("%w: issued by %q", ErrUnauthenticated, claims.Issuer)
	case claims.Audience != v.audience:
		// A valid Google token for somebody else's service is still not a token
		// for this one.
		return fmt.Errorf("%w: audience %q", ErrUnauthenticated, claims.Audience)
	case !strings.EqualFold(claims.Email, v.serviceAccount):
		return fmt.Errorf("%w: pushed as %q", ErrUnauthenticated, claims.Email)
	case !claims.EmailVerified:
		return fmt.Errorf("%w: the service account address is unverified", ErrUnauthenticated)
	}

	now := v.now()
	if claims.ExpiresAt == 0 || now.After(time.Unix(claims.ExpiresAt, 0).Add(clockSkew)) {
		return fmt.Errorf("%w: expired", ErrUnauthenticated)
	}
	if claims.IssuedAt != 0 && now.Add(clockSkew).Before(time.Unix(claims.IssuedAt, 0)) {
		return fmt.Errorf("%w: issued in the future", ErrUnauthenticated)
	}
	return nil
}

// key returns the signing key with this id, fetching the JWKS when the cache
// has expired or does not carry it.
//
// A key id that is not cached forces one refresh: Google rotates keys, and a
// verifier that only refreshed on the clock would reject every push for as long
// as its cache had left to run.
func (v *Verifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	if kid == "" {
		return nil, fmt.Errorf("%w: no key id", ErrUnauthenticated)
	}

	v.mu.Lock()
	cached, ok := v.keys[kid]
	fresh := v.now().Before(v.keysUntil)
	v.mu.Unlock()

	if ok && fresh {
		return cached, nil
	}

	keys, ttl, err := v.fetchKeys(ctx)
	if err != nil {
		if ok {
			// Google is unreachable but the key was cached. Using a key that is
			// merely stale beats refusing a legitimate push.
			return cached, nil
		}
		return nil, err
	}

	v.mu.Lock()
	v.keys = keys
	v.keysUntil = v.now().Add(ttl)
	key, found := keys[kid]
	v.mu.Unlock()

	if !found {
		return nil, fmt.Errorf("%w: signed with an unknown key", ErrUnauthenticated)
	}
	return key, nil
}

// fetchKeys reads Google's JWKS and how long it may be held.
func (v *Verifier) fetchKeys(ctx context.Context) (map[string]*rsa.PublicKey, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.certsURL, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("gmail: fetch the signing keys: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("gmail: fetch the signing keys: %d", resp.StatusCode)
	}

	var jwks struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			Alg string `json:"alg"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&jwks); err != nil {
		return nil, 0, fmt.Errorf("gmail: read the signing keys: %w", err)
	}

	keys := make(map[string]*rsa.PublicKey, len(jwks.Keys))
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		modulus, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		exponent, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{
			N: new(big.Int).SetBytes(modulus),
			E: int(new(big.Int).SetBytes(exponent).Int64()),
		}
	}
	if len(keys) == 0 {
		return nil, 0, errors.New("gmail: the signing key set is empty")
	}
	return keys, cacheTTL(resp.Header.Get("Cache-Control")), nil
}

// cacheTTL reads max-age off a Cache-Control header.
func cacheTTL(header string) time.Duration {
	for _, directive := range strings.Split(header, ",") {
		directive = strings.TrimSpace(directive)
		value, ok := strings.CutPrefix(directive, "max-age=")
		if !ok {
			continue
		}
		seconds, err := strconv.Atoi(value)
		if err != nil || seconds <= 0 {
			continue
		}
		return max(time.Duration(seconds)*time.Second, minCacheTTL)
	}
	return minCacheTTL
}

// decodeSegment reads one base64url JWT segment as JSON.
func decodeSegment(segment string, out any) error {
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// PushEnvelope is the Pub/Sub push body.
//
// Only the message id is used, and only for the log line: the payload carries a
// historyId, and believing it would let anyone who got a token replayed at them
// choose which point in history this process resumes from. The sync reads its
// cursor from the database (§4.2 step 2).
type PushEnvelope struct {
	Message struct {
		MessageID   string `json:"messageId"`
		PublishTime string `json:"publishTime"`
	} `json:"message"`
	Subscription string `json:"subscription"`
}
