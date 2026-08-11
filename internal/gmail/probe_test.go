package gmail

import (
	"testing"
	"time"

	"github.com/farrellm/rental-bot/internal/alert"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// readingsByKey indexes a probe's answer so a test can ask about one condition
// without depending on the order they come back in.
func readingsByKey(t *testing.T, readings []alert.Reading) map[string]alert.Reading {
	t.Helper()
	out := map[string]alert.Reading{}
	for _, r := range readings {
		if _, seen := out[r.Key]; seen {
			t.Fatalf("the probe reported %q twice in one reading", r.Key)
		}
		out[r.Key] = r
	}
	return out
}

// A probe over an unconnected mailbox clears everything rather than reporting
// nothing: nobody asked for ingestion, so whatever was said earlier is no
// longer true.
func TestProbeClearsEverythingWithNoAccount(t *testing.T) {
	h := newHarness(t)
	probe := Probe(h.tokens, 48*time.Hour, nil)

	got := readingsByKey(t, probe(t.Context()))
	for _, key := range []string{KeyGrantRevoked, KeySyncDegraded, KeyWatchLapsed, KeySyncStalled} {
		reading, ok := got[key]
		if !ok {
			t.Errorf("the probe said nothing about %q", key)
			continue
		}
		if !reading.Cleared {
			t.Errorf("%q is outstanding with no mailbox connected", key)
		}
	}
}

func TestProbeReadsTheAccount(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	tests := []struct {
		name string
		// setup mutates the connected account's row.
		setup func(t *testing.T, h *harness)
		// want maps a key to whether the condition should be outstanding.
		want map[string]bool
	}{
		{
			name:  "a healthy mailbox is quiet",
			setup: func(t *testing.T, h *harness) { syncedAt(t, h, now.Add(-10*time.Minute), now.Add(time.Hour)) },
			want: map[string]bool{
				KeyGrantRevoked: false, KeySyncDegraded: false,
				KeyWatchLapsed: false, KeySyncStalled: false,
			},
		},
		{
			name: "a lapsed watch is a warning and nothing else",
			setup: func(t *testing.T, h *harness) {
				syncedAt(t, h, now.Add(-10*time.Minute), now.Add(-time.Hour))
			},
			want: map[string]bool{
				KeyGrantRevoked: false, KeySyncDegraded: false,
				KeyWatchLapsed: true, KeySyncStalled: false,
			},
		},
		{
			name: "a degraded sync is reported",
			setup: func(t *testing.T, h *harness) {
				syncedAt(t, h, now.Add(-10*time.Minute), now.Add(time.Hour))
				setStatus(t, h, "degraded", "the history walk timed out")
			},
			want: map[string]bool{
				KeyGrantRevoked: false, KeySyncDegraded: true,
				KeyWatchLapsed: false, KeySyncStalled: false,
			},
		},
		{
			// A revoked grant already explains the stall and the dead watch.
			// Reporting all three is one condition arriving as three messages.
			name: "a revoked grant speaks for the conditions it causes",
			setup: func(t *testing.T, h *harness) {
				syncedAt(t, h, now.Add(-100*time.Hour), now.Add(-time.Hour))
				setStatus(t, h, "revoked", "the grant was revoked")
			},
			want: map[string]bool{
				KeyGrantRevoked: true, KeySyncDegraded: false,
				KeyWatchLapsed: false, KeySyncStalled: false,
			},
		},
		{
			name: "a poller that stopped running is reported",
			setup: func(t *testing.T, h *harness) {
				syncedAt(t, h, now.Add(-72*time.Hour), now.Add(time.Hour))
			},
			want: map[string]bool{
				KeyGrantRevoked: false, KeySyncDegraded: false,
				KeyWatchLapsed: false, KeySyncStalled: true,
			},
		},
		{
			// The callback enqueues the first sync. A mailbox connected a
			// moment ago has not stalled.
			name:  "a mailbox that has never synced has not stalled",
			setup: func(t *testing.T, h *harness) {},
			want: map[string]bool{
				KeyGrantRevoked: false, KeySyncDegraded: false,
				KeyWatchLapsed: true, KeySyncStalled: false,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.connect(t)
			tt.setup(t, h)

			got := readingsByKey(t, Probe(h.tokens, 48*time.Hour, clock)(t.Context()))
			for key, outstanding := range tt.want {
				reading, ok := got[key]
				if !ok {
					t.Errorf("the probe said nothing about %q", key)
					continue
				}
				if reading.Cleared == outstanding {
					t.Errorf("%q outstanding = %t, want %t", key, !reading.Cleared, outstanding)
				}
			}
		})
	}
}

// Zero turns the stalled-sync check off rather than firing on every sweep.
func TestProbeStalledCheckIsOptional(t *testing.T) {
	h := newHarness(t)
	h.connect(t)

	got := readingsByKey(t, Probe(h.tokens, 0, nil)(t.Context()))
	if _, reported := got[KeySyncStalled]; reported {
		t.Error("the probe reported on a stalled sync with the check turned off")
	}
}

func TestProbeIsNilWithoutAStore(t *testing.T) {
	if Probe(nil, time.Hour, nil) != nil {
		t.Error("a probe was built over a nil store")
	}
}

// syncedAt sets the cursor and the watch expiry directly, which is faster and
// clearer than driving a whole sync to age one column.
func syncedAt(t *testing.T, h *harness, lastSync, watchExpires time.Time) {
	t.Helper()
	ctx := t.Context()
	stamp := time.Now().UTC().Format(time.RFC3339)

	if err := h.repo.Write().SetGmailCursor(ctx, sqlc.SetGmailCursorParams{
		HistoryID:     "5100",
		LastSyncAt:    lastSync.UTC().Format(time.RFC3339),
		LastSyncCount: 0,
		UpdatedAt:     stamp,
	}); err != nil {
		t.Fatalf("SetGmailCursor: %v", err)
	}
	expiry := watchExpires.UTC().Format(time.RFC3339)
	if err := h.repo.Write().SetGmailWatch(ctx, sqlc.SetGmailWatchParams{
		WatchExpiresAt: &expiry,
		UpdatedAt:      stamp,
	}); err != nil {
		t.Fatalf("SetGmailWatch: %v", err)
	}
}

func setStatus(t *testing.T, h *harness, status, detail string) {
	t.Helper()
	if err := h.repo.Write().SetGmailStatus(t.Context(), sqlc.SetGmailStatusParams{
		Status:    status,
		LastError: detail,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("SetGmailStatus: %v", err)
	}
}
