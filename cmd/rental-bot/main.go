// Command rental-bot is the single process that serves the API, the SPA, and
// (from M3 onward) the ingestion, job, and alerting subsystems.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/farrellm/rental-bot/internal/auth"
	"github.com/farrellm/rental-bot/internal/blob"
	"github.com/farrellm/rental-bot/internal/config"
	"github.com/farrellm/rental-bot/internal/gmail"
	"github.com/farrellm/rental-bot/internal/httpapi"
	"github.com/farrellm/rental-bot/internal/jobs"
	"github.com/farrellm/rental-bot/internal/scheduler"
	"github.com/farrellm/rental-bot/internal/secret"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/version"
	"github.com/farrellm/rental-bot/migrations"
	"github.com/farrellm/rental-bot/web"
)

// shutdownTimeout bounds how long in-flight requests get to finish.
const shutdownTimeout = 15 * time.Second

func main() {
	var (
		configPath  = flag.String("config", "config.toml", "path to the TOML config file; missing is not an error")
		migrateOnly = flag.Bool("migrate", false, "apply migrations and exit")
		newUser     = flag.String("create-user", "", "create this user (or reset their password) and exit")
		showVersion = flag.Bool("version", false, "print the build identity and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("rental-bot", version.String())
		return
	}

	if err := run(*configPath, *migrateOnly, *newUser); err != nil {
		slog.Default().Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(configPath string, migrateOnly bool, newUser string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	logger := cfg.Log.Logger(os.Stdout)
	slog.SetDefault(logger)

	// Signals are handled from the top, so a Ctrl-C during startup stops
	// the process rather than being swallowed by a slow migration.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.Database.Path, cfg.Database.ReadPoolSize)
	if err != nil {
		return err
	}
	defer db.Close()

	applied, err := db.Migrate(ctx, migrations.FS)
	if err != nil {
		return err
	}
	for _, m := range applied {
		logger.Info("applied migration", "version", m.Version, "file", m.Filename())
	}

	schemaVersion, err := db.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	if migrateOnly {
		logger.Info("migrations up to date", "schema_version", schemaVersion, "applied", len(applied))
		return nil
	}

	// After the migrations, because the users table has to exist, and before
	// the server, because this is not a server mode.
	if newUser != "" {
		return createUser(ctx, db, newUser)
	}

	if err := ensureDirs(cfg); err != nil {
		return err
	}

	repo := db.Repo()
	blobs, err := blob.New(cfg.Storage.Blobs)
	if err != nil {
		return err
	}

	// Secure cookies follow the configured scheme. https in production is not
	// optional; hardcoding it would break a phone testing over the LAN.
	secure := strings.HasPrefix(cfg.Server.BaseURL, "https://")
	guard := auth.NewGuard(auth.NewSessions(repo), auth.NewCSRF(cfg.Secrets.Key), secure, httpapi.WriteProblem)

	// The queue and the pool that drains it. Everything the webhook and the
	// scheduler want done goes through here rather than running inline, so a
	// Pub/Sub push is answered in milliseconds and a Gmail history walk
	// happens on a worker with retries behind it.
	queue := jobs.NewQueue(repo)
	runner := jobs.NewRunner(queue, jobs.RunnerOptions{
		Workers:      cfg.Jobs.Workers,
		PollInterval: cfg.Jobs.PollInterval.Duration,
		LeaseTimeout: cfg.Jobs.LeaseTimeout.Duration,
		Logger:       logger,
	})

	ticker := scheduler.New(queue, runner.Notify, logger)

	// Ingestion is optional. A fresh clone has no Google project, and the whole
	// subsystem stays unbuilt rather than being built and left to fail: no
	// handlers, no scheduled polls, and a screen that says it is not configured.
	ingestion, err := wireIngestion(cfg, repo, blobs, runner, ticker, logger)
	if err != nil {
		return err
	}

	started := time.Now()
	handler := httpapi.New(httpapi.Options{
		Config:       cfg,
		DB:           db,
		Repo:         repo,
		Blobs:        blobs,
		Guard:        guard,
		Queue:        queue,
		Runner:       runner,
		Gmail:        ingestion.tokens,
		Archive:      ingestion.archive,
		PushVerifier: ingestion.verifier,
		Limiter:      auth.NewLimiter(),
		Logger:       logger,
		SPA:          web.SPA(),
		StartedAt:    started,
	})

	srv := &http.Server{
		Addr:    cfg.Server.Addr,
		Handler: handler,
		// Slow-loris protection and a bound on a stuck client. The write
		// timeout is generous because document downloads land in M2.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	logger.Info("starting",
		"version", version.Version,
		"commit", version.Commit,
		"go", version.GoVersion(),
		"addr", cfg.Server.Addr,
		"database", cfg.Database.Path,
		"schema_version", schemaVersion,
		"spa_embedded", web.Embedded,
		"workers", cfg.Jobs.Workers,
		"gmail", cfg.Gmail.Enabled(),
	)

	// Background work starts before the listener: a job that was mid-run when
	// the last process stopped should be reclaimed and retried whether or not
	// anyone is calling the API.
	//
	// The context here is the signal context, so a Ctrl-C stops the workers at
	// the same instant it stops accepting requests, and Stop below is what
	// waits for the work in flight.
	runner.Start(ctx)
	ticker.Start(ctx)

	errc := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		stop() // a second signal now kills the process outright
		logger.Info("shutting down", "timeout", shutdownTimeout)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	// The workers were already told to stop by the cancelled signal context;
	// this waits for what is in flight. A job that does not finish in time
	// stays locked, and the next process's reclaim sweep returns it to pending
	// -- the same path a kill -9 takes.
	if err := ticker.Stop(shutdownCtx); err != nil {
		logger.Warn("scheduler did not stop cleanly", "error", err)
	}
	if err := runner.Stop(shutdownCtx); err != nil {
		logger.Warn("job workers did not stop cleanly", "error", err)
	}

	logger.Info("stopped", "uptime", time.Since(started).Round(time.Second).String())
	return nil
}

// ingestion is what the HTTP layer needs from the Gmail subsystem. Every field
// is nil when ingestion is not configured, and every route reads that as "not
// configured" rather than as a failure.
type ingestion struct {
	tokens   *gmail.Store
	archive  *gmail.Archive
	verifier *gmail.Verifier
}

// wireIngestion builds the Gmail subsystem, or nothing at all.
//
// A blank gmail.client_id builds none of it. That is a working state and the
// one a fresh clone is in — not an error, and not a mailbox that looks
// disconnected when no mailbox was ever asked for.
func wireIngestion(
	cfg config.Config,
	repo *store.Repo,
	blobs *blob.Store,
	runner *jobs.Runner,
	ticker *scheduler.Scheduler,
	logger *slog.Logger,
) (ingestion, error) {
	if !cfg.Gmail.Enabled() {
		logger.Info("email ingestion is not configured; set gmail.client_id to enable it")
		return ingestion{}, nil
	}

	// config.Validate has already refused an ingestion setup without a key, so
	// this failing means the key itself is unusable.
	box, err := secret.New(cfg.Secrets.Key)
	if err != nil {
		return ingestion{}, fmt.Errorf("email ingestion needs the encryption key: %w", err)
	}
	archive, err := gmail.NewArchive(cfg.Storage.RawEmail)
	if err != nil {
		return ingestion{}, err
	}

	tokens := gmail.NewStore(repo, box, cfg, strings.TrimSuffix(cfg.Server.BaseURL, "/")+gmail.CallbackPath)
	syncer := gmail.NewSyncer(tokens, repo, blobs, archive, cfg.Gmail, logger)
	watcher := gmail.NewWatcher(tokens, cfg.Gmail.Topic)
	gmail.Register(runner, syncer, watcher, logger)

	// The verifier is built even when the Pub/Sub half is unconfigured. It
	// refuses everything in that state, which is what the webhook should do
	// when there is nothing to check a push against.
	verifier := gmail.NewVerifier(cfg.Gmail.PubSub.Audience, cfg.Gmail.PubSub.ServiceAccount)
	if !verifier.Configured() {
		logger.Warn("the Gmail push endpoint will refuse every request; set gmail.pubsub.audience and gmail.pubsub.service_account")
	}

	// The poller, not the webhook, is what makes ingestion reliable (§4.2).
	// Pub/Sub is at-least-once and occasionally lossy, and a watch can lapse
	// silently; every step is idempotent, so the overlap costs nothing.
	ticker.Add(scheduler.Task{
		Name:  gmail.KindSync,
		Kind:  gmail.KindSync,
		Every: cfg.Gmail.PollInterval.Duration,
	})
	// At start as well as on the tick: a process that has been down for a week
	// has a lapsed watch and should not wait a day to find that out.
	ticker.Add(scheduler.Task{
		Name:    gmail.KindRenewWatch,
		Kind:    gmail.KindRenewWatch,
		Every:   cfg.Gmail.WatchRenewInterval.Duration,
		AtStart: true,
	})

	logger.Info("email ingestion is configured",
		"poll_interval", cfg.Gmail.PollInterval.Duration,
		"senders", len(cfg.Gmail.AllowedSenders),
		"push", verifier.Configured(),
	)
	return ingestion{tokens: tokens, archive: archive, verifier: verifier}, nil
}

// ensureDirs creates the storage directories so a write in a later milestone
// fails at startup here, not halfway through ingesting an email.
func ensureDirs(cfg config.Config) error {
	for _, dir := range cfg.Dirs() {
		if dir == "" || dir == "." {
			continue
		}
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}
