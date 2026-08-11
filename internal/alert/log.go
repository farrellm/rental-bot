package alert

import (
	"context"
	"log/slog"

	"github.com/farrellm/rental-bot/internal/domain"
)

// LogSink writes every notice to the process log and records it.
//
// It is subscribed always, including on a host with no Telegram. Two things
// follow from that. The dispatch register on the intake screen has content
// before anybody pairs, so the operator can see what *would* have been sent
// and judge whether the channel is worth setting up. And an alert raised on a
// host that never paired is still on file rather than lost, which matters
// because the first thing anyone does after an outage is look for what the
// process knew and when.
type LogSink struct{ log *slog.Logger }

// NewLogSink builds the sink. A nil logger takes the default.
func NewLogSink(logger *slog.Logger) *LogSink {
	if logger == nil {
		logger = slog.Default()
	}
	return &LogSink{log: logger}
}

// Name is the notifications.channel value.
func (s *LogSink) Name() string { return "log" }

// Deliver writes one line, at the level the severity asks for.
//
// It never fails. A sink that could fail to write to the log would need
// somewhere to report that, and there is nowhere further down to go.
func (s *LogSink) Deliver(_ context.Context, n Notice) error {
	args := []any{"key", n.Key, "severity", n.Severity}
	if n.Detail != "" {
		args = append(args, "detail", n.Detail)
	}
	if n.Restated() {
		args = append(args, "restated", n.SendCount+1, "since", domain.Stamp(n.FirstSeenAt))
	}

	switch {
	case n.Recovered:
		s.log.Info("alert cleared: "+n.Title, args...)
	case n.Severity == Critical:
		s.log.Error("alert: "+n.Title, args...)
	case n.Severity == Warning:
		s.log.Warn("alert: "+n.Title, args...)
	default:
		s.log.Info("alert: "+n.Title, args...)
	}
	return nil
}
