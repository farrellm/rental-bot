package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/farrellm/rental-bot/internal/alert"
	"github.com/farrellm/rental-bot/internal/jobs"
)

func notice(severity alert.Severity, key, title string) alert.Notice {
	return alert.Notice{
		Alert:       alert.Alert{Key: key, Severity: severity, Title: title, Detail: "what happened"},
		FirstSeenAt: time.Now().UTC(),
	}
}

// Pairing ---------------------------------------------------------------

func TestPairingCodeIsShownOnceAndWorksOnce(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	code, expires, err := h.store.IssuePairingCode(ctx)
	if err != nil {
		t.Fatalf("IssuePairingCode: %v", err)
	}
	if len(code) != pairingCodeLength+1 {
		t.Errorf("code = %q, want %d characters and a dash", code, pairingCodeLength)
	}
	if !expires.After(time.Now()) {
		t.Errorf("expires = %s, want a time in the future", expires)
	}

	// Only the hash is on file, so the code is unrecoverable from here.
	state, err := h.store.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state.Paired() {
		t.Error("a chat is paired before anyone used the code")
	}
	if !state.PairingOpen(time.Now()) {
		t.Error("the pairing is not open after a code was issued")
	}

	if err := h.store.Pair(ctx, code, 4471); err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if err := h.store.Pair(ctx, code, 9999); !errors.Is(err, ErrBadPairingCode) {
		t.Errorf("replaying the code = %v, want ErrBadPairingCode", err)
	}

	chat, err := h.store.Chat(ctx)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if chat != 4471 {
		t.Errorf("Chat = %d, want the chat that used the code first", chat)
	}
}

// §8.2 puts re-pairing behind server access. An endpoint that could mint a
// code for an already-paired bot would be a way around that.
func TestAPairedChannelWillNotIssueACode(t *testing.T) {
	h := newHarness(t)
	h.pair(t, 4471)

	if _, _, err := h.store.IssuePairingCode(t.Context()); !errors.Is(err, ErrAlreadyPaired) {
		t.Errorf("IssuePairingCode on a paired channel = %v, want ErrAlreadyPaired", err)
	}
}

func TestUnpairForgetsEverything(t *testing.T) {
	h := newHarness(t)
	h.pair(t, 4471)

	if err := h.store.Unpair(t.Context()); err != nil {
		t.Fatalf("Unpair: %v", err)
	}
	if _, err := h.store.Chat(t.Context()); !errors.Is(err, ErrNotPaired) {
		t.Errorf("Chat after unpairing = %v, want ErrNotPaired", err)
	}
	if err := h.store.Unpair(t.Context()); !errors.Is(err, ErrNotPaired) {
		t.Errorf("Unpair with nothing paired = %v, want ErrNotPaired", err)
	}
}

func TestPairingCommandParsing(t *testing.T) {
	tests := []struct {
		in       string
		wantCode string
		wantOK   bool
	}{
		{"/start 4F2K-9QT1", "4F2K-9QT1", true},
		{"/start 4f2k-9qt1", "4F2K-9QT1", true},
		{"/start@rental_records_bot 4F2K-9QT1", "4F2K-9QT1", true},
		{"  /start   4F2K-9QT1  ", "4F2K-9QT1", true},
		{"/start", "", false},
		{"/start ", "", false},
		{"/status", "", false},
		{"hello", "", false},
		{"", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			code, ok := pairingCommand(strings.TrimSpace(tt.in))
			if ok != tt.wantOK {
				t.Fatalf("ok = %t, want %t", ok, tt.wantOK)
			}
			if code != tt.wantCode {
				t.Errorf("code = %q, want %q", code, tt.wantCode)
			}
		})
	}
}

// The whole inbound surface, driven through the real library against the fake
// API: a code arrives on a chat, and the chat is paired.
func TestThePollerPairsFromAnUpdate(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	code, _, err := h.store.IssuePairingCode(ctx)
	if err != nil {
		t.Fatalf("IssuePairingCode: %v", err)
	}

	poller := newTestPoller(t, h)
	h.fake.queueUpdate(1, 4471, "/start "+code)
	poller.Start(ctx)
	t.Cleanup(func() { stopPoller(t, poller) })

	waitFor(t, "the chat to pair", func() bool {
		state, err := h.store.State(ctx)
		return err == nil && state.Paired()
	})

	state, _ := h.store.State(ctx)
	if *state.ChatID != 4471 {
		t.Errorf("ChatID = %d, want 4471", *state.ChatID)
	}
	if state.LastUpdateID != 1 {
		t.Errorf("LastUpdateID = %d, want 1: the cursor is what stops a restart replaying", state.LastUpdateID)
	}
}

// Bot usernames get discovered and probed. Every update is checked against the
// stored chat and dropped otherwise (§8.2).
func TestThePollerDropsUpdatesFromOtherChats(t *testing.T) {
	h := newHarness(t)
	h.pair(t, 4471)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	poller := newTestPoller(t, h)
	h.fake.queueUpdate(1, 9999, "/status")
	poller.Start(ctx)
	t.Cleanup(func() { stopPoller(t, poller) })

	waitFor(t, "the update to be dropped", func() bool { return poller.Unauthorized() == 1 })

	for _, msg := range h.fake.sentMessages() {
		if msg.ChatID == 9999 {
			t.Error("the poller answered a chat that is not the paired one")
		}
	}
}

// A code that has lapsed is refused, and the refusal says nothing about which
// of wrong, used, or expired it was.
func TestAnExpiredCodeIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	h.store.ttl = time.Millisecond
	code, _, err := h.store.IssuePairingCode(ctx)
	if err != nil {
		t.Fatalf("IssuePairingCode: %v", err)
	}
	time.Sleep(2 * time.Millisecond)

	if err := h.store.Pair(ctx, code, 4471); !errors.Is(err, ErrBadPairingCode) {
		t.Errorf("Pair with an expired code = %v, want ErrBadPairingCode", err)
	}
}

// Delivery --------------------------------------------------------------

func TestARoutineAlertGoesThroughTheQueue(t *testing.T) {
	h := newHarness(t)
	h.pair(t, 4471)
	ctx := t.Context()

	if err := h.sender.Deliver(ctx, notice(alert.Warning, "gmail.watch.lapsed", "The Gmail watch has lapsed")); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	// Nothing has gone out yet: the queue is what carries it, and that is the
	// point of the split.
	if got := len(h.fake.sentMessages()); got != 0 {
		t.Fatalf("sent %d messages before the queue ran, want 0", got)
	}
	depth, err := h.queue.Depth(ctx)
	if err != nil {
		t.Fatalf("Depth: %v", err)
	}
	if depth["pending"] != 1 {
		t.Fatalf("queue depth = %+v, want one pending job", depth)
	}

	runQueue(t, h)

	sent := h.fake.sentMessages()
	if len(sent) != 1 {
		t.Fatalf("sent %d messages after the queue ran, want 1", len(sent))
	}
	if sent[0].ChatID != 4471 {
		t.Errorf("sent to chat %d, want 4471", sent[0].ChatID)
	}
	if !strings.Contains(sent[0].Text, "WARNING") || !strings.Contains(sent[0].Text, "lapsed") {
		t.Errorf("message = %q, want the severity and the title", sent[0].Text)
	}
	if !strings.Contains(sent[0].Text, "https://rental.example.com/intake") {
		t.Errorf("message = %q, want the deep link §8.6 asks for", sent[0].Text)
	}
}

// An alert reporting that the job queue is stuck must not be enqueued on the
// stuck queue (§8.4).
func TestACriticalAlertSkipsTheQueue(t *testing.T) {
	h := newHarness(t)
	h.pair(t, 4471)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h.sender.Start(ctx)
	t.Cleanup(func() { stopSender(t, h) })

	if err := h.sender.Deliver(ctx, notice(alert.Critical, "jobs.backlog", "The job queue is not draining")); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	waitFor(t, "the critical alert to go out", func() bool { return len(h.fake.sentMessages()) == 1 })

	depth, err := h.queue.Depth(ctx)
	if err != nil {
		t.Fatalf("Depth: %v", err)
	}
	if depth["pending"] != 0 {
		t.Errorf("queue depth = %+v, want nothing queued: a critical alert must not ride the queue", depth)
	}
}

// A network blip delays delivery rather than losing it.
func TestACriticalAlertSurvivesADeadChannel(t *testing.T) {
	h := newHarness(t)
	h.pair(t, 4471)
	// The retry would otherwise spend six seconds proving the spool works.
	h.sender.backoff = time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	h.fake.setFailSends(true)
	h.sender.Start(ctx)
	t.Cleanup(func() { stopSender(t, h) })

	if err := h.sender.Deliver(ctx, notice(alert.Critical, "host.disk", "Disk space is low")); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	waitForLong(t, "the alert to reach the spool", func() bool {
		names, err := h.spool.Pending()
		return err == nil && len(names) == 1
	})

	// The channel comes back, and the next attempt takes the spool with it.
	h.fake.setFailSends(false)
	if err := h.sender.Deliver(ctx, notice(alert.Critical, "host.disk", "Disk space is low")); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	waitForLong(t, "the spool to drain", func() bool {
		names, err := h.spool.Pending()
		return err == nil && len(names) == 0
	})
	if got := len(h.fake.sentMessages()); got < 2 {
		t.Errorf("sent %d messages, want the spooled one and the new one", got)
	}
}

// A burst becomes one digest rather than a message a second for the next
// minute (§8.4).
func TestABurstCoalescesIntoADigest(t *testing.T) {
	h := newHarness(t)

	for i, key := range []string{"a.one", "b.two", "c.three"} {
		h.sender.critical <- spooled{At: time.Now(), Key: key, Text: "condition " + string(rune('1'+i))}
	}

	first := <-h.sender.critical
	digest := h.sender.coalesce(first)

	if !strings.Contains(digest.Text, "3 conditions at once") {
		t.Errorf("digest = %q, want it to say how many there were", digest.Text)
	}
	for _, want := range []string{"condition 1", "condition 2", "condition 3"} {
		if !strings.Contains(digest.Text, want) {
			t.Errorf("digest = %q, want it to carry %q", digest.Text, want)
		}
	}
}

// A mute is the operator asking for quiet, not a failure. Critical is never
// suppressed, or the mute would be a way to turn the channel off from inside
// the channel.
func TestTheMuteSuppressesEverythingBelowCritical(t *testing.T) {
	h := newHarness(t)
	h.pair(t, 4471)
	ctx := t.Context()

	if err := h.store.Mute(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Mute: %v", err)
	}

	if err := h.sender.Send(ctx, alert.Warning, "a warning"); err != nil {
		t.Errorf("a muted warning returned %v, want nil: quiet was asked for", err)
	}
	if got := len(h.fake.sentMessages()); got != 0 {
		t.Errorf("sent %d messages while muted, want 0", got)
	}

	if err := h.sender.Send(ctx, alert.Critical, "a critical condition"); err != nil {
		t.Fatalf("Send critical: %v", err)
	}
	if got := len(h.fake.sentMessages()); got != 1 {
		t.Errorf("sent %d messages, want the critical one through the mute", got)
	}
}

// An alert raised where nobody has paired is not a failure, and must not spend
// five attempts discovering that nobody has paired.
func TestSendingWithNothingPaired(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	if err := h.sender.Send(ctx, alert.Warning, "a warning"); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("Send = %v, want ErrNotPaired", err)
	}

	if err := h.sender.Deliver(ctx, notice(alert.Warning, "k", "a condition")); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	runQueue(t, h)

	depth, err := h.queue.Depth(ctx)
	if err != nil {
		t.Fatalf("Depth: %v", err)
	}
	if depth["failed"] != 0 {
		t.Errorf("queue depth = %+v, want the job done rather than failed", depth)
	}
	if depth["done"] != 1 {
		t.Errorf("queue depth = %+v, want the job marked done", depth)
	}
}

func TestARecoveryReadsAsARecovery(t *testing.T) {
	n := notice(alert.Warning, "gmail.watch.lapsed", "The Gmail watch has been renewed")
	n.Recovered = true

	text := render(n, "https://rental.example.com")
	if !strings.HasPrefix(text, "CLEARED: ") {
		t.Errorf("message = %q, want it to open with CLEARED", text)
	}
}

func TestARestatementSaysItIsOne(t *testing.T) {
	n := notice(alert.Warning, "gmail.watch.lapsed", "The Gmail watch has lapsed")
	n.SendCount = 3

	text := render(n, "")
	if !strings.Contains(text, "Said 4 times") {
		t.Errorf("message = %q, want the tally", text)
	}
	if !strings.Contains(text, "Still open") {
		t.Errorf("message = %q, want it to say the condition is still open", text)
	}
}

// §8.6 caps the length and adds a deep link rather than sending a wall of text
// nobody reads on a phone.
func TestAMessageIsCapped(t *testing.T) {
	n := notice(alert.Warning, "k", "a condition")
	n.Detail = strings.Repeat("x", 4000)

	if got := len(render(n, "https://rental.example.com")); got > messageLimit+3 {
		t.Errorf("message is %d characters, want it cut to %d", got, messageLimit)
	}
}

func TestTheRateLimitSpacesMessages(t *testing.T) {
	h := newHarness(t)
	h.pair(t, 4471)
	h.sender.spacing = 80 * time.Millisecond
	ctx := t.Context()

	start := time.Now()
	for range 3 {
		if err := h.sender.Send(ctx, alert.Critical, "a condition"); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	// Two gaps between three messages. The first goes straight out.
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("three messages took %s, want at least two spacings", elapsed)
	}
}

// A delivery clears the degradation in the same statement that records it.
func TestADeliveryClearsTheDegradation(t *testing.T) {
	h := newHarness(t)
	h.pair(t, 4471)
	ctx := t.Context()

	h.fake.setFailSends(true)
	if err := h.sender.Send(ctx, alert.Critical, "a condition"); err == nil {
		t.Fatal("Send succeeded against a dead channel")
	}
	state, _ := h.store.State(ctx)
	if state.Status != "degraded" || state.LastError == "" {
		t.Fatalf("state = %+v, want degraded with a reason", state)
	}

	h.fake.setFailSends(false)
	if err := h.sender.Send(ctx, alert.Critical, "a condition"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	state, _ = h.store.State(ctx)
	if state.Status != "paired" || state.LastError != "" {
		t.Errorf("state = %+v, want paired with no error after a successful send", state)
	}
}

// Helpers ---------------------------------------------------------------

func newTestPoller(t *testing.T, h *harness) *Poller {
	t.Helper()
	poller, err := NewPoller(h.store, testToken, h.fake.URL(), PollerOptions{
		BaseURL:     "https://rental.example.com",
		PollTimeout: 2 * time.Second,
		Logger:      quiet(),
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	return poller
}

func stopPoller(t *testing.T, p *Poller) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// The poll is parked on a socket read the fake holds; not waiting it out
	// is the same trade the process makes at shutdown.
	_ = p.Stop(ctx)
}

func stopSender(t *testing.T, h *harness) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = h.sender.Stop(ctx)
}

// runQueue drains whatever is pending through a real runner, so the test
// exercises the registration as well as the send.
func runQueue(t *testing.T, h *harness) {
	t.Helper()

	runner := jobs.NewRunner(h.queue, jobs.RunnerOptions{
		Workers: 1, PollInterval: 10 * time.Millisecond, Logger: quiet(),
	})
	Register(runner, h.sender, quiet())

	ctx, cancel := context.WithCancel(t.Context())
	runner.Start(ctx)
	defer func() {
		cancel()
		stopCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		_ = runner.Stop(stopCtx)
	}()

	waitFor(t, "the queue to drain", func() bool {
		depth, err := h.queue.Depth(t.Context())
		return err == nil && depth["pending"] == 0 && depth["running"] == 0
	})
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitForLong(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
