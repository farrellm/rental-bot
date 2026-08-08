package gmail

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const (
	testAudience = "https://rental.example.com/webhooks/gmail"
	testAccount  = "gmail-push@rental.iam.gserviceaccount.com"
)

// signer is a fake Google: one RSA key, served as a JWKS.
type signer struct {
	key *rsa.PrivateKey
	kid string
	url string
}

func newSigner(t *testing.T) *signer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	s := &signer{key: key, kid: "test-key-1"}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{
			"kid": s.kid,
			"kty": "RSA",
			"alg": "RS256",
			"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(server.Close)
	s.url = server.URL
	return s
}

// token builds a signed JWT with the given claims layered over valid ones.
func (s *signer) token(t *testing.T, alg string, overrides map[string]any) string {
	t.Helper()

	claims := map[string]any{
		"iss":            "https://accounts.google.com",
		"aud":            testAudience,
		"email":          testAccount,
		"email_verified": true,
		"exp":            time.Now().Add(time.Hour).Unix(),
		"iat":            time.Now().Add(-time.Minute).Unix(),
	}
	for k, v := range overrides {
		if v == nil {
			delete(claims, k)
			continue
		}
		claims[k] = v
	}

	header := map[string]any{"alg": alg, "kid": s.kid, "typ": "JWT"}
	if kid, ok := overrides["__kid"]; ok {
		header["kid"] = kid
		delete(claims, "__kid")
	}

	encode := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}

	body := encode(header) + "." + encode(claims)
	if alg == "none" {
		return body + "."
	}

	digest := crypto.SHA256.New()
	digest.Write([]byte(body))
	signature, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest.Sum(nil))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return body + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func newVerifier(t *testing.T, s *signer) *Verifier {
	t.Helper()
	v := NewVerifier(testAudience, testAccount)
	v.SetCertsURL(s.url)
	return v
}

func TestVerifyAcceptsAGenuinePush(t *testing.T) {
	s := newSigner(t)
	v := newVerifier(t, s)

	if err := v.Verify(t.Context(), "Bearer "+s.token(t, "RS256", nil)); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// The second call is served from the cached key set.
	if err := v.Verify(t.Context(), "Bearer "+s.token(t, "RS256", nil)); err != nil {
		t.Fatalf("second Verify: %v", err)
	}
}

func TestVerifyRefusesEverythingElse(t *testing.T) {
	s := newSigner(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	tests := []struct {
		name  string
		token func(*testing.T) string
	}{
		{"no token", func(*testing.T) string { return "" }},
		{"not a JWT", func(*testing.T) string { return "Bearer nonsense" }},
		{"alg none", func(t *testing.T) string {
			return "Bearer " + s.token(t, "none", nil)
		}},
		{"wrong audience", func(t *testing.T) string {
			return "Bearer " + s.token(t, "RS256", map[string]any{"aud": "https://someone-else.example/hook"})
		}},
		{"wrong issuer", func(t *testing.T) string {
			return "Bearer " + s.token(t, "RS256", map[string]any{"iss": "https://evil.example"})
		}},
		{"wrong service account", func(t *testing.T) string {
			return "Bearer " + s.token(t, "RS256", map[string]any{"email": "someone@else.example"})
		}},
		{"unverified email", func(t *testing.T) string {
			return "Bearer " + s.token(t, "RS256", map[string]any{"email_verified": false})
		}},
		{"expired", func(t *testing.T) string {
			return "Bearer " + s.token(t, "RS256", map[string]any{"exp": time.Now().Add(-time.Hour).Unix()})
		}},
		{"no expiry", func(t *testing.T) string {
			return "Bearer " + s.token(t, "RS256", map[string]any{"exp": nil})
		}},
		{"issued in the future", func(t *testing.T) string {
			return "Bearer " + s.token(t, "RS256", map[string]any{"iat": time.Now().Add(time.Hour).Unix()})
		}},
		{"unknown key id", func(t *testing.T) string {
			return "Bearer " + s.token(t, "RS256", map[string]any{"__kid": "some-other-key"})
		}},
		{"signed by someone else", func(t *testing.T) string {
			forger := &signer{key: other, kid: s.kid, url: s.url}
			return "Bearer " + forger.token(t, "RS256", nil)
		}},
		{"tampered signature", func(t *testing.T) string {
			token := s.token(t, "RS256", nil)
			return "Bearer " + token[:len(token)-4] + "AAAA"
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newVerifier(t, s)
			err := v.Verify(t.Context(), tt.token(t))
			if !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("Verify = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

// Failing open would put an unauthenticated enqueue endpoint on the public
// internet, which is worse than a webhook that does not work.
func TestUnconfiguredVerifierRefusesEverything(t *testing.T) {
	s := newSigner(t)

	for _, v := range []*Verifier{
		NewVerifier("", testAccount),
		NewVerifier(testAudience, ""),
	} {
		v.SetCertsURL(s.url)
		if v.Configured() {
			t.Error("a half-configured verifier reports itself configured")
		}
		if err := v.Verify(t.Context(), "Bearer "+s.token(t, "RS256", nil)); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("Verify = %v, want ErrUnauthenticated", err)
		}
	}
}

func TestCacheTTL(t *testing.T) {
	tests := []struct {
		header string
		want   time.Duration
	}{
		{"public, max-age=3600", time.Hour},
		{"max-age=21600, must-revalidate", 6 * time.Hour},
		{"no-store", minCacheTTL},
		{"", minCacheTTL},
		// Nothing gets a TTL short enough to put a fetch on every push.
		{"max-age=1", minCacheTTL},
	}
	for _, tt := range tests {
		if got := cacheTTL(tt.header); got != tt.want {
			t.Errorf("cacheTTL(%q) = %s, want %s", tt.header, got, tt.want)
		}
	}
}
