// Package store owns the SQLite connection pools and the migration runner.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go driver: static binary, FTS5 included
)

// pragmas are applied to every connection, reader and writer alike.
//
//	journal_mode=WAL   readers do not block the writer
//	busy_timeout=5000  wait out a held lock instead of failing immediately
//	foreign_keys=ON    SQLite defaults this off, which is a trap
//	synchronous=NORMAL the durability WAL is designed for
var pragmas = []string{
	"journal_mode(WAL)",
	"busy_timeout(5000)",
	"foreign_keys(ON)",
	"synchronous(NORMAL)",
}

// DB holds the two pools this application talks to.
//
// SQLite allows exactly one writer. Rather than discover that under load, the
// writer pool is capped at a single connection and every mutation goes
// through it; reads use a separate pool and proceed concurrently. Writes
// therefore serialize, which docs/DESIGN.md §2 accepts knowingly.
type DB struct {
	writer *sql.DB
	reader *sql.DB
	path   string
}

// Open prepares the database file and both pools. The parent directory is
// created if it does not exist.
func Open(ctx context.Context, path string, readPoolSize int) (*DB, error) {
	if path == "" {
		return nil, fmt.Errorf("store: database path is empty")
	}
	if readPoolSize < 1 {
		readPoolSize = 1
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("store: create %s: %w", dir, err)
		}
	}

	writer, err := open(ctx, path, 1)
	if err != nil {
		return nil, err
	}
	reader, err := open(ctx, path, readPoolSize)
	if err != nil {
		writer.Close()
		return nil, err
	}
	return &DB{writer: writer, reader: reader, path: path}, nil
}

func open(ctx context.Context, path string, maxConns int) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	return db, nil
}

// dsn builds a driver connection string carrying the pragmas.
func dsn(path string) string {
	q := make(url.Values, len(pragmas))
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	return "file:" + path + "?" + q.Encode()
}

// Writer returns the single-connection pool. Every mutation uses it.
func (db *DB) Writer() *sql.DB { return db.writer }

// Reader returns the concurrent read pool.
func (db *DB) Reader() *sql.DB { return db.reader }

// Path reports the database file this DB was opened from.
func (db *DB) Path() string { return db.path }

// Ping reports whether the database is reachable, from the reader pool.
func (db *DB) Ping(ctx context.Context) error {
	return db.reader.PingContext(ctx)
}

// Close shuts both pools down, reporting whatever failed.
//
// Both are closed even if the first one refuses, because leaving the writer
// connection open would keep the WAL from being checkpointed.
func (db *DB) Close() error {
	readErr := db.reader.Close()
	writeErr := db.writer.Close()

	if readErr != nil {
		readErr = fmt.Errorf("store: close reader: %w", readErr)
	}
	if writeErr != nil {
		writeErr = fmt.Errorf("store: close writer: %w", writeErr)
	}
	return errors.Join(readErr, writeErr)
}
