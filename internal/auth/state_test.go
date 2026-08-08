package auth

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newStateGuard(t *testing.T, key string) *Guard {
	t.Helper()
	return NewGuard(nil, NewCSRF([]byte(key)), false, nil)
}

func TestStateRoundTrips(t *testing.T) {
	g := newStateGuard(t, "a key")

	state, err := g.IssueState("gmail-connect", "session-hash")
	if err != nil {
		t.Fatalf("IssueState: %v", err)
	}
	if err := g.CheckState("gmail-connect", "session-hash", state); err != nil {
		t.Fatalf("CheckState: %v", err)
	}
}

// Two issues must differ, or a state captured once is a state forever.
func TestStateIsNotDeterministic(t *testing.T) {
	g := newStateGuard(t, "a key")

	first, err := g.IssueState("gmail-connect", "session-hash")
	if err != nil {
		t.Fatalf("IssueState: %v", err)
	}
	second, err := g.IssueState("gmail-connect", "session-hash")
	if err != nil {
		t.Fatalf("IssueState: %v", err)
	}
	if first == second {
		t.Fatal("two issued states are identical")
	}
}

// This is the login-CSRF defense: an attacker who starts the flow with their
// own Google account must not be able to hand the callback to the operator and
// have the operator's session accept it.
func TestStateFromAnotherSessionIsRefused(t *testing.T) {
	g := newStateGuard(t, "a key")

	attacker, err := g.IssueState("gmail-connect", "the-attackers-session")
	if err != nil {
		t.Fatalf("IssueState: %v", err)
	}
	if err := g.CheckState("gmail-connect", "the-operators-session", attacker); !errors.Is(err, ErrBadState) {
		t.Fatalf("CheckState = %v, want ErrBadState", err)
	}
}

func TestStateRefusesEverythingElse(t *testing.T) {
	g := newStateGuard(t, "a key")
	valid, err := g.IssueState("gmail-connect", "session-hash")
	if err != nil {
		t.Fatalf("IssueState: %v", err)
	}

	// An expiry the caller chose, MAC'd for a different body.
	future := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	parts := strings.Split(valid, ".")
	forgedExpiry := parts[0] + "." + future + "." + parts[2]

	// One issued fifteen minutes and a second ago.
	stale := staleState(t, g, "gmail-connect", "session-hash")

	tests := []struct {
		name    string
		purpose string
		session string
		state   string
	}{
		{"empty", "gmail-connect", "session-hash", ""},
		{"no session", "gmail-connect", "", valid},
		{"not three parts", "gmail-connect", "session-hash", "abc.def"},
		{"another purpose", "some-other-flow", "session-hash", valid},
		{"tampered mac", "gmail-connect", "session-hash", parts[0] + "." + parts[1] + ".00"},
		{"extended expiry", "gmail-connect", "session-hash", forgedExpiry},
		{"expired", "gmail-connect", "session-hash", stale},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := g.CheckState(tt.purpose, tt.session, tt.state); !errors.Is(err, ErrBadState) {
				t.Fatalf("CheckState = %v, want ErrBadState", err)
			}
		})
	}
}

// A state signed with another server's key does not verify here.
func TestStateFromAnotherKeyIsRefused(t *testing.T) {
	state, err := newStateGuard(t, "one key").IssueState("gmail-connect", "session-hash")
	if err != nil {
		t.Fatalf("IssueState: %v", err)
	}
	if err := newStateGuard(t, "another key").CheckState("gmail-connect", "session-hash", state); !errors.Is(err, ErrBadState) {
		t.Fatalf("CheckState = %v, want ErrBadState", err)
	}
}

func TestIssueStateNeedsASession(t *testing.T) {
	if _, err := newStateGuard(t, "a key").IssueState("gmail-connect", ""); err == nil {
		t.Fatal("IssueState succeeded without a session")
	}
}

// staleState builds a correctly signed state whose expiry has passed.
func staleState(t *testing.T, g *Guard, purpose, session string) string {
	t.Helper()
	body := "AAAAAAAAAAAAAAAAAAAAAA." + strconv.FormatInt(time.Now().Add(-time.Second).Unix(), 10)
	return body + "." + g.stateMAC(purpose, session, body)
}
