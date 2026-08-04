package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Server.Addr, Default().Server.Addr; got != want {
		t.Errorf("Server.Addr = %q, want %q", got, want)
	}
}

func TestLoadFileThenEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	write(t, path, `
[server]
addr = ":9000"
base_url = "https://rental.example.com"

[database]
path = "/srv/rental.db"
read_pool_size = 8

[log]
level = "debug"
format = "json"
`)

	// The environment wins over the file; the file wins over defaults.
	t.Setenv(envPrefix+"SERVER_ADDR", ":7777")
	t.Setenv(envPrefix+"DATABASE_READ_POOL_SIZE", "2")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	tests := []struct {
		name      string
		got, want any
	}{
		{"Server.Addr (env)", cfg.Server.Addr, ":7777"},
		{"Server.BaseURL (file)", cfg.Server.BaseURL, "https://rental.example.com"},
		{"Database.Path (file)", cfg.Database.Path, "/srv/rental.db"},
		{"Database.ReadPoolSize (env)", cfg.Database.ReadPoolSize, 2},
		{"Log.Level (file)", cfg.Log.Level, "debug"},
		{"Storage.Blobs (default)", cfg.Storage.Blobs, Default().Storage.Blobs},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
		}
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{"unknown log level", map[string]string{"LOG_LEVEL": "chatty"}},
		{"unknown log format", map[string]string{"LOG_FORMAT": "yaml"}},
		{"empty listen address", map[string]string{"SERVER_ADDR": ""}},
		{"zero read pool", map[string]string{"DATABASE_READ_POOL_SIZE": "0"}},
		{"non-numeric read pool", map[string]string{"DATABASE_READ_POOL_SIZE": "four"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(envPrefix+k, v)
			}
			if _, err := Load(""); err == nil {
				t.Fatal("Load succeeded, want an error")
			}
		})
	}
}

func TestLoadSecretKeyFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("mode 0600 is read", func(t *testing.T) {
		path := filepath.Join(dir, "ok.key")
		write(t, path, "hunter2\n")
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(envPrefix+"SECRET_KEY_FILE", path)

		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := string(cfg.Secrets.Key); got != "hunter2" {
			t.Errorf("Secrets.Key = %q, want %q", got, "hunter2")
		}
	})

	t.Run("group-readable is refused", func(t *testing.T) {
		path := filepath.Join(dir, "loose.key")
		write(t, path, "hunter2")
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
		t.Setenv(envPrefix+"SECRET_KEY_FILE", path)

		if _, err := Load(""); err == nil {
			t.Fatal("Load succeeded on a group-readable key file, want an error")
		}
	})

	t.Run("environment beats key file", func(t *testing.T) {
		t.Setenv(envPrefix+"SECRET_KEY_FILE", filepath.Join(dir, "absent.key"))
		t.Setenv(envPrefix+"SECRET_KEY", "from-env")

		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := string(cfg.Secrets.Key); got != "from-env" {
			t.Errorf("Secrets.Key = %q, want %q", got, "from-env")
		}
	})
}

func TestSecretsAreNotReadFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	write(t, path, "[secrets]\nkey = \"do-not-use-me\"\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Secrets.Key) != 0 {
		t.Errorf("Secrets.Key = %q, want empty: secrets must not come from the config file", cfg.Secrets.Key)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
