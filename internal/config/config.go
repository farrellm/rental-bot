// Package config loads runtime configuration: a TOML file overlaid with
// environment variables.
//
// The file is optional — with no file and no environment, Load returns
// working defaults that write into ./data, which is what `make dev` uses.
// Secrets never come from the TOML file; they come from the environment or
// from a key file referenced by the environment.
package config

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// envPrefix is prepended to every environment variable this package reads.
const envPrefix = "RENTAL_BOT_"

// Config is the whole of the process's configuration.
type Config struct {
	Server   Server   `toml:"server"`
	Database Database `toml:"database"`
	Storage  Storage  `toml:"storage"`
	Gmail    Gmail    `toml:"gmail"`
	LLM      LLM      `toml:"llm"`
	Telegram Telegram `toml:"telegram"`
	Jobs     Jobs     `toml:"jobs"`
	Log      Log      `toml:"log"`

	// Secrets is populated from the environment only. Anything in the
	// TOML file under [secrets] is ignored on purpose.
	Secrets Secrets `toml:"-"`
}

// Server holds HTTP listener settings.
type Server struct {
	// Addr is the listen address, e.g. ":8082".
	Addr string `toml:"addr"`
	// BaseURL is the externally reachable origin, used to build the deep
	// links that email replies and Telegram messages carry.
	BaseURL string `toml:"base_url"`
}

// Database points at the SQLite file.
type Database struct {
	Path string `toml:"path"`
	// ReadPoolSize bounds the reader connection pool. Writes serialize on a
	// single connection regardless (see docs/DESIGN.md §2).
	ReadPoolSize int `toml:"read_pool_size"`
}

// Storage holds the on-disk locations for content that lives outside the
// database (docs/DESIGN.md §9.1).
type Storage struct {
	Blobs    string `toml:"blobs"`
	RawEmail string `toml:"raw_email"`
	// Spool holds what could not be delivered when it was raised. A critical
	// alert cannot ride the job queue — the queue is one of the things it
	// reports on (§8.4) — so it goes to disk instead and is drained on the next
	// attempt.
	Spool string `toml:"spool"`
	// MaxUploadBytes caps one document. It is a cap on the request body, so it
	// is enforced before the bytes reach the disk rather than after. §5.3 asks
	// for the same cap on email attachments, and M3 reads this one.
	MaxUploadBytes int64 `toml:"max_upload_bytes"`
}

// Gmail configures email ingestion (docs/DESIGN.md §4).
//
// An empty ClientID disables the whole subsystem: no scheduler entries, no
// webhook, and the intake screen says it is not configured rather than
// pretending to be disconnected. That is the state a fresh clone is in, and it
// has to be a working state.
type Gmail struct {
	// ClientID is the OAuth client. ClientSecret is not here on purpose — it is
	// a secret, and secrets come from the environment.
	ClientID string `toml:"client_id"`
	// Topic is the Pub/Sub topic users.watch publishes to, in Google's full
	// form: projects/<project>/topics/<topic>.
	Topic string `toml:"topic"`
	// AllowedSenders is the addresses whose mail is processed. Everything else
	// is labelled and stored ignored. A public-ish inbox will receive spam;
	// this is the first and cheapest defense (§4.2).
	AllowedSenders []string `toml:"allowed_senders"`
	// ProcessedLabel and IgnoredLabel are applied in Gmail so the mailbox shows
	// what this process did, and so a human can see it from a phone.
	ProcessedLabel string `toml:"processed_label"`
	IgnoredLabel   string `toml:"ignored_label"`
	// PollInterval is the fallback poller's period. This, not the webhook, is
	// what makes ingestion reliable (§4.2 step 6).
	PollInterval Duration `toml:"poll_interval"`
	// WatchRenewInterval is how often users.watch is re-registered. Google
	// expires a watch after 7 days regardless.
	WatchRenewInterval Duration `toml:"watch_renew_interval"`
	// MaxAttachmentBytes caps one attachment. Anything larger is recorded and
	// not downloaded (§4.3).
	MaxAttachmentBytes int64 `toml:"max_attachment_bytes"`

	PubSub PubSub `toml:"pubsub"`
}

// PubSub is what the push endpoint checks an incoming request against.
//
// The push carries an OIDC JWT and nothing else worth trusting. Both values
// have to match or the request is not from the subscription (§4.2 step 2).
type PubSub struct {
	// Audience is the `aud` claim configured on the push subscription. Google
	// defaults it to the push endpoint URL.
	Audience string `toml:"audience"`
	// ServiceAccount is the `email` claim: the account the subscription pushes
	// as.
	ServiceAccount string `toml:"service_account"`
}

// Enabled reports whether email ingestion is configured at all.
func (g Gmail) Enabled() bool { return g.ClientID != "" }

// LLM configures the reading half of ingestion (docs/DESIGN.md §5).
//
// A blank Provider disables all of it: no classify handler, no extract
// handler, no sweep, and forwarded mail that is still archived and still filed
// but never read. That is the state a fresh clone is in and it has to be a
// working one — the same reason a blank gmail.client_id is the off switch
// there.
//
// The provider name is in the file and the API key is in the environment, the
// split every other subsystem here uses.
type LLM struct {
	// Provider is anthropic, openai or google.
	Provider string `toml:"provider"`
	// Model is the provider's own model id.
	Model string `toml:"model"`
	// Timeout bounds one call. A scanned lease is a slow read, and the job
	// queue's own retry is what covers a call that does not come back.
	Timeout Duration `toml:"timeout"`
	// MaxRetries is how many times the SDK retries a 429 or a 5xx before
	// handing the error back. The job queue retries on top of this, so it is
	// deliberately small.
	MaxRetries int `toml:"max_retries"`
	// MaxAttachmentBytes is the largest enclosure that will be sent to a model.
	// Below gmail.max_attachment_bytes on purpose: the bytes are worth keeping
	// at a size that is not worth reading.
	MaxAttachmentBytes int64 `toml:"max_attachment_bytes"`
	// MonthlyTokenBudget trips a breaker and raises a critical alert rather
	// than quietly running up a bill (§5.3). Zero turns the cap off.
	MonthlyTokenBudget int64 `toml:"monthly_token_budget"`
	// AutoApply allows §5.4's one exception to the review gate. Turning it off
	// sends every proposal to a person, which is the conservative setting and
	// not the default one — a receipt that clears all three conditions is the
	// case the whole pipeline was built to make effortless.
	AutoApply bool `toml:"auto_apply"`
	// AutoApplyConfidence is the second of §5.4's three conditions. The design
	// names 0.90 and there is no reason to go lower; going higher is a matter
	// of taste.
	AutoApplyConfidence float64 `toml:"auto_apply_confidence"`
	// SweepInterval is how often messages that arrived but were never read are
	// looked for. This is to the direct enqueue what the Gmail poller is to the
	// webhook: the enqueue makes it fast, this makes it reliable.
	SweepInterval Duration `toml:"sweep_interval"`
}

// Enabled reports whether the reading half of ingestion is configured at all.
func (l LLM) Enabled() bool { return l.Provider != "" }

// Telegram configures the alert channel (docs/DESIGN.md §8).
//
// An empty BotUsername disables the whole subsystem: no sender, no pairing
// loop, no scheduled sweep, and an intake screen that says it is not set up
// rather than pretending nobody has paired. That is the state a fresh clone is
// in, and it has to be a working state.
//
// The username is the off switch rather than the token for the same reason
// gmail.client_id is: a non-secret in the file, the secret in the environment.
// It also earns its place — the screen has to say which bot to send /start to,
// and asking Telegram costs a round trip to learn something the operator
// already knows.
type Telegram struct {
	// BotUsername is the bot's @name, without the @. BotToken is not here on
	// purpose — it is a secret, and secrets come from the environment.
	BotUsername string `toml:"bot_username"`
	// Cooldown is how long a condition stays quiet after it has been reported.
	// §8.3: "an alert channel noisy enough to be muted is worse than no channel
	// at all."
	Cooldown Duration `toml:"cooldown"`
	// CriticalCooldown is the same for critical alerts, which are worth
	// restating sooner because nobody is going to fix a thing they forgot.
	CriticalCooldown Duration `toml:"critical_cooldown"`
	// PairingTTL bounds how long a pairing code is good for. §8.2 says ten
	// minutes and single-use.
	PairingTTL Duration `toml:"pairing_ttl"`
	// PollInterval is the getUpdates long-poll timeout. Telegram holds the
	// request open for it, so this is not a busy loop's period.
	PollInterval Duration `toml:"poll_interval"`
	// SweepInterval is how often the probes run. It bounds how late a condition
	// nobody publishes an event for — a lapsed watch, a backlog — is noticed.
	SweepInterval Duration `toml:"sweep_interval"`
	// QueueBacklogThreshold is how many pending jobs is too many. Zero disables
	// the check rather than alerting on every job.
	QueueBacklogThreshold int `toml:"queue_backlog_threshold"`
	// SilenceAfter is how long a connected mailbox may go unchecked before that
	// is worth saying out loud. §12 lists silent ingestion stoppage as a risk,
	// and this is its mitigation. It measures the poll, not the mail: a quiet
	// week is normal, a poller that stopped running is not.
	SilenceAfter Duration `toml:"silence_after"`
}

// Enabled reports whether the alert channel is configured at all.
func (t Telegram) Enabled() bool { return t.BotUsername != "" }

// Jobs configures the worker pool that drains the jobs table.
type Jobs struct {
	// Workers bounds concurrency. Writes serialize on one SQLite connection
	// (§2), so this is about overlapping network calls, not database
	// throughput.
	Workers int `toml:"workers"`
	// PollInterval is how often an idle worker looks for work. An enqueue wakes
	// the runner immediately, so this only bounds how late a job scheduled for
	// the future starts.
	PollInterval Duration `toml:"poll_interval"`
	// LeaseTimeout is how long a claimed job may stay locked before the runner
	// assumes the worker holding it is gone and returns it to pending.
	LeaseTimeout Duration `toml:"lease_timeout"`
}

// Log configures structured logging.
type Log struct {
	// Level is one of debug, info, warn, error.
	Level string `toml:"level"`
	// Format is text or json.
	Format string `toml:"format"`
}

// Secrets are read from the environment, never from the config file.
type Secrets struct {
	// Key is the AES-GCM key protecting encrypted columns (§9.2). M3 is the
	// milestone that introduces one — the Gmail refresh token — so it is
	// required whenever ingestion is configured, and optional otherwise.
	Key []byte
	// GmailClientSecret pairs with Gmail.ClientID.
	GmailClientSecret string
	// LLMAPIKey pairs with LLM.Provider. The SDK would resolve a provider's
	// own environment variable on its own; this process reads its secrets in
	// one place instead, so a key that arrives by a second route cannot be a
	// key nothing validates at startup.
	LLMAPIKey string
	// TelegramBotToken pairs with Telegram.BotUsername.
	//
	// §9.2 lists the bot token under field encryption, alongside the Gmail
	// refresh token. It is here instead, and no column holds it. The refresh
	// token is encrypted in the database because OAuth produces it at runtime
	// and there is nowhere else for it to live; a bot token is a static
	// credential the operator configures once, which makes it the same kind of
	// thing as the Gmail client secret directly above.
	TelegramBotToken string
}

// Duration is a time.Duration that reads from TOML and from an environment
// variable as "10m" rather than as a count of nanoseconds.
type Duration struct {
	time.Duration
}

// UnmarshalText lets BurntSushi/toml decode a quoted duration string.
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("%q is not a duration like 10m or 24h", text)
	}
	d.Duration = parsed
	return nil
}

// MarshalText keeps a round trip through TOML readable.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.String()), nil
}

// Default returns the configuration used when nothing overrides it.
func Default() Config {
	return Config{
		Server: Server{
			Addr:    ":8082",
			BaseURL: "http://localhost:8082",
		},
		Database: Database{
			Path:         "data/rental.db",
			ReadPoolSize: 4,
		},
		Storage: Storage{
			Blobs:    "data/blobs",
			RawEmail: "data/raw-email",
			Spool:    "data/spool",
			// Comfortably past a scanned lease, well short of anything that
			// would be a memory problem to receive.
			MaxUploadBytes: 25 << 20, // 25 MiB
		},
		Gmail: Gmail{
			ProcessedLabel: "rental-bot/processed",
			IgnoredLabel:   "rental-bot/ignored",
			// Ten minutes is §4.2's figure. It bounds how long ingestion can be
			// stopped without anyone noticing, which is the number that
			// matters — the webhook is what makes it fast.
			PollInterval: Duration{10 * time.Minute},
			// Google expires a watch after 7 days. Daily renewal leaves six
			// days of failures before anything is actually lost.
			WatchRenewInterval: Duration{24 * time.Hour},
			MaxAttachmentBytes: 25 << 20, // 25 MiB, matching one upload
		},
		LLM: LLM{
			// The provider is blank, which is the off switch. Everything below
			// it is what the subsystem uses once somebody names one.
			Model: "claude-sonnet-5",
			// A scanned twelve-page lease is a slow read, and the queue's own
			// retry covers a call that never comes back.
			Timeout:    Duration{90 * time.Second},
			MaxRetries: 2,
			// Under gmail.max_attachment_bytes: bytes worth keeping are not
			// always bytes worth reading, and a 25 MiB scan costs more to send
			// than the record it produces is worth.
			MaxAttachmentBytes: 10 << 20, // 10 MiB
			// About twenty dollars a month at current rates, which is far more
			// than a small portfolio's forwarded mail costs to read. It is a
			// runaway-bill breaker, not a spending target.
			MonthlyTokenBudget:  2_000_000,
			AutoApply:           true,
			AutoApplyConfidence: 0.90, // §5.4's figure
			// Fifteen minutes: longer than the Gmail poll, because this only
			// catches what the enqueue at sync time already missed.
			SweepInterval: Duration{15 * time.Minute},
		},
		Telegram: Telegram{
			// Six hours is §8.3's figure. Long enough that a condition nobody
			// has got to yet does not nag, short enough that a condition
			// nobody has got to in a working day says so again.
			Cooldown: Duration{6 * time.Hour},
			// Critical means the thing is broken now. An hour is the longest
			// that should pass without a reminder.
			CriticalCooldown: Duration{time.Hour},
			PairingTTL:       Duration{10 * time.Minute},
			// Telegram's own maximum for a long poll is 50 seconds.
			PollInterval: Duration{30 * time.Second},
			// Five minutes bounds how late a lapsed watch or a backlog is
			// noticed. The conditions the probes watch move in hours.
			SweepInterval: Duration{5 * time.Minute},
			// Two workers draining a queue that normally holds one or two jobs.
			// Fifty pending means something has stopped draining it.
			QueueBacklogThreshold: 50,
			// Two days without the mailbox being checked at all, against a
			// poller that runs every ten minutes. Anything shorter would fire
			// on a laptop that was asleep over a weekend.
			SilenceAfter: Duration{48 * time.Hour},
		},
		Jobs: Jobs{
			Workers:      2,
			PollInterval: Duration{5 * time.Second},
			// Longer than any job here should take, short enough that a
			// killed process does not strand work for an hour.
			LeaseTimeout: Duration{10 * time.Minute},
		},
		Log: Log{
			Level:  "info",
			Format: "text",
		},
	}
}

// Load reads configuration from path, overlays the environment, and
// validates the result. An empty path, or a path that does not exist, is not
// an error: defaults and environment carry the process.
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		switch _, err := os.Stat(path); {
		case err == nil:
			if _, err := toml.DecodeFile(path, &cfg); err != nil {
				return Config{}, fmt.Errorf("config %s: %w", path, err)
			}
		case errors.Is(err, os.ErrNotExist):
			// Defaults plus environment. Nothing to report.
		default:
			return Config{}, fmt.Errorf("config %s: %w", path, err)
		}
	}

	if err := cfg.overlayEnv(); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// overlayEnv applies RENTAL_BOT_* variables on top of whatever the file set.
//
// It reads as a table of every variable this process understands and the field
// it writes, which is the useful thing to be able to scan. The loader collects
// failures rather than returning at the first, so an operator who mistyped two
// durations is told about both instead of finding the second one on the next
// restart.
func (c *Config) overlayEnv() error {
	var env envLoader

	env.str("SERVER_ADDR", &c.Server.Addr)
	env.str("SERVER_BASE_URL", &c.Server.BaseURL)

	env.str("DATABASE_PATH", &c.Database.Path)
	env.integer("DATABASE_READ_POOL_SIZE", &c.Database.ReadPoolSize)

	env.str("STORAGE_BLOBS", &c.Storage.Blobs)
	env.str("STORAGE_RAW_EMAIL", &c.Storage.RawEmail)
	env.str("STORAGE_SPOOL", &c.Storage.Spool)
	env.bytes("STORAGE_MAX_UPLOAD_BYTES", &c.Storage.MaxUploadBytes)

	env.str("GMAIL_CLIENT_ID", &c.Gmail.ClientID)
	env.str("GMAIL_TOPIC", &c.Gmail.Topic)
	env.list("GMAIL_ALLOWED_SENDERS", &c.Gmail.AllowedSenders)
	env.str("GMAIL_PROCESSED_LABEL", &c.Gmail.ProcessedLabel)
	env.str("GMAIL_IGNORED_LABEL", &c.Gmail.IgnoredLabel)
	env.duration("GMAIL_POLL_INTERVAL", &c.Gmail.PollInterval)
	env.duration("GMAIL_WATCH_RENEW_INTERVAL", &c.Gmail.WatchRenewInterval)
	env.bytes("GMAIL_MAX_ATTACHMENT_BYTES", &c.Gmail.MaxAttachmentBytes)
	env.str("GMAIL_PUBSUB_AUDIENCE", &c.Gmail.PubSub.Audience)
	env.str("GMAIL_PUBSUB_SERVICE_ACCOUNT", &c.Gmail.PubSub.ServiceAccount)

	env.str("LLM_PROVIDER", &c.LLM.Provider)
	env.str("LLM_MODEL", &c.LLM.Model)
	env.duration("LLM_TIMEOUT", &c.LLM.Timeout)
	env.integer("LLM_MAX_RETRIES", &c.LLM.MaxRetries)
	env.bytes("LLM_MAX_ATTACHMENT_BYTES", &c.LLM.MaxAttachmentBytes)
	env.count("LLM_MONTHLY_TOKEN_BUDGET", &c.LLM.MonthlyTokenBudget)
	env.boolean("LLM_AUTO_APPLY", &c.LLM.AutoApply)
	env.fraction("LLM_AUTO_APPLY_CONFIDENCE", &c.LLM.AutoApplyConfidence)
	env.duration("LLM_SWEEP_INTERVAL", &c.LLM.SweepInterval)

	env.str("TELEGRAM_BOT_USERNAME", &c.Telegram.BotUsername)
	env.duration("TELEGRAM_COOLDOWN", &c.Telegram.Cooldown)
	env.duration("TELEGRAM_CRITICAL_COOLDOWN", &c.Telegram.CriticalCooldown)
	env.duration("TELEGRAM_PAIRING_TTL", &c.Telegram.PairingTTL)
	env.duration("TELEGRAM_POLL_INTERVAL", &c.Telegram.PollInterval)
	env.duration("TELEGRAM_SWEEP_INTERVAL", &c.Telegram.SweepInterval)
	env.integer("TELEGRAM_QUEUE_BACKLOG_THRESHOLD", &c.Telegram.QueueBacklogThreshold)
	env.duration("TELEGRAM_SILENCE_AFTER", &c.Telegram.SilenceAfter)

	env.integer("JOBS_WORKERS", &c.Jobs.Workers)
	env.duration("JOBS_POLL_INTERVAL", &c.Jobs.PollInterval)
	env.duration("JOBS_LEASE_TIMEOUT", &c.Jobs.LeaseTimeout)

	env.str("LOG_LEVEL", &c.Log.Level)
	env.str("LOG_FORMAT", &c.Log.Format)

	if err := env.err(); err != nil {
		return err
	}
	return c.loadSecrets()
}

// loadSecrets reads the encryption key from the environment or a key file.
func (c *Config) loadSecrets() error {
	var env envLoader
	env.str("GMAIL_CLIENT_SECRET", &c.Secrets.GmailClientSecret)
	env.str("LLM_API_KEY", &c.Secrets.LLMAPIKey)
	env.str("TELEGRAM_BOT_TOKEN", &c.Secrets.TelegramBotToken)

	if v, ok := os.LookupEnv(envPrefix + "SECRET_KEY"); ok && v != "" {
		c.Secrets.Key = []byte(v)
		return nil
	}

	path, ok := os.LookupEnv(envPrefix + "SECRET_KEY_FILE")
	if !ok || path == "" {
		return nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("secret key file %s: %w", path, err)
	}
	// The key never belongs to the database and never belongs to a group.
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("secret key file %s has mode %#o; it must be 0600", path, perm)
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("secret key file %s: %w", path, err)
	}
	c.Secrets.Key = []byte(strings.TrimSpace(string(key)))
	return nil
}

// Validate reports the first configuration value that cannot work.
func (c Config) Validate() error {
	if c.Server.Addr == "" {
		return errors.New("server.addr is empty")
	}
	if c.Database.Path == "" {
		return errors.New("database.path is empty")
	}
	if c.Database.ReadPoolSize < 1 {
		return fmt.Errorf("database.read_pool_size is %d; it must be at least 1", c.Database.ReadPoolSize)
	}
	if c.Storage.MaxUploadBytes < 1 {
		return fmt.Errorf("storage.max_upload_bytes is %d; it must be at least 1", c.Storage.MaxUploadBytes)
	}
	if _, err := ParseLevel(c.Log.Level); err != nil {
		return err
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		return fmt.Errorf("log.format is %q; it must be text or json", c.Log.Format)
	}
	if err := c.validateJobs(); err != nil {
		return err
	}
	if err := c.validateGmail(); err != nil {
		return err
	}
	if err := c.validateLLM(); err != nil {
		return err
	}
	return c.validateTelegram()
}

// validateLLM checks the reading settings, and only when there are any.
//
// A blank llm.provider is the off switch, the same way a blank
// gmail.client_id is. Name one, though, and everything it needs has to be
// there: half-configured reading fails at the first forwarded receipt, which
// is days later and looks like a pipeline that silently stopped.
func (c Config) validateLLM() error {
	if !c.LLM.Enabled() {
		return nil
	}
	switch c.LLM.Provider {
	case "anthropic", "openai", "google":
	default:
		return fmt.Errorf("llm.provider is %q; it must be anthropic, openai or google", c.LLM.Provider)
	}
	if c.Secrets.LLMAPIKey == "" {
		return errors.New("llm.provider is set but RENTAL_BOT_LLM_API_KEY is empty")
	}
	if c.LLM.Model == "" {
		return errors.New("llm.provider is set but llm.model is empty")
	}
	// Reading forwarded mail without a mailbox to read it from is a subsystem
	// with nothing to do. Saying so at startup beats an empty review queue
	// nobody can explain.
	if !c.Gmail.Enabled() {
		return errors.New("llm.provider is set but gmail.client_id is empty; there is no mail to read")
	}
	if c.LLM.Timeout.Duration < time.Second {
		return fmt.Errorf("llm.timeout is %s; it must be at least 1s", c.LLM.Timeout)
	}
	if c.LLM.MaxRetries < 0 {
		return fmt.Errorf("llm.max_retries is %d; it must not be negative", c.LLM.MaxRetries)
	}
	if c.LLM.MaxAttachmentBytes < 1 {
		return fmt.Errorf("llm.max_attachment_bytes is %d; it must be at least 1", c.LLM.MaxAttachmentBytes)
	}
	if c.LLM.MonthlyTokenBudget < 0 {
		return fmt.Errorf("llm.monthly_token_budget is %d; it must not be negative, and 0 turns the cap off", c.LLM.MonthlyTokenBudget)
	}
	// A confidence threshold outside 0..1 compares against a number the model
	// cannot produce, which either applies everything or applies nothing.
	if c.LLM.AutoApplyConfidence < 0 || c.LLM.AutoApplyConfidence > 1 {
		return fmt.Errorf("llm.auto_apply_confidence is %v; it must be between 0 and 1", c.LLM.AutoApplyConfidence)
	}
	if c.LLM.SweepInterval.Duration < time.Minute {
		return fmt.Errorf("llm.sweep_interval is %s; it must be at least 1m", c.LLM.SweepInterval)
	}
	return nil
}

// validateGmail checks the ingestion settings, and only when there are any.
//
// A blank gmail.client_id is the off switch, not an oversight: a fresh clone
// runs the web application without a Google project, and every later stage
// checks Enabled before doing anything. Set it, though, and everything it needs
// has to be there — half-configured ingestion fails at the first forwarded
// email, days later, instead of at startup.
func (c Config) validateGmail() error {
	if !c.Gmail.Enabled() {
		return nil
	}
	if c.Secrets.GmailClientSecret == "" {
		return errors.New("gmail.client_id is set but RENTAL_BOT_GMAIL_CLIENT_SECRET is empty")
	}
	if c.Gmail.Topic == "" {
		return errors.New("gmail.client_id is set but gmail.topic is empty")
	}
	if len(c.Gmail.AllowedSenders) == 0 {
		return errors.New("gmail.allowed_senders is empty; ingestion would process every message that arrives")
	}
	// The Gmail refresh token is stored encrypted, so this is the milestone
	// that makes the key required (§9.2).
	if len(c.Secrets.Key) == 0 {
		return errors.New("gmail.client_id is set but no encryption key is configured; set RENTAL_BOT_SECRET_KEY or RENTAL_BOT_SECRET_KEY_FILE")
	}
	if c.Gmail.PollInterval.Duration < time.Minute {
		return fmt.Errorf("gmail.poll_interval is %s; it must be at least 1m", c.Gmail.PollInterval)
	}
	if c.Gmail.WatchRenewInterval.Duration < time.Minute {
		return fmt.Errorf("gmail.watch_renew_interval is %s; it must be at least 1m", c.Gmail.WatchRenewInterval)
	}
	// A watch Google expires after 7 days has to be renewed inside that window.
	if c.Gmail.WatchRenewInterval.Duration > 6*24*time.Hour {
		return fmt.Errorf("gmail.watch_renew_interval is %s; Google expires a watch after 7 days, so it must be under 144h", c.Gmail.WatchRenewInterval)
	}
	if c.Gmail.MaxAttachmentBytes < 1 {
		return fmt.Errorf("gmail.max_attachment_bytes is %d; it must be at least 1", c.Gmail.MaxAttachmentBytes)
	}
	return nil
}

// validateTelegram checks the alert channel, and only when there is one.
//
// A blank telegram.bot_username is the off switch, the same way a blank
// gmail.client_id is. Set it, though, and everything it needs has to be there —
// half-configured alerting fails at the first outage, which is the worst
// possible moment to find out the channel was never going to work.
func (c Config) validateTelegram() error {
	if !c.Telegram.Enabled() {
		return nil
	}
	if c.Secrets.TelegramBotToken == "" {
		return errors.New("telegram.bot_username is set but RENTAL_BOT_TELEGRAM_BOT_TOKEN is empty")
	}
	// A bare @ is a common paste; it is not part of the name.
	if strings.HasPrefix(c.Telegram.BotUsername, "@") {
		return fmt.Errorf("telegram.bot_username is %q; drop the leading @", c.Telegram.BotUsername)
	}
	if c.Telegram.Cooldown.Duration < time.Minute {
		return fmt.Errorf("telegram.cooldown is %s; it must be at least 1m", c.Telegram.Cooldown)
	}
	if c.Telegram.CriticalCooldown.Duration < time.Minute {
		return fmt.Errorf("telegram.critical_cooldown is %s; it must be at least 1m", c.Telegram.CriticalCooldown)
	}
	// A critical condition restated less often than an ordinary one inverts the
	// severities, which is the sort of mistake that only shows up in an outage.
	if c.Telegram.CriticalCooldown.Duration > c.Telegram.Cooldown.Duration {
		return fmt.Errorf("telegram.critical_cooldown is %s, longer than telegram.cooldown %s; a critical condition would be restated less often than an ordinary one",
			c.Telegram.CriticalCooldown, c.Telegram.Cooldown)
	}
	if c.Telegram.PairingTTL.Duration < time.Minute {
		return fmt.Errorf("telegram.pairing_ttl is %s; it must be at least 1m", c.Telegram.PairingTTL)
	}
	// Telegram closes a getUpdates long poll at 50 seconds regardless.
	if c.Telegram.PollInterval.Duration < time.Second || c.Telegram.PollInterval.Duration > 50*time.Second {
		return fmt.Errorf("telegram.poll_interval is %s; it must be between 1s and 50s", c.Telegram.PollInterval)
	}
	if c.Telegram.SweepInterval.Duration < time.Minute {
		return fmt.Errorf("telegram.sweep_interval is %s; it must be at least 1m", c.Telegram.SweepInterval)
	}
	if c.Telegram.QueueBacklogThreshold < 0 {
		return fmt.Errorf("telegram.queue_backlog_threshold is %d; it must not be negative", c.Telegram.QueueBacklogThreshold)
	}
	if c.Telegram.SilenceAfter.Duration < time.Minute {
		return fmt.Errorf("telegram.silence_after is %s; it must be at least 1m", c.Telegram.SilenceAfter)
	}
	if c.Storage.Spool == "" {
		return errors.New("telegram.bot_username is set but storage.spool is empty; a critical alert has nowhere to wait when Telegram is unreachable")
	}
	return nil
}

func (c Config) validateJobs() error {
	if c.Jobs.Workers < 1 {
		return fmt.Errorf("jobs.workers is %d; it must be at least 1", c.Jobs.Workers)
	}
	if c.Jobs.PollInterval.Duration < time.Second {
		return fmt.Errorf("jobs.poll_interval is %s; it must be at least 1s", c.Jobs.PollInterval)
	}
	if c.Jobs.LeaseTimeout.Duration < c.Jobs.PollInterval.Duration {
		return fmt.Errorf("jobs.lease_timeout is %s, shorter than jobs.poll_interval %s; a job would be reclaimed before the worker holding it looked up",
			c.Jobs.LeaseTimeout, c.Jobs.PollInterval)
	}
	return nil
}

// Dirs lists the directories the process expects to be able to write.
func (c Config) Dirs() []string {
	return []string{
		filepath.Dir(c.Database.Path),
		c.Storage.Blobs,
		c.Storage.RawEmail,
		c.Storage.Spool,
	}
}

// ParseLevel maps a configured level name onto a slog level.
func ParseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(name) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("log.level is %q; it must be debug, info, warn, or error", name)
	}
}

// Logger builds the process logger described by the log settings.
func (l Log) Logger(w io.Writer) *slog.Logger {
	level, err := ParseLevel(l.Level)
	if err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}

	var h slog.Handler
	if l.Format == "json" {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h)
}

// envLoader reads RENTAL_BOT_* variables into fields, collecting what it could
// not read rather than stopping at the first.
//
// A variable that is not set leaves its field alone, so the layering is file,
// then environment, and an unset variable is not the same as an empty one.
type envLoader struct{ errs []error }

// err reports everything that failed, or nil.
func (e *envLoader) err() error { return errors.Join(e.errs...) }

func (e *envLoader) str(suffix string, dst *string) {
	if v, ok := os.LookupEnv(envPrefix + suffix); ok {
		*dst = v
	}
}

// list reads a comma-separated variable, trimming and dropping blanks, so
// RENTAL_BOT_GMAIL_ALLOWED_SENDERS="a@x.com, b@x.com" works the way anyone
// would expect it to.
func (e *envLoader) list(suffix string, dst *[]string) {
	v, ok := os.LookupEnv(envPrefix + suffix)
	if !ok {
		return
	}
	out := []string{}
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	*dst = out
}

func (e *envLoader) integer(suffix string, dst *int) {
	v, ok := os.LookupEnv(envPrefix + suffix)
	if !ok {
		return
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s%s is %q; it must be an integer", envPrefix, suffix, v))
		return
	}
	*dst = n
}

// bytes reads a size in bytes. It is separate from integer only because the
// fields it writes are int64.
func (e *envLoader) bytes(suffix string, dst *int64) { e.count(suffix, dst) }

// count reads any int64 quantity. bytes is the same reader under the name the
// size fields read better with.
func (e *envLoader) count(suffix string, dst *int64) {
	v, ok := os.LookupEnv(envPrefix + suffix)
	if !ok {
		return
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s%s is %q; it must be an integer", envPrefix, suffix, v))
		return
	}
	*dst = n
}

// boolean reads a switch. strconv's spellings are accepted -- true, false, 1,
// 0, yes and no are not all of them, but every one of them is unambiguous.
func (e *envLoader) boolean(suffix string, dst *bool) {
	v, ok := os.LookupEnv(envPrefix + suffix)
	if !ok {
		return
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s%s is %q; it must be true or false", envPrefix, suffix, v))
		return
	}
	*dst = b
}

// fraction reads a number between zero and one. The range is checked here
// rather than in Validate, because "0.90" and "90" are both things somebody
// will type and only one of them means what they meant.
func (e *envLoader) fraction(suffix string, dst *float64) {
	v, ok := os.LookupEnv(envPrefix + suffix)
	if !ok {
		return
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 || f > 1 {
		e.errs = append(e.errs, fmt.Errorf("%s%s is %q; it must be a number between 0 and 1", envPrefix, suffix, v))
		return
	}
	*dst = f
}

func (e *envLoader) duration(suffix string, dst *Duration) {
	v, ok := os.LookupEnv(envPrefix + suffix)
	if !ok {
		return
	}
	if err := dst.UnmarshalText([]byte(v)); err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s%s: %w", envPrefix, suffix, err))
	}
}
