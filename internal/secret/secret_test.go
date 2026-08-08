package secret

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestNewRejectsAnEmptyKey(t *testing.T) {
	if _, err := New(nil); !errors.Is(err, ErrNoKey) {
		t.Fatalf("New(nil) = %v, want ErrNoKey", err)
	}
	if _, err := New([]byte{}); !errors.Is(err, ErrNoKey) {
		t.Fatalf("New(empty) = %v, want ErrNoKey", err)
	}
}

func TestRoundTrip(t *testing.T) {
	box := newBox(t, "a passphrase of no particular length")

	cases := []struct {
		name      string
		plaintext string
	}{
		{"empty", ""},
		{"refresh token", "1//0gVeryLongLookingRefreshTokenValue-abcdef"},
		{"multibyte", "café — policy №1"},
		{"long", strings.Repeat("x", 8192)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sealed, err := box.SealString(tc.plaintext)
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}
			if strings.Contains(sealed, tc.plaintext) && tc.plaintext != "" {
				t.Fatal("the sealed value contains the plaintext")
			}

			got, err := box.OpenString(sealed)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if got != tc.plaintext {
				t.Fatalf("Open = %q, want %q", got, tc.plaintext)
			}
		})
	}
}

// Two seals of one value must not look alike, or the database tells a reader
// which rows hold the same secret without ever decrypting one.
func TestSealIsNotDeterministic(t *testing.T) {
	box := newBox(t, "key")

	first, err := box.SealString("same")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	second, err := box.SealString("same")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if first == second {
		t.Fatal("sealing one value twice produced the same ciphertext")
	}
}

func TestOpenRefusesWhatItCannotAuthenticate(t *testing.T) {
	box := newBox(t, "key")

	sealed, err := box.SealString("the refresh token")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	cases := []struct {
		name  string
		value string
	}{
		{"not base64", "!!!!"},
		{"too short for a nonce", base64.StdEncoding.EncodeToString([]byte("short"))},
		{"tampered", flipLastByte(t, sealed)},
		{"truncated", sealed[:len(sealed)-8]},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := box.OpenString(tc.value); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Open(%s) = %v, want ErrCorrupt", tc.name, err)
			}
		})
	}
}

// A database copy without the key is the threat this package exists for.
func TestAnotherKeyCannotOpenIt(t *testing.T) {
	sealed, err := newBox(t, "the real key").SealString("the refresh token")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if _, err := newBox(t, "a different key").OpenString(sealed); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open with the wrong key = %v, want ErrCorrupt", err)
	}
}

func newBox(t *testing.T, key string) *Box {
	t.Helper()
	box, err := New([]byte(key))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return box
}

func flipLastByte(t *testing.T, sealed string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw[len(raw)-1] ^= 0xff
	return base64.StdEncoding.EncodeToString(raw)
}
