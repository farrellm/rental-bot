package auth

import (
	"sync"
	"time"
)

// Rate limiting for sign-in, per IP and per account (docs/DESIGN.md §7.1).
//
// The state is in memory and does not survive a restart. That is a deliberate
// trade: persisting it would put a write on every failed sign-in, and this is
// a single-operator box where a restart is an operator action rather than
// something an attacker can provoke. An attacker who can restart the process
// already has more than a password.
const (
	// freeAttempts are allowed before any delay. Typing a password wrong twice
	// is a Tuesday, not an attack.
	freeAttempts = 3

	// backoffBase is the first delay, doubling with each further failure.
	backoffBase = 1 * time.Second

	// backoffMax caps the delay, so a locked-out operator waits minutes rather
	// than being locked out until the next restart.
	backoffMax = 15 * time.Minute

	// forgetAfter is how long an idle key is kept. Beyond it the record is
	// evicted and the caller starts clean.
	forgetAfter = 1 * time.Hour
)

// Limiter tracks failed attempts by key and applies exponential backoff.
// The zero value is not usable; call NewLimiter.
type Limiter struct {
	mu      sync.Mutex
	records map[string]*record
	now     func() time.Time
	swept   time.Time
}

type record struct {
	failures int
	until    time.Time
	seen     time.Time
}

// NewLimiter returns an empty limiter.
func NewLimiter() *Limiter {
	return &Limiter{records: make(map[string]*record), now: time.Now}
}

// Allow reports whether key may attempt now. When it may not, the second
// return is how long the caller has to wait, which the handler reports as
// Retry-After so the client can say something specific.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweepLocked(now)

	r, ok := l.records[key]
	if !ok || !now.Before(r.until) {
		return true, 0
	}
	return false, r.until.Sub(now)
}

// Fail records a failed attempt and extends the backoff.
func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	r, ok := l.records[key]
	if !ok {
		r = &record{}
		l.records[key] = r
	}
	r.failures++
	r.seen = now

	if r.failures >= freeAttempts {
		delay := backoffBase << min(r.failures-freeAttempts, 30)
		if delay > backoffMax || delay <= 0 {
			delay = backoffMax
		}
		r.until = now.Add(delay)
	}
}

// Reset clears the record for key, which a successful sign-in does.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.records, key)
}

// sweepLocked evicts idle records. It runs at most once a minute, so a busy
// limiter does not walk the whole map on every request.
func (l *Limiter) sweepLocked(now time.Time) {
	if now.Sub(l.swept) < time.Minute {
		return
	}
	l.swept = now
	for key, r := range l.records {
		if now.Sub(r.seen) > forgetAfter && !now.Before(r.until) {
			delete(l.records, key)
		}
	}
}
