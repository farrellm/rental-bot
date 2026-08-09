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

	"github.com/farrellm/rental-bot/internal/alert"
	"github.com/farrellm/rental-bot/internal/auth"
	"github.com/farrellm/rental-bot/internal/blob"
	"github.com/farrellm/rental-bot/internal/config"
	"github.com/farrellm/rental-bot/internal/gmail"
	"github.com/farrellm/rental-bot/internal/httpapi"
	"github.com/farrellm/rental-bot/internal/jobs"
	"github.com/farrellm/rental-bot/internal/scheduler"
	"github.com/farrellm/rental-bot/internal/secret"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/telegram"
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
		unpair      = flag.Bool("unpair-telegram", false, "forget the paired Telegram chat and exit")
		showVersion = flag.Bool("version", false, "print the build identity and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("rental-bot", version.String())
		return
	}

	if err := run(modes{
		configPath:  *configPath,
		migrateOnly: *migrateOnly,
		newUser:     *newUser,
		unpair:      *unpair,
	}); err != nil {
		slog.Default().Error("fatal", "error", err)
		os.Exit(1)
	}
}

// modes carries the do-one-thing-and-exit flags, so run's signature does not
// grow a boolean per flag.
type modes struct {
	configPath  string
	migrateOnly bool
	newUser     string
	unpair      bool
}

func run(m modes) error {
	configPath := m.configPath
	migrateOnly := m.migrateOnly
	newUser := m.newUser
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

	// After the migrations, because the tables have to exist, and before the
	// server, because neither of these is a server mode.
	if newUser != "" {
		return createUser(ctx, db, newUser)
	}
	if m.unpair {
		return unpairTelegram(ctx, db)
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
	// The alert bus exists whether or not there is a channel to send on. The
	// log sink is always subscribed, so a condition raised on a host that has
	// never paired is still recorded and still on the dispatch register — and
	// the wiring below does not have to check for a nil bus.
	bus := alert.New(repo, alert.Options{
		Cooldown:         cfg.Telegram.Cooldown.Duration,
		CriticalCooldown: cfg.Telegram.CriticalCooldown.Duration,
		Logger:           logger,
	})
	bus.Subscribe(alert.NewLogSink(logger))

	queue := jobs.NewQueue(repo)
	runner := jobs.NewRunner(queue, jobs.RunnerOptions{
		Workers:      cfg.Jobs.Workers,
		PollInterval: cfg.Jobs.PollInterval.Duration,
		LeaseTimeout: cfg.Jobs.LeaseTimeout.Duration,
		Logger:       logger,
		// A job past max_attempts is a failed row that nothing retries and
		// nothing reads. This is the only voice it has.
		OnDeadLetter: func(ctx context.Context, job jobs.Job, cause error) {
			bus.Publish(ctx, alert.Alert{
				// Keyed by kind, not by id: five sync jobs failing is one
				// condition, and five messages about it is the noise §8.3
				// exists to prevent.
				Key:      "jobs.dead_letter." + job.Kind,
				Severity: alert.Critical,
				Title:    "A " + job.Kind + " job gave up",
				Detail: alert.Errorf("It failed %d times and nothing will retry it. Last error: %v",
					job.Attempts, cause),
			})
		},
	})

	ticker := scheduler.New(queue, runner.Notify, logger)
	watchdog := alert.NewWatchdog(bus, logger)
	watchdog.Add("queue", alert.QueueDepthProbe(queue, cfg.Telegram.QueueBacklogThreshold))
	alert.RegisterSweep(runner, watchdog)
	ticker.Add(scheduler.Task{
		Name:  alert.KindSweep,
		Kind:  alert.KindSweep,
		Every: cfg.Telegram.SweepInterval.Duration,
		// At start as well as on the tick: a process coming back after a long
		// outage should say what is wrong now, not in five minutes.
		AtStart: true,
	})

	// Ingestion is optional. A fresh clone has no Google project, and the whole
	// subsystem stays unbuilt rather than being built and left to fail: no
	// handlers, no scheduled polls, and a screen that says it is not configured.
	ingestion, err := wireIngestion(cfg, repo, blobs, runner, ticker, logger)
	if err != nil {
		return err
	}
	if ingestion.tokens != nil {
		watchdog.Add("gmail", gmail.Probe(ingestion.tokens, cfg.Telegram.SilenceAfter.Duration, nil))
	}

	// The channel is optional too, and for the same reason: nobody has to want
	// a bot for the rest of this to work.
	channel, err := wireTelegram(cfg, repo, bus, queue, runner, logger)
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
		Alerts:       bus,
		Telegram:     channel.store,
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
		"telegram", cfg.Telegram.Enabled(),
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
	if channel.sender != nil {
		channel.sender.Start(ctx)
	}
	if channel.poller != nil {
		channel.poller.Start(ctx)
	}

	// §8.3's Host class: the operator should be able to tell a restart they
	// asked for from one they did not.
	//
	// "Started" is a condition rather than an event here, and the shutdown
	// below is what clears it. An open line on the dispatch register then
	// means the process is running and a ruled-off one means it stopped
	// cleanly — and a crash loop, which never reaches the clear, restates the
	// same line with a climbing tally instead of filling the register.
	bus.Publish(ctx, alert.Alert{
		Key:      keyProcessStarted,
		Severity: alert.Info,
		Title:    "rental-bot started",
		Detail:   alert.Errorf("Version %s, schema %d.", version.Version, schemaVersion),
	})

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

	// The poller before the sender: the poller can still be told to pair, and
	// the sender is what has to be alive to say hello afterwards.
	if channel.poller != nil {
		if err := channel.poller.Stop(shutdownCtx); err != nil {
			// Expected, and not worth an error line: the long poll is parked
			// on a socket read Telegram holds for up to fifty seconds. Nothing
			// is lost, because the cursor is on disk.
			logger.Debug("the telegram long poll did not stop cleanly", "error", err)
		}
	}
	if channel.sender != nil {
		// This clears the started condition and then drains, so a clean stop
		// is the last thing that goes out rather than the first thing lost.
		bus.Resolve(shutdownCtx, keyProcessStarted, "rental-bot stopped")
		if err := channel.sender.Stop(shutdownCtx); err != nil {
			logger.Warn("the alert sender did not stop cleanly", "error", err)
		}
	} else {
		bus.Resolve(shutdownCtx, keyProcessStarted, "rental-bot stopped")
	}

	logger.Info("stopped", "uptime", time.Since(started).Round(time.Second).String())
	return nil
}

// keyProcessStarted names the condition "this process is running". It is
// raised at startup and cleared at a clean shutdown.
const keyProcessStarted = "host.process.started"

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

// channel is what the rest of the process needs from the Telegram subsystem.
// Every field is nil when no bot is configured, which is a working state.
type channel struct {
	store  *telegram.Store
	sender *telegram.Sender
	poller *telegram.Poller
}

// wireTelegram builds the alert channel, or nothing at all.
//
// A blank telegram.bot_username builds none of it. The alert bus still runs —
// the log sink is subscribed either way — so conditions are still recorded and
// still on the dispatch register; there is simply nowhere to send them.
func wireTelegram(
	cfg config.Config,
	repo *store.Repo,
	bus *alert.Bus,
	queue *jobs.Queue,
	runner *jobs.Runner,
	logger *slog.Logger,
) (channel, error) {
	if !cfg.Telegram.Enabled() {
		logger.Info("the alert channel is not configured; set telegram.bot_username to enable it")
		return channel{}, nil
	}

	tokens := telegram.NewStore(repo, cfg.Telegram.PairingTTL.Duration)

	spool, err := telegram.NewSpool(cfg.Storage.Spool)
	if err != nil {
		return channel{}, err
	}
	client, err := telegram.NewClient(cfg.Secrets.TelegramBotToken, telegram.DefaultBaseURL)
	if err != nil {
		return channel{}, err
	}

	baseURL := strings.TrimSuffix(cfg.Server.BaseURL, "/")
	sender := telegram.NewSender(tokens, client, queue, runner.Notify, spool, telegram.SenderOptions{
		BaseURL: baseURL,
		Logger:  logger,
	})
	telegram.Register(runner, sender, logger)
	bus.Subscribe(sender)

	poller, err := telegram.NewPoller(tokens, cfg.Secrets.TelegramBotToken, telegram.DefaultBaseURL,
		telegram.PollerOptions{
			BaseURL:     baseURL,
			PollTimeout: cfg.Telegram.PollInterval.Duration,
			Logger:      logger,
		})
	if err != nil {
		return channel{}, err
	}

	// A code at startup, so pairing works from a shell on a host whose web app
	// is not reachable yet — which is the situation a first deploy is in, and
	// the one §8.2 describes. The screen offers a fresh one; this is the
	// fallback, not the only way.
	logPairingCode(context.Background(), tokens, cfg.Telegram.BotUsername, logger)

	logger.Info("the alert channel is configured",
		"bot", "@"+cfg.Telegram.BotUsername,
		"cooldown", cfg.Telegram.Cooldown.Duration,
		"spool", spool.Dir(),
	)
	return channel{store: tokens, sender: sender, poller: poller}, nil
}

// logPairingCode prints a code when nobody has paired, and says nothing when
// somebody has.
func logPairingCode(ctx context.Context, tokens *telegram.Store, username string, logger *slog.Logger) {
	code, expires, err := tokens.IssuePairingCode(ctx)
	switch {
	case errors.Is(err, telegram.ErrAlreadyPaired):
		return
	case err != nil:
		logger.Error("could not issue a telegram pairing code", "error", err)
		return
	}
	logger.Warn("the alert channel is not paired",
		"send", "/start "+code,
		"to", "@"+username,
		"expires", expires.Format(time.RFC3339),
	)
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
