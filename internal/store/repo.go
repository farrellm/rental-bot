package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// Repo is the generated query layer bound to the pools this package owns.
//
// sqlc generates one *Queries against one DBTX, but writes have to go through
// the single-connection writer pool and reads through the reader pool. Rather
// than leave that to each call site's memory, Repo holds one *Queries per pool
// and names them: repo.Read() and repo.Write() read as what they are, and a
// mistake is visible in review rather than at 3am under contention.
type Repo struct {
	db    *DB
	read  *sqlc.Queries
	write *sqlc.Queries
}

// Repo returns the query layer for this database.
func (db *DB) Repo() *Repo {
	return &Repo{
		db:    db,
		read:  sqlc.New(db.Reader()),
		write: sqlc.New(db.Writer()),
	}
}

// Read returns the queries bound to the reader pool.
func (r *Repo) Read() *sqlc.Queries { return r.read }

// Write returns the queries bound to the writer pool, which is one connection.
func (r *Repo) Write() *sqlc.Queries { return r.write }

// Tx runs fn inside a write transaction, committing when it returns nil and
// rolling back otherwise.
//
// This is what a read-modify-write needs: PATCH reads the current row, merges
// the patch in Go, and writes every column back, because a nullable column has
// three states on the wire and COALESCE cannot tell "absent" from "null".
// Doing that read inside the write transaction keeps the merge honest even
// though the single writer connection already serialises the writes.
func (r *Repo) Tx(ctx context.Context, fn func(q *sqlc.Queries) error) error {
	tx, err := r.db.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	if err := fn(r.write.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// ErrNotFound reports that a row the caller named does not exist.
//
// sqlc surfaces a missing :one row as sql.ErrNoRows, which is a database
// detail; handlers care about the distinction between "no such property" and
// "the query failed", and they should not have to import database/sql to ask.
var ErrNotFound = errors.New("store: not found")

// NotFound reports whether err means the row was absent.
func NotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound)
}

// Conflict reports whether err is a uniqueness violation, which a handler
// answers with 409 rather than 500: two units labelled "Main" on one property
// is the caller's mistake, not the server's.
//
// modernc.org/sqlite reports constraint failures as a message rather than a
// typed error, so this and ForeignKey match on the text SQLite itself produces.
func Conflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// ForeignKey reports whether err is a reference to a row that is not there,
// which a handler answers with 422: naming a unit that does not exist is a bad
// request, not a broken server.
func ForeignKey(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}
