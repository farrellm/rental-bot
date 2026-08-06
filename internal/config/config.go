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

	"github.com/BurntSushi/toml"
)

// envPrefix is prepended to every environment variable this package reads.
const envPrefix = "RENTAL_BOT_"

// Config is the whole of the process's configuration.
type Config struct {
	Server   Server   `toml:"server"`
	Database Database `toml:"database"`
	Storage  Storage  `toml:"storage"`
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

// Log configures structured logging.
type Log struct {
	// Level is one of debug, info, warn, error.
	Level string `toml:"level"`
	// Format is text or json.
	Format string `toml:"format"`
}

// Secrets are read from the environment, never from the config file.
type Secrets struct {
	// Key is the AES-GCM key protecting encrypted columns (§9.2). It is
	// optional at M0 because nothing is encrypted yet; the milestone that
	// introduces an encrypted column makes it required.
	Key []byte
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
	envString("LOG_LEVEL", &c.Log.Level)
	envString("LOG_FORMAT", &c.Log.Format)

	return c.loadSecrets()
}

// loadSecrets reads the encryption key from the environment or a key file.
func (c *Config) loadSecrets() error {
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
