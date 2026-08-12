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
	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/gmail"
	"github.com/farrellm/rental-bot/internal/httpapi"
	"github.com/farrellm/rental-bot/internal/ingest"
	"github.com/farrellm/rental-bot/internal/jobs"
	"github.com/farrellm/rental-bot/internal/llm"
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
	cfg, err := config.Load(m.configPath)
	if err != nil {
		return err
	}

	logger := cfg.Log.Logger(os.Stdout)
	slog.SetDefault(logger)

	// Signals are handled from the top, so a Ctrl-C during startup stops
	// the process rather than being swallowed by a slow migration.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, schema, err := openDatabase(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer db.Close()

	if m.migrateOnly {
		logger.Info("migrations up to date", "schema_version", schema.version, "applied", schema.applied)
		return nil
	}

	// After the migrations, because the tables have to exist, and before the
	// server, because neither of these is a server mode.
	if m.newUser != "" {
		return createUser(ctx, db, m.newUser)
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

	bg := wireBackground(cfg, repo, logger)

	// Ingestion is optional. A fresh clone has no Google project, and the whole
	// subsystem stays unbuilt rather than being built and left to fail: no
	// handlers, no scheduled polls, and a screen that says it is not configured.
	ingestion, err := wireIngestion(cfg, repo, blobs, bg, logger)
	if err != nil {
		return err
	}
	if ingestion.tokens != nil {
		bg.watchdog.Add("gmail", gmail.Probe(ingestion.tokens, cfg.Telegram.SilenceAfter.Duration, nil))
	}

	// The channel is optional too, and for the same reason: nobody has to want
	// a bot for the rest of this to work.
	channel, err := wireTelegram(cfg, repo, bg.bus, bg.queue, bg.runner, logger)
	if err != nil {
		return err
	}

	started := time.Now()
	srv := newHTTPServer(cfg, logger, httpapi.Options{
		Config:       cfg,
		DB:           db,
		Repo:         repo,
		Blobs:        blobs,
		Guard:        guard,
		Queue:        bg.queue,
		Runner:       bg.runner,
		Gmail:        ingestion.tokens,
		Archive:      ingestion.archive,
		PushVerifier: ingestion.verifier,
		Alerts:       bg.bus,
		Telegram:     channel.store,
		Limiter:      auth.NewLimiter(),
		Logger:       logger,
		SPA:          web.SPA(),
		StartedAt:    started,
	})

	logger.Info("starting",
		"version", version.Version,
		"commit", version.Commit,
		"go", version.GoVersion(),
		"addr", cfg.Server.Addr,
		"database", cfg.Database.Path,
		"schema_version", schema.version,
		"spa_embedded", web.Embedded,
		"workers", cfg.Jobs.Workers,
		"gmail", cfg.Gmail.Enabled(),
		"llm", cfg.LLM.Enabled(),
		"telegram", cfg.Telegram.Enabled(),
	)

	// Background work starts before the listener: a job that was mid-run when
	// the last process stopped should be reclaimed and retried whether or not
	// anyone is calling the API.
	//
	// The context here is the signal context, so a Ctrl-C stops the workers at
	// the same instant it stops accepting requests, and Stop below is what
	// waits for the work in flight.
	bg.runner.Start(ctx)
	bg.ticker.Start(ctx)
	if channel.sender != nil {
		channel.sender.Start(ctx)
	}
	if channel.poller != nil {
		channel.poller.Start(ctx)
	}

	// §8.3's Host class: the operator should be able to tell a restart they
	// asked for from one they did not.
	//
	// One condition, never resolved. A restart is not news on its own — a
	// deploy is a restart — but a *rate* of restarts is, and that is exactly
	// what the tally in the dispatch register shows: one line saying "started,
	// 7 times since Tuesday" rather than seven lines saying "started". A clean
	// shutdown deliberately says nothing here; the log records it, and a
	// message announcing that the process has gone away arrives too late to be
	// worth anybody's attention.
	bg.bus.Publish(ctx, alert.Alert{
		Key:      keyProcessStarted,
		Severity: alert.Info,
		Title:    "rental-bot started",
		Detail:   alert.Errorf("Version %s, schema %d.", version.Version, schema.version),
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

	if err := shutdown(srv, bg, channel, logger); err != nil {
		return err
	}

	logger.Info("stopped", "uptime", time.Since(started).Round(time.Second).String())
	return nil
}

// schemaState is where a migration run left the database.
type schemaState struct {
	// version is the highest applied migration.
	version int
	// applied is how many ran just now, which is zero on an already-current
	// database.
	applied int
}

// openDatabase opens the file and applies whatever is pending.
func openDatabase(ctx context.Context, cfg config.Config, logger *slog.Logger) (*store.DB, schemaState, error) {
	db, err := store.Open(ctx, cfg.Database.Path, cfg.Database.ReadPoolSize)
	if err != nil {
		return nil, schemaState{}, err
	}

	applied, err := db.Migrate(ctx, migrations.FS)
	if err != nil {
		db.Close()
		return nil, schemaState{}, err
	}
	for _, m := range applied {
		logger.Info("applied migration", "version", m.Version, "file", m.Filename())
	}

	version, err := db.SchemaVersion(ctx)
	if err != nil {
		db.Close()
		return nil, schemaState{}, err
	}
	return db, schemaState{version: version, applied: len(applied)}, nil
}

// background is the half of the process that runs without anyone asking: the
// queue, the pool that drains it, the ticker that feeds it, and the alert bus
// everything reports to.
//
// Unlike ingestion and channel, none of these is optional. They exist on every
// host, which is what lets the rest of the wiring skip the nil checks.
type background struct {
	bus      *alert.Bus
	queue    *jobs.Queue
	runner   *jobs.Runner
	ticker   *scheduler.Scheduler
	watchdog *alert.Watchdog
}

// wireBackground builds that half.
//
// Everything the webhook and the scheduler want done is enqueued rather than
// run inline, so a Pub/Sub push is answered in milliseconds and a Gmail history
// walk happens on a worker with retries behind it.
//
// The alert bus exists whether or not there is a channel to send on. The log
// sink is always subscribed, so a condition raised on a host that has never
// paired is still recorded and still on the dispatch register — and nothing
// downstream has to check for a nil bus.
func wireBackground(cfg config.Config, repo *store.Repo, logger *slog.Logger) background {
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

	return background{bus: bus, queue: queue, runner: runner, ticker: ticker, watchdog: watchdog}
}

// newHTTPServer builds the listener around the API handler.
func newHTTPServer(cfg config.Config, logger *slog.Logger, opts httpapi.Options) *http.Server {
	return &http.Server{
		Addr:    cfg.Server.Addr,
		Handler: httpapi.New(opts),
		// Slow-loris protection and a bound on a stuck client. The write
		// timeout is generous because document downloads land in M2.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}

// shutdown stops everything, in an order that is the point of the function.
//
// The listener first, so nothing new arrives. Then the background work, then
// the alert channel last of all -- and within the channel, the poller before
// the sender.
func shutdown(srv *http.Server, bg background, channel channel, logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	// The workers were already told to stop by the cancelled signal context;
	// this waits for what is in flight. A job that does not finish in time
	// stays locked, and the next process's reclaim sweep returns it to pending
	// -- the same path a kill -9 takes.
	if err := bg.ticker.Stop(ctx); err != nil {
		logger.Warn("scheduler did not stop cleanly", "error", err)
	}
	if err := bg.runner.Stop(ctx); err != nil {
		logger.Warn("job workers did not stop cleanly", "error", err)
	}

	// The poller before the sender: the poller can still be told to pair, and
	// the sender is what has to be alive to say hello afterwards.
	if channel.poller != nil {
		if err := channel.poller.Stop(ctx); err != nil {
			// Expected, and not worth an error line: the long poll is parked
			// on a socket read Telegram holds for up to fifty seconds. Nothing
			// is lost, because the cursor is on disk.
			logger.Debug("the telegram long poll did not stop cleanly", "error", err)
		}
	}
	if channel.sender != nil {
		// Last, so anything raised on the way down still has somewhere to go —
		// and so what it cannot deliver reaches the spool rather than the
		// floor.
		if err := channel.sender.Stop(ctx); err != nil {
			logger.Warn("the alert sender did not stop cleanly", "error", err)
		}
	}
	return nil
}

// keyProcessStarted names the condition "this process has been restarted".
// It is never resolved; the tally against it is the interesting part.
const keyProcessStarted = "host.process.started"

// ingestion is what the HTTP layer needs from the Gmail subsystem. Every field
// is nil when ingestion is not configured, and every route reads that as "not
// configured" rather than as a failure.
type ingestion struct {
	tokens   *gmail.Store
	archive  *gmail.Archive
	verifier *gmail.Verifier
	// pipeline is nil when no model is configured, which is a working state:
	// mail is still fetched, archived and filed, and nothing reads it.
	pipeline *ingest.Pipeline
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
	bg background,
	logger *slog.Logger,
) (ingestion, error) {
	if !cfg.Gmail.Enabled() {
		logger.Info("email ingestion is not configured; set gmail.client_id to enable it")
		return ingestion{}, nil
	}
	runner, ticker := bg.runner, bg.ticker

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

	// The reading half. It is optional on top of an optional subsystem: a
	// mailbox with no model behind it still collects, archives and files, and
	// the review queue is simply empty.
	pipeline, err := wireIngest(cfg, repo, blobs, box, bg, logger)
	if err != nil {
		return ingestion{}, err
	}
	if pipeline != nil {
		syncer.OnFiled(func(ctx context.Context, messageID int64) {
			if err := pipeline.EnqueueClassify(ctx, messageID); err != nil {
				// Not fatal to the sync. The message is on disk and the sweep
				// is what covers an enqueue that did not happen -- the same
				// division the poller and the webhook already have.
				logger.Error("could not queue a message for reading",
					"email_message_id", messageID, "error", err)
			}
		})
	}

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
	return ingestion{tokens: tokens, archive: archive, verifier: verifier, pipeline: pipeline}, nil
}

// wireIngest builds the reading half, or nothing at all.
//
// A blank llm.provider builds none of it: no classify handler, no extract
// handler, no sweep, and a syncer with nobody listening. That is the state a
// fresh clone is in and it has to be a working one -- the same reason a blank
// gmail.client_id builds no mailbox.
func wireIngest(
	cfg config.Config,
	repo *store.Repo,
	blobs *blob.Store,
	box *secret.Box,
	bg background,
	logger *slog.Logger,
) (*ingest.Pipeline, error) {
	if !cfg.LLM.Enabled() {
		logger.Info("forwarded mail will be filed but not read; set llm.provider to enable it")
		return nil, nil
	}

	client, err := llm.New(cfg.LLM, cfg.Secrets.LLMAPIKey)
	if err != nil {
		return nil, err
	}
	// The breaker reads ingest_proposals, so it needs the repository and
	// nothing else. A budget of zero returns a nil one, which spends freely.
	client = client.WithBudget(llm.NewBudget(repo, cfg.LLM.MonthlyTokenBudget, bg.bus, logger))

	pipeline := ingest.New(ingest.Options{
		Repo:   repo,
		Blobs:  blobs,
		Reader: client,
		Queue:  bg.queue,
		Notify: bg.runner.Notify,
		Box:    box,
		Config: cfg.LLM,
		Alerts: bg.bus,
		Logger: logger,
	})
	ingest.Register(bg.runner, pipeline, logger)

	// The sweep is to the enqueue at sync time what the Gmail poller is to the
	// webhook: the enqueue makes reading fast, this makes it reliable. At start
	// as well as on the tick, because a process that has been down has mail
	// waiting and should not wait a quarter of an hour to find that out.
	bg.ticker.Add(scheduler.Task{
		Name:    ingest.KindSweep,
		Kind:    ingest.KindSweep,
		Every:   cfg.LLM.SweepInterval.Duration,
		AtStart: true,
	})

	logger.Info("forwarded mail will be read",
		"provider", cfg.LLM.Provider,
		"model", cfg.LLM.Model,
		"auto_apply", cfg.LLM.AutoApply,
		"monthly_token_budget", cfg.LLM.MonthlyTokenBudget,
	)
	return pipeline, nil
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
		"expires", domain.Stamp(expires),
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
