package httpapi

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// spaHandler serves the built frontend, falling back to index.html so the
// client-side router owns every path the server does not.
func (s *server) spaHandler() http.Handler {
	if s.spa == nil {
		return http.HandlerFunc(s.handleMissingSPA)
	}
	files := http.FileServerFS(s.spa)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			WriteProblem(w, r, http.StatusMethodNotAllowed, "This endpoint only answers GET.")
			return
		}

		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" || name == "." {
			s.serveIndex(w, r)
			return
		}
		if _, err := fs.Stat(s.spa, name); err != nil {
			// Not a file we ship, so it is a client-side route.
			s.serveIndex(w, r)
			return
		}

		// Vite fingerprints asset filenames, so they can be cached hard.
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	})
}

// serveIndex writes the SPA shell. It is never cached: the document names the
// fingerprinted assets, so a stale copy pins the client to an old build.
func (s *server) serveIndex(w http.ResponseWriter, r *http.Request) {
	index, err := fs.ReadFile(s.spa, "index.html")
	if err != nil {
		loggerFrom(r.Context()).Error("read index.html", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "The application shell is missing from this build.")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(index); err != nil {
		loggerFrom(r.Context()).Debug("write index.html", "error", err)
	}
}

// missingSPA is what a binary built without the `spa` tag serves at the root.
// It says what happened and what to run, because the alternative is an
// operator staring at a blank 404 wondering which half is broken.
const missingSPA = `<!doctype html>
<meta charset="utf-8">
<title>rental-bot</title>
<style>
  body { background:#132227; color:#E8E2D2; font:16px/1.6 ui-monospace,monospace;
         margin:0; display:grid; place-items:center; min-height:100vh; padding:2rem; }
  div { max-width:34rem }
  code { color:#8FBFB0 }
</style>
<div>
  <p>The API is running. The frontend is not in this binary.</p>
  <p>Build it in with <code>make build</code>, or run <code>make dev</code> to
     serve the app from Vite on port 5174.</p>
  <p><a href="/readyz" style="color:#8FBFB0">Check readiness</a></p>
</div>
`

func (s *server) handleMissingSPA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		WriteProblem(w, r, http.StatusMethodNotAllowed, "This endpoint only answers GET.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(missingSPA)); err != nil {
		loggerFrom(r.Context()).Debug("write placeholder", "error", err)
	}
}
