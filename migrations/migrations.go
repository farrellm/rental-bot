// Package migrations holds the numbered SQL files applied at startup.
//
// Migrations are append-only. An applied migration is never edited — the
// runner records a checksum and refuses to start if one changes, because a
// file that has already run on the production database is history, not
// source. Corrections land as a new NNNN_ file.
package migrations

import "embed"

// FS holds every migration, named NNNN_description.sql.
//
//go:embed *.sql
var FS embed.FS
