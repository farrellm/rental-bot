package store

import (
	"errors"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/farrellm/rental-bot/migrations"
)

func TestMigrateAppliesRealSchema(t *testing.T) {
	ctx := t.Context()
	db := openTemp(t)

	ran, err := db.Migrate(ctx, migrations.FS)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(ran) == 0 {
		t.Fatal("Migrate applied nothing on a fresh database")
	}

	version, err := db.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != ran[len(ran)-1].Version {
		t.Errorf("SchemaVersion = %d, want %d", version, ran[len(ran)-1].Version)
	}

	// Every table 0001 promises should be queryable.
	for _, table := range []string{"kv", "users", "sessions", "properties", "units", "jobs"} {
		if _, err := db.Reader().ExecContext(ctx, "SELECT 1 FROM "+table+" LIMIT 1"); err != nil {
			t.Errorf("table %s: %v", table, err)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := t.Context()
	db := openTemp(t)

	first, err := db.Migrate(ctx, migrations.FS)
	if err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	second, err := db.Migrate(ctx, migrations.FS)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second Migrate applied %d migrations, want 0", len(second))
	}

	applied, err := db.Applied(ctx)
	if err != nil {
		t.Fatalf("Applied: %v", err)
	}
	if len(applied) != len(first) {
		t.Errorf("Applied lists %d migrations, want %d", len(applied), len(first))
	}
	if len(applied) > 0 && applied[0].AppliedAt == "" {
		t.Error("applied_at is empty")
	}
}

func TestMigrateRefusesEditedMigration(t *testing.T) {
	ctx := t.Context()
	db := openTemp(t)

	original := fstest.MapFS{
		"0001_init.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY) STRICT;`)},
	}
	if _, err := db.Migrate(ctx, original); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Same version, different contents: the file was edited after it ran.
	edited := fstest.MapFS{
		"0001_init.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY, extra TEXT) STRICT;`)},
	}
	_, err := db.Migrate(ctx, edited)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("Migrate on an edited migration returned %v, want ErrChecksumMismatch", err)
	}
}

func TestMigrateAppliesNewVersionsOnly(t *testing.T) {
	ctx := t.Context()
	db := openTemp(t)

	first := fstest.MapFS{
		"0001_init.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY) STRICT;`)},
	}
	if _, err := db.Migrate(ctx, first); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// A correction lands as a new file, never as an edit to the old one.
	second := fstest.MapFS{
		"0001_init.sql":      first["0001_init.sql"],
		"0002_add_table.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE b (id INTEGER PRIMARY KEY) STRICT;`)},
	}
	ran, err := db.Migrate(ctx, second)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(ran) != 1 || ran[0].Version != 2 {
		t.Fatalf("Migrate applied %v, want only version 2", ran)
	}
	if got, want := ran[0].Filename(), "0002_add_table.sql"; got != want {
		t.Errorf("Filename = %q, want %q", got, want)
	}
}

func TestMigrateRollsBackFailedMigration(t *testing.T) {
	ctx := t.Context()
	db := openTemp(t)

	broken := fstest.MapFS{
		"0001_init.sql": &fstest.MapFile{Data: []byte(
			"CREATE TABLE a (id INTEGER PRIMARY KEY) STRICT;\nCREATE TABLE a (id INTEGER PRIMARY KEY) STRICT;")},
	}
	if _, err := db.Migrate(ctx, broken); err == nil {
		t.Fatal("Migrate on invalid SQL succeeded, want an error")
	}

	version, err := db.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != 0 {
		t.Errorf("SchemaVersion = %d after a failed migration, want 0", version)
	}
	if _, err := db.Reader().ExecContext(ctx, "SELECT 1 FROM a"); err == nil {
		t.Error("table a survived a rolled-back migration")
	}
}

func TestMigrateRejectsBadFilenames(t *testing.T) {
	ctx := t.Context()
	tests := map[string]fstest.MapFS{
		"unnumbered": {
			"init.sql": &fstest.MapFile{Data: []byte(`SELECT 1;`)},
		},
		"duplicate version": {
			"0001_a.sql": &fstest.MapFile{Data: []byte(`SELECT 1;`)},
			"0001_b.sql": &fstest.MapFile{Data: []byte(`SELECT 1;`)},
		},
	}
	for name, fsys := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := openTemp(t).Migrate(ctx, fsys); err == nil {
				t.Fatal("Migrate succeeded, want an error")
			}
		})
	}
}

func TestUnmigratedDatabaseReportsVersionZero(t *testing.T) {
	ctx := t.Context()
	db := openTemp(t)

	version, err := db.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != 0 {
		t.Errorf("SchemaVersion = %d, want 0", version)
	}

	applied, err := db.Applied(ctx)
	if err != nil {
		t.Fatalf("Applied: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("Applied returned %d rows, want 0", len(applied))
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	ctx := t.Context()
	db := openTemp(t)
	if _, err := db.Migrate(ctx, migrations.FS); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// SQLite defaults foreign key enforcement off; the DSN pragma turns it
	// on, and a unit pointing at no property must be refused.
	_, err := db.Writer().ExecContext(ctx,
		`INSERT INTO units (property_id, label, created_at, updated_at) VALUES (999, 'A', '', '')`)
	if err == nil {
		t.Fatal("inserted a unit against a nonexistent property, want a foreign key error")
	}
}

// openTemp opens a database in a directory scoped to the test.
func openTemp(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "rental.db"), 2)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
