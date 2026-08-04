//go:build spa

package web

import (
	"embed"
	"io/fs"
)

// dist is the output of `npm run build`, rooted at index.html.
//
//go:embed all:dist
var dist embed.FS

// Embedded reports whether this binary carries the frontend.
const Embedded = true

// SPA returns the built frontend, rooted so that index.html is at the top.
func SPA() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// Only reachable if the embed directive above changes; the
		// compiler has already proven dist exists.
		panic("web: embedded dist is unreadable: " + err.Error())
	}
	return sub
}
