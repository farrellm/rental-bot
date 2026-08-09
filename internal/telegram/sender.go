package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/farrellm/rental-bot/internal/alert"
	"github.com/farrellm/rental-bot/internal/jobs"
)

const (
	// messageLimit is well inside Telegram's 4096 characters. §8.6 caps the
	// length and adds a deep link rather than sending a wall of text nobody
	// reads on a phone.
	messageLimit = 1200

	// sendSpacing respects Telegram's roughly one message per second per chat.
	// Going over gets a 429 and a retry_after, which turns an alert storm into
	// a delivery outage.
	sendSpacing = time.Second

	// criticalBuffer is how many critical alerts can be in flight before the
	// spool takes over. Small on purpose: past a handful in flight the channel
	// is not keeping up, and the disk is the honest place for the rest.
	criticalBuffer = 32

	// criticalTries and criticalBackoff bound the direct path's retry. It has
	// no queue behind it, so it does its own, briefly, and then spools.
	criticalTries   = 3
	criticalBackoff = 2 * time.Second
)

// Sender is the alert.Sink that puts a condition in front of a person.
//
// It carries §8.4's two delivery paths, and the split is the whole point:
//
//   - Routine alerts go through the jobs queue and inherit its retries.
//   - Critical alerts cannot. An alert reporting that the job queue is stuck
//     must not be enqueued on the stuck queue. Those go down a small buffered
//     channel drained by a dedicated goroutine with bounded retry, and what
//     that cannot deliver goes to the disk spool.
type Sender struct {
	store  *Store
	client *Client
	queue  *jobs.Queue
	notify func()
	spool  *Spool
	log    *slog.Logger
	// baseURL is the origin the deep link in a message points at.
	baseURL string
	now     func() time.Time

	critical chan spooled

	// sendMu serializes outbound calls so the token bucket is a bucket rather
	// than a suggestion: the job workers and the critical goroutine both send.
	sendMu   sync.Mutex
	lastSend time.Time
	// spacing and backoff are the two waits in this file, as fields so a test
	// can shrink them rather than spending ten seconds proving the spool works.
	spacing time.Duration
	backoff time.Duration

	wg      sync.WaitGroup
	startMu sync.Mutex
	started bool
}

// SenderOptions is what a sender needs that is not a dependency.
type SenderOptions struct {
	// BaseURL is the origin a deep link points at.
	BaseURL string
	Logger  *slog.Logger
}

// NewSender builds the channel's outbound half.
//
// notify may be nil; it wakes the job pool after a routine alert is enqueued,
// so an alert starts going out now rather than at the pool's next poll.
func NewSender(st *Store, client *Client, queue *jobs.Queue, notify func(), spool *Spool, opts SenderOptions) *Sender {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if notify == nil {
		notify = func() {}
	}
	return &Sender{
		store:    st,
		client:   client,
		queue:    queue,
		notify:   notify,
		spool:    spool,
		log:      opts.Logger,
		baseURL:  strings.TrimSuffix(opts.BaseURL, "/"),
		now:      func() time.Time { return time.Now().UTC() },
		spacing:  sendSpacing,
		backoff:  criticalBackoff,
		critical: make(chan spooled, criticalBuffer),
	}
}

// Name is the notifications.channel value.
func (s *Sender) Name() string { return "telegram" }

// Deliver routes a notice onto one of the two paths.
//
// It never blocks for long: the bus calls this from whichever goroutine
// noticed the condition, and that goroutine has its own work to get back to.
func (s *Sender) Deliver(ctx context.Context, n alert.Notice) error {
	text := render(n, s.baseURL)

	if n.Severity == alert.Critical {
		select {
		case s.critical <- spooled{At: s.now(), Key: n.Key, Text: text}:
			return nil
		default:
			// The direct path is backed up. Disk is where this waits; the
			// alternative is dropping the most important message there is.
			s.log.Warn("the critical alert channel is full; spooling", "key", n.Key)
			return s.spool.Add(s.now(), n.Key, text)
		}
	}

	payload := sendPayload{Key: n.Key, Severity: n.Severity, Text: text}
	if _, err := s.queue.Enqueue(ctx, KindSend, payload, jobs.Options{}); err != nil {
		return fmt.Errorf("telegram: queue an alert: %w", err)
	}
	s.notify()
	return nil
}

// sendPayload is what a telegram.send job carries.
//
// The rendered text rather than a notification id: the message says what was
// true when the condition was noticed, and re-rendering it at delivery time
// from a row that has moved on would send a message about a different moment.
// The severity rides along because the mute is applied at delivery, not at
// enqueue -- a mute set while the job waited should still take effect.
type sendPayload struct {
	Key      string         `json:"key"`
	Severity alert.Severity `json:"severity"`
	Text     string         `json:"text"`
}

// Start launches the goroutine that drains the critical path.
//
// It also drains whatever the last run left spooled, because a process that
// restarts after an outage should say what it could not say before it says
// anything new.
func (s *Sender) Start(ctx context.Context) {
	s.startMu.Lock()
	if s.started {
		s.startMu.Unlock()
		return
	}
	s.started = true
	s.startMu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.drainCritical(ctx)
	}()
}

// Stop waits for the critical goroutine, bounded by ctx.
//
// Anything still in the buffer at that point goes to the spool rather than
// down with the process: a shutdown during an incident is exactly when the
// last message matters.
func (s *Sender) Stop(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	var err error
	select {
	case <-done:
	case <-ctx.Done():
		err = errors.New("telegram: the sender did not stop before the shutdown deadline")
	}

	for {
		select {
		case msg := <-s.critical:
			if spoolErr := s.spool.Add(msg.At, msg.Key, msg.Text); spoolErr != nil {
				s.log.Error("spool an undelivered alert during shutdown", "error", spoolErr, "key", msg.Key)
			}
		default:
			return err
		}
	}
}

// drainCritical is the path that does not touch the job queue.
func (s *Sender) drainCritical(ctx context.Context) {
	s.flushSpool(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-s.critical:
			s.sendOrSpool(ctx, s.coalesce(msg))
			s.flushSpool(ctx)
		}
	}
}

// coalesce joins whatever else is already waiting into one message.
//
// §8.4 asks for this: a burst becomes a digest rather than a queue of messages
// arriving one per second for the next minute. Only what is *already* in the
// buffer is taken — this never waits for more.
func (s *Sender) coalesce(first spooled) spooled {
	parts := []string{first.Text}
	keys := []string{first.Key}

	for {
		select {
		case next := <-s.critical:
			parts = append(parts, next.Text)
			keys = append(keys, next.Key)
			if len(parts) >= 10 {
				return digest(first.At, keys, parts)
			}
		default:
			if len(parts) == 1 {
				return first
			}
			return digest(first.At, keys, parts)
		}
	}
}

func digest(at time.Time, keys, parts []string) spooled {
	head := fmt.Sprintf("%d conditions at once:\n\n", len(parts))
	return spooled{
		At:   at,
		Key:  strings.Join(keys, ","),
		Text: truncate(head+strings.Join(parts, "\n\n"), messageLimit),
	}
}

// sendOrSpool tries a few times and then puts the message on disk.
func (s *Sender) sendOrSpool(ctx context.Context, msg spooled) {
	var last error
	for attempt := range criticalTries {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				last = ctx.Err()
			case <-time.After(time.Duration(attempt) * s.backoff):
			}
			if ctx.Err() != nil {
				break
			}
		}
		if err := s.Send(ctx, alert.Critical, msg.Text); err != nil {
			last = err
			continue
		}
		return
	}

	s.log.Error("could not deliver a critical alert; spooling it", "error", last, "key", msg.Key)
	if err := s.spool.Add(msg.At, msg.Key, msg.Text); err != nil {
		// The end of the line. Nothing below this can be told about it.
		s.log.Error("could not spool a critical alert either", "error", err, "key", msg.Key)
	}
}

// flushSpool delivers whatever is waiting on disk, oldest first.
//
// It stops at the first failure rather than working through the whole
// directory: if one send fails the channel is down, and the rest would fail
// the same way and lose their place in the order.
func (s *Sender) flushSpool(ctx context.Context) {
	names, err := s.spool.Pending()
	if err != nil {
		s.log.Error("read the alert spool", "error", err)
		return
	}
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		msg, err := s.spool.Read(name)
		if err != nil {
			// Unreadable, and rereading it next time will not help.
			s.log.Error("drop an unreadable spooled alert", "error", err, "file", name)
			_ = s.spool.Remove(name)
			continue
		}
		// Everything on disk got there by being critical, so nothing here is
		// subject to the mute.
		if err := s.Send(ctx, alert.Critical, msg.Text); err != nil {
			s.log.Warn("the alert channel is still unreachable; leaving the spool", "error", err)
			return
		}
		if err := s.spool.Remove(name); err != nil {
			s.log.Error("remove a delivered spooled alert", "error", err, "file", name)
		}
	}
}

// Send puts one message in front of the paired chat.
//
// This is where the mute and the rate limit apply, rather than at Deliver: a
// mute set while a message sat in the queue should still take effect, and the
// bucket has to cover both paths or it is not a bucket.
//
// A muted message is dropped, not failed. It is not an error that the operator
// asked for quiet, and returning one would spend the job's attempts on it.
func (s *Sender) Send(ctx context.Context, severity alert.Severity, text string) error {
	state, err := s.store.State(ctx)
	if err != nil {
		return err
	}
	if !state.Paired() {
		return ErrNotPaired
	}
	// §8.3: a mute suppresses everything below critical. A mute that could
	// hide a critical alert would be a way to turn the channel off from inside
	// the channel.
	if severity != alert.Critical && state.Muted(s.now()) {
		s.log.Debug("alert suppressed by the mute", "muted_until", state.MutedUntil)
		return nil
	}

	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	if wait := s.spacing - s.now().Sub(s.lastSend); wait > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}

	if err := s.client.SendMessage(ctx, *state.ChatID, text); err != nil {
		if recordErr := s.store.RecordFailure(ctx, err); recordErr != nil {
			s.log.Error("record the delivery failure", "error", recordErr)
		}
		return err
	}
	s.lastSend = s.now()

	if err := s.store.RecordSent(ctx); err != nil {
		s.log.Error("record the delivery", "error", err)
	}
	return nil
}

// render turns a notice into the text that goes out.
//
// §8.6: an alert body is operational detail and never record content. Nothing
// here reads a property, a tenant, or a document — the conditions this
// milestone raises are all about the process itself, and the deep link is what
// carries the reader to anything more specific.
func render(n alert.Notice, baseURL string) string {
	var b strings.Builder

	switch {
	case n.Recovered:
		b.WriteString("CLEARED: ")
	default:
		b.WriteString(strings.ToUpper(string(n.Severity)))
		b.WriteString(": ")
	}
	b.WriteString(n.Title)

	if n.Detail != "" {
		b.WriteString("\n\n")
		b.WriteString(n.Detail)
	}
	if n.Restated() {
		b.WriteString(fmt.Sprintf("\n\nStill open. Said %d times since %s.",
			n.SendCount+1, n.FirstSeenAt.Format("2 Jan 15:04 MST")))
	}
	if baseURL != "" {
		b.WriteString("\n\n")
		b.WriteString(baseURL)
		b.WriteString("/intake")
	}
	return truncate(b.String(), messageLimit)
}
