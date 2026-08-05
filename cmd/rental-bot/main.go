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
	"github.com/farrellm/rental-bot/internal/config"
	"github.com/farrellm/rental-bot/internal/httpapi"
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
	// Secure cookies follow the configured scheme. https in production is not
	// optional; hardcoding it would break a phone testing over the LAN.
	secure := strings.HasPrefix(cfg.Server.BaseURL, "https://")
	guard := auth.NewGuard(auth.NewSessions(repo), auth.NewCSRF(cfg.Secrets.Key), secure, httpapi.WriteProblem)

	started := time.Now()
	handler := httpapi.New(httpapi.Options{
		Config:    cfg,
		DB:        db,
		Repo:      repo,
		Guard:     guard,
		Limiter:   auth.NewLimiter(),
		Logger:    logger,
		SPA:       web.SPA(),
		StartedAt: started,
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
	)

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

	logger.Info("stopped", "uptime", time.Since(started).Round(time.Second).String())
	return nil
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
