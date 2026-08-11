// Package alert is the in-process bus that carries operational conditions to
// whoever is listening (docs/DESIGN.md §8.3).
//
// It knows nothing about Telegram. What it owns is the part that has to be the
// same on every channel: a stable key per condition, a cooldown so a flapping
// condition says one thing and then stays quiet, and an explicit message when
// the condition clears. §8.3 calls deduplication and cooldown mandatory rather
// than polish, and it is right — an alert channel noisy enough to be muted is
// worse than no channel at all.
//
// A channel is a Sink. The bus decides whether something should be said; a
// sink decides how to say it, and on which of §8.4's two delivery paths.
package alert

import (
	"context"
	"fmt"
	"time"

	"github.com/farrellm/rental-bot/internal/domain"
)

// Severity is how loud a condition is.
//
// The three values are the ones the notifications table's CHECK carries, and
// the string is what goes in the column.
type Severity string

const (
	// Info is worth a line in the record and nothing more: a process started,
	// a test notice.
	Info Severity = "info"
	// Warning is degraded but working. A lapsed watch is a warning, because
	// the poller carries ingestion on its own.
	Warning Severity = "warning"
	// Critical is stopped, and nothing but a person is going to fix it.
	Critical Severity = "critical"
)

// Valid reports whether s is one of the three the schema accepts.
func (s Severity) Valid() bool {
	switch s {
	case Info, Warning, Critical:
		return true
	}
	return false
}

// Alert is one condition, as the thing that noticed it describes it.
//
// Key names the *condition*, not the occurrence: two reports of the same
// lapsed watch share it, and two different lapsed watches do not. Everything
// the bus does — the cooldown, the tally, the recovery message — hangs off
// getting that distinction right, so keys are written as dotted constants next
// to the code that raises them rather than being built from a message.
type Alert struct {
	Key      string
	Severity Severity
	Title    string
	Detail   string
}

// Notice is an alert as a sink receives it: the condition, plus what the
// record says about how often it has been stated.
//
// A sink needs the tally to write an honest message. "The Gmail watch has
// lapsed" said for the fourth time is not the same message as the first, and a
// channel that renders them identically teaches its reader to stop looking.
type Notice struct {
	Alert
	// FirstSeenAt is when the condition was first recorded, not when this
	// restatement happened.
	FirstSeenAt time.Time
	// SendCount is how many times this condition has been sent before, so the
	// first delivery of a condition carries zero.
	SendCount int64
	// Recovered marks the message that says the condition has cleared.
	Recovered bool
}

// Restated reports whether this is a repeat rather than the first word on a
// condition.
func (n Notice) Restated() bool { return !n.Recovered && n.SendCount > 0 }

// Sink is one channel an alert can go out on.
//
// Name is the notifications.channel value the bus records against, so a sink
// added later gets its own row per condition rather than sharing one and
// silencing the first.
//
// Deliver may be slow and may fail. The bus records the notification before
// calling it and does not roll that back on an error: a condition that was
// noticed and could not be delivered is still a condition that was noticed,
// and the sink is what owns retrying it.
type Sink interface {
	Name() string
	Deliver(ctx context.Context, n Notice) error
}

// Publisher is what a subsystem needs in order to raise an alert.
//
// Handlers and wiring take this rather than *Bus, so a nil one means alerting
// is not configured — which is a working state, and the state a fresh clone is
// in.
type Publisher interface {
	Publish(ctx context.Context, a Alert)
	Resolve(ctx context.Context, key, title string)
}

// Publish raises a on p when p is non-nil.
//
// Every caller would otherwise write the same nil check, and the one that
// forgot would panic on a host that never configured a channel.
func Publish(ctx context.Context, p Publisher, a Alert) {
	if p == nil {
		return
	}
	p.Publish(ctx, a)
}

// Resolve clears key on p when p is non-nil.
func Resolve(ctx context.Context, p Publisher, key, title string) {
	if p == nil {
		return
	}
	p.Resolve(ctx, key, title)
}

// Errorf builds the detail line for an alert raised from an error.
func Errorf(format string, args ...any) string {
	return domain.Truncate(fmt.Sprintf(format, args...), detailLimit)
}
