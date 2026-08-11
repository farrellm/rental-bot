package telegram

import (
	"context"
	"errors"
	"log/slog"

	"github.com/farrellm/rental-bot/internal/jobs"
)

// Register wires the routine delivery path onto a runner.
//
// The subsystem owns its own registration, the way gmail.Register does, so
// main never calls runner.Handle directly and the kind constant never has to
// leave the package that defines it.
func Register(runner *jobs.Runner, sender *Sender, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}

	runner.Handle(KindSend, func(ctx context.Context, job jobs.Job) error {
		var payload sendPayload
		if err := job.Decode(&payload); err != nil {
			// A payload this build cannot read will not become readable on the
			// next attempt. Failing it outright keeps the queue moving.
			logger.Error("undecodable alert payload", "error", err, "job_id", job.ID)
			return nil
		}

		err := sender.Send(ctx, payload.Severity, payload.Text)
		if errors.Is(err, ErrNotPaired) {
			// Nothing to do and nothing wrong. An alert raised on a host where
			// nobody has paired should not spend five attempts discovering
			// that nobody has paired; the log sink already has it on file.
			logger.Debug("alert not sent: no chat is paired", "key", payload.Key)
			return nil
		}
		return err
	})
}
