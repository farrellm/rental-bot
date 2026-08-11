package store

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/farrellm/rental-bot/internal/domain"
)

// Migration is one applied migration, as recorded in schema_migrations.
type Migration struct {
	Version   int    `json:"version"`
	Name      string `json:"name"`
	Checksum  string `json:"checksum"`
	AppliedAt string `json:"applied_at"`
}

// Filename reconstructs the file this migration came from.
func (m Migration) Filename() string { return migrationFilename(m.Version, m.Name) }

// ErrChecksumMismatch reports a migration that changed after it was applied.
var ErrChecksumMismatch = errors.New("store: applied migration has changed on disk")

// migrationName matches NNNN_description.sql.
var migrationName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

const createSchemaMigrations = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    checksum   TEXT NOT NULL,
    applied_at TEXT NOT NULL
) STRICT;`

// Migrate applies every migration in fsys that has not run yet, in version
// order, each inside its own transaction. It returns the migrations it
// applied — empty on an already-current database.
//
// Before applying anything it verifies that the migrations already recorded
// still hash to what was recorded. Migrations are append-only; one that has
// run on a real database is history, and editing it means the schema on disk
// and the schema in the repository have silently diverged. That is worth
// refusing to start over.
func (db *DB) Migrate(ctx context.Context, fsys fs.FS) ([]Migration, error) {
	files, err := loadMigrations(fsys)
	if err != nil {
		return nil, err
	}

	if _, err := db.writer.ExecContext(ctx, createSchemaMigrations); err != nil {
		return nil, fmt.Errorf("store: create schema_migrations: %w", err)
	}

	applied, err := appliedByVersion(ctx, db.writer)
	if err != nil {
		return nil, err
	}

	var ran []Migration
	for _, f := range files {
		if prev, ok := applied[f.Version]; ok {
			if prev.Checksum != f.Checksum {
				return nil, fmt.Errorf("%w: %s was applied as %s but is now %s",
					ErrChecksumMismatch, f.Filename(), short(prev.Checksum), short(f.Checksum))
			}
			continue
		}
		m, err := db.apply(ctx, f)
		if err != nil {
			return ran, err
		}
		ran = append(ran, m)
	}
	return ran, nil
}

// apply runs one migration and records it, atomically.
func (db *DB) apply(ctx context.Context, f migrationFile) (Migration, error) {
	tx, err := db.writer.BeginTx(ctx, nil)
	if err != nil {
		return Migration{}, fmt.Errorf("store: %s: %w", f.Filename(), err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	if _, err := tx.ExecContext(ctx, f.SQL); err != nil {
		return Migration{}, fmt.Errorf("store: %s: %w", f.Filename(), err)
	}

	m := Migration{
		Version:   f.Version,
		Name:      f.Name,
		Checksum:  f.Checksum,
		AppliedAt: domain.Stamp(time.Now()),
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
		m.Version, m.Name, m.Checksum, m.AppliedAt)
	if err != nil {
		return Migration{}, fmt.Errorf("store: record %s: %w", f.Filename(), err)
	}
	if err := tx.Commit(); err != nil {
		return Migration{}, fmt.Errorf("store: commit %s: %w", f.Filename(), err)
	}
	return m, nil
}

// Applied lists the migrations recorded in the database, oldest first. A
// database with no schema_migrations table yet reports none.
func (db *DB) Applied(ctx context.Context) ([]Migration, error) {
	rows, err := db.reader.QueryContext(ctx,
		`SELECT version, name, checksum, applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		if isMissingTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	return scanMigrations(rows)
}

// SchemaVersion reports the highest applied migration version, or 0 if the
// database has never been migrated.
func (db *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	err := db.reader.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	switch {
	case err == nil:
		return int(v.Int64), nil
	case errors.Is(err, sql.ErrNoRows), isMissingTable(err):
		return 0, nil
	default:
		return 0, fmt.Errorf("store: read schema version: %w", err)
	}
}

// migrationFile is one migration read off the embedded filesystem.
type migrationFile struct {
	Version  int
	Name     string
	Checksum string
	SQL      string
}

// Filename reconstructs the file's name for error messages.
func (f migrationFile) Filename() string { return migrationFilename(f.Version, f.Name) }

// migrationFilename is the NNNN_name.sql spelling, which is both how a
// migration is found on disk and how it is named in an error.
func migrationFilename(version int, name string) string {
	return fmt.Sprintf("%04d_%s.sql", version, name)
}

// loadMigrations reads and validates every *.sql in fsys, in version order.
func loadMigrations(fsys fs.FS) ([]migrationFile, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("store: read migrations: %w", err)
	}

	out := make([]migrationFile, 0, len(names))
	seen := make(map[int]string, len(names))
	for _, name := range names {
		m := migrationName.FindStringSubmatch(name)
		if m == nil {
			return nil, fmt.Errorf("store: migration %q is not named NNNN_description.sql", name)
		}
		version, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("store: migration %q: %w", name, err)
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("store: migrations %s and %s share version %d", prev, name, version)
		}
		seen[version] = name

		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("store: read %s: %w", name, err)
		}
		sum := sha256.Sum256(body)
		out = append(out, migrationFile{
			Version:  version,
			Name:     m[2],
			Checksum: hex.EncodeToString(sum[:]),
			SQL:      string(body),
		})
	}
	slices.SortFunc(out, func(a, b migrationFile) int { return cmp.Compare(a.Version, b.Version) })
	return out, nil
}

// appliedByVersion indexes the recorded migrations for lookup.
func appliedByVersion(ctx context.Context, db *sql.DB) (map[int]Migration, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT version, name, checksum, applied_at FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	recorded, err := scanMigrations(rows)
	if err != nil {
		return nil, err
	}

	out := make(map[int]Migration, len(recorded))
	for _, m := range recorded {
		out[m.Version] = m
	}
	return out, nil
}

// scanMigrations reads a schema_migrations result set, closing it.
func scanMigrations(rows *sql.Rows) ([]Migration, error) {
	defer rows.Close()

	var out []Migration
	for rows.Next() {
		var m Migration
		if err := rows.Scan(&m.Version, &m.Name, &m.Checksum, &m.AppliedAt); err != nil {
			return nil, fmt.Errorf("store: read schema_migrations: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	return out, nil
}

// isMissingTable reports whether err is SQLite complaining about a table that
// does not exist, which is what a never-migrated database looks like.
func isMissingTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

func short(checksum string) string {
	if len(checksum) > 12 {
		return checksum[:12]
	}
	return checksum
}
