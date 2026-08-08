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
	Jobs     Jobs     `toml:"jobs"`
	Log      Log      `toml:"log"`

	// Secrets is populated from the environment only. Anything in the
	// TOML file under [secrets] is ignored on purpose.
	Secrets Secrets `toml:"-"`
}

// Server holds HTTP listener settings.
type Server struct {
	// Addr is the listen address, e.g. ":8080".
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
			Addr:    ":8080",
			BaseURL: "http://localhost:8080",
		},
		Database: Database{
			Path:         "data/rental.db",
			ReadPoolSize: 4,
		},
		Storage: Storage{
			Blobs:    "data/blobs",
			RawEmail: "data/raw-email",
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
func (c *Config) overlayEnv() error {
	envString("SERVER_ADDR", &c.Server.Addr)
	envString("SERVER_BASE_URL", &c.Server.BaseURL)
	envString("DATABASE_PATH", &c.Database.Path)
	if err := envInt("DATABASE_READ_POOL_SIZE", &c.Database.ReadPoolSize); err != nil {
		return err
	}
	envString("STORAGE_BLOBS", &c.Storage.Blobs)
	envString("STORAGE_RAW_EMAIL", &c.Storage.RawEmail)
	if err := envInt64("STORAGE_MAX_UPLOAD_BYTES", &c.Storage.MaxUploadBytes); err != nil {
		return err
	}
	envString("GMAIL_CLIENT_ID", &c.Gmail.ClientID)
	envString("GMAIL_TOPIC", &c.Gmail.Topic)
	envList("GMAIL_ALLOWED_SENDERS", &c.Gmail.AllowedSenders)
	envString("GMAIL_PROCESSED_LABEL", &c.Gmail.ProcessedLabel)
	envString("GMAIL_IGNORED_LABEL", &c.Gmail.IgnoredLabel)
	if err := envDuration("GMAIL_POLL_INTERVAL", &c.Gmail.PollInterval); err != nil {
		return err
	}
	if err := envDuration("GMAIL_WATCH_RENEW_INTERVAL", &c.Gmail.WatchRenewInterval); err != nil {
		return err
	}
	if err := envInt64("GMAIL_MAX_ATTACHMENT_BYTES", &c.Gmail.MaxAttachmentBytes); err != nil {
		return err
	}
	envString("GMAIL_PUBSUB_AUDIENCE", &c.Gmail.PubSub.Audience)
	envString("GMAIL_PUBSUB_SERVICE_ACCOUNT", &c.Gmail.PubSub.ServiceAccount)

	if err := envInt("JOBS_WORKERS", &c.Jobs.Workers); err != nil {
		return err
	}
	if err := envDuration("JOBS_POLL_INTERVAL", &c.Jobs.PollInterval); err != nil {
		return err
	}
	if err := envDuration("JOBS_LEASE_TIMEOUT", &c.Jobs.LeaseTimeout); err != nil {
		return err
	}

	envString("LOG_LEVEL", &c.Log.Level)
	envString("LOG_FORMAT", &c.Log.Format)

	return c.loadSecrets()
}

// loadSecrets reads the encryption key from the environment or a key file.
func (c *Config) loadSecrets() error {
	envString("GMAIL_CLIENT_SECRET", &c.Secrets.GmailClientSecret)

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
	return c.validateGmail()
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

func envString(suffix string, dst *string) {
	if v, ok := os.LookupEnv(envPrefix + suffix); ok {
		*dst = v
	}
}

func envInt(suffix string, dst *int) error {
	v, ok := os.LookupEnv(envPrefix + suffix)
	if !ok {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("%s%s is %q; it must be an integer", envPrefix, suffix, v)
	}
	*dst = n
	return nil
}

// envList reads a comma-separated variable, trimming and dropping blanks, so
// RENTAL_BOT_GMAIL_ALLOWED_SENDERS="a@x.com, b@x.com" works the way anyone
// would expect it to.
func envList(suffix string, dst *[]string) {
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

func envDuration(suffix string, dst *Duration) error {
	v, ok := os.LookupEnv(envPrefix + suffix)
	if !ok {
		return nil
	}
	if err := dst.UnmarshalText([]byte(v)); err != nil {
		return fmt.Errorf("%s%s: %w", envPrefix, suffix, err)
	}
	return nil
}

func envInt64(suffix string, dst *int64) error {
	v, ok := os.LookupEnv(envPrefix + suffix)
	if !ok {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fmt.Errorf("%s%s is %q; it must be an integer", envPrefix, suffix, v)
	}
	*dst = n
	return nil
}
