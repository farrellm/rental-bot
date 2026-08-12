package ingest

import (
	"context"
	"errors"
	"log/slog"

	"github.com/farrellm/rental-bot/internal/jobs"
	"github.com/farrellm/rental-bot/internal/llm"
)

// Register wires the three stages onto the runner.
//
// The shape is gmail.Register's and telegram.Register's: this package owns its
// kinds and its decoding, and main owns the wiring. A handler is registered
// only when there is a model to run it, which is why this is called from a
// wiring function that returns early on a blank llm.provider.
func Register(runner *jobs.Runner, p *Pipeline, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}

	runner.Handle(KindClassify, func(ctx context.Context, job jobs.Job) error {
		var payload classifyPayload
		if err := job.Decode(&payload); err != nil {
			// An undecodable payload never becomes decodable. Retrying it four
			// more times spends four attempts to reach the same place.
			logger.Error("undecodable classify payload", "error", err, "job_id", job.ID)
			return nil
		}
		return budgeted(p.Classify(ctx, payload.EmailMessageID), logger, job)
	})

	runner.Handle(KindExtract, func(ctx context.Context, job jobs.Job) error {
		var payload extractPayload
		if err := job.Decode(&payload); err != nil {
			logger.Error("undecodable extract payload", "error", err, "job_id", job.ID)
			return nil
		}
		return budgeted(p.Extract(ctx, payload.ProposalID), logger, job)
	})

	runner.Handle(KindSweep, func(ctx context.Context, _ jobs.Job) error {
		_, err := p.Sweep(ctx)
		return err
	})
}

// budgeted turns a spent budget into a finished job rather than a failed one.
//
// The condition is already being reported by the breaker, at critical, with a
// recovery message when it clears. Retrying five times and then dead-lettering
// would report it a second time in a way that says the pipeline is broken,
// which it is not -- the mail is on file and the sweep reads it once there is
// budget again.
func budgeted(err error, logger *slog.Logger, job jobs.Job) error {
	if errors.Is(err, llm.ErrBudgetExceeded) {
		logger.Warn("leaving a message unread until there is budget", "job_id", job.ID, "kind", job.Kind)
		return nil
	}
	return err
}
