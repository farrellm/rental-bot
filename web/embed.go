//go:build !spa

// Package web carries the built single-page app into the binary.
//
// The embed is behind the `spa` build tag so that a plain `go build` and
// `go test ./...` work in a fresh clone, where web/dist does not exist yet.
// `make build` runs the frontend build first and then builds with the tag;
// that is the binary you deploy. Without the tag the server still serves the
// API and answers the root with a page saying which build this is.
package web

import "io/fs"

// Embedded reports whether this binary carries the frontend.
const Embedded = false

// SPA returns the built frontend, or nil when it was not built in.
func SPA() fs.FS { return nil }
