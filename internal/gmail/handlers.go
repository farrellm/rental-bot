package gmail

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/farrellm/rental-bot/internal/jobs"
)

// Job kinds. These strings are in the jobs table, so renaming one strands the
// rows already queued under the old name.
const (
	KindSync       = "gmail.sync"
	KindRenewWatch = "gmail.watch.renew"
)

// Register wires the ingestion handlers onto a runner.
func Register(runner *jobs.Runner, syncer *Syncer, watcher *Watcher, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}

	runner.Handle(KindSync, func(ctx context.Context, _ jobs.Job) error {
		result, err := syncer.Sync(ctx)
		if errors.Is(err, ErrNotConnected) {
			// Nothing to do and nothing wrong. A scheduled poll against an
			// unconnected mailbox should not spend five retries discovering
			// that no mailbox is connected.
			logger.Debug("gmail sync skipped: no account is connected")
			return nil
		}
		if err != nil {
			return err
		}
		logger.Info("gmail sync",
			"fetched", result.Fetched,
			"stored", result.Stored,
			"ignored", result.Ignored,
			"failed", result.Failed,
			"resynced", result.Resynced,
		)
		return nil
	})

	runner.Handle(KindRenewWatch, func(ctx context.Context, _ jobs.Job) error {
		expires, err := watcher.Renew(ctx)
		if errors.Is(err, ErrNotConnected) {
			logger.Debug("gmail watch renewal skipped: no account is connected")
			return nil
		}
		if err != nil {
			return err
		}
		logger.Info("gmail watch registered", "expires", expires)
		return nil
	})
}

// Watcher keeps the Pub/Sub push registration alive.
//
// Google expires a watch after seven days whatever it says, which is why this
// is a scheduled job rather than something done once at connect time. A lapsed
// watch is not an outage — the poller carries ingestion on its own — but it
// does turn "within seconds" into "within ten minutes", and the operator should
// be able to see which of the two they are getting.
type Watcher struct {
	tokens *Store
	topic  string
}

// NewWatcher builds a watcher for one topic.
func NewWatcher(tokens *Store, topic string) *Watcher {
	return &Watcher{tokens: tokens, topic: topic}
}

// Renew registers the watch and records when it expires.
func (w *Watcher) Renew(ctx context.Context) (string, error) {
	if w.topic == "" {
		return "", errors.New("gmail: no Pub/Sub topic is configured")
	}

	client, _, err := w.tokens.Client(ctx)
	if err != nil {
		return "", err
	}

	result, err := client.Watch(ctx, w.topic, nil)
	if err != nil {
		if recordErr := w.tokens.RecordFailure(ctx, err); recordErr != nil {
			return "", fmt.Errorf("gmail: watch failed (%v) and the failure could not be recorded: %w", err, recordErr)
		}
		return "", err
	}

	if err := w.tokens.RecordWatch(ctx, result.Expiration); err != nil {
		return "", err
	}
	return result.Expiration.Format("2006-01-02T15:04:05Z07:00"), nil
}

// Stop cancels the push registration and clears the recorded expiry.
func (w *Watcher) Stop(ctx context.Context) error {
	client, _, err := w.tokens.Client(ctx)
	if err != nil {
		return err
	}
	if err := client.StopWatch(ctx); err != nil {
		return err
	}
	return w.tokens.RecordWatch(ctx, time.Time{})
}
