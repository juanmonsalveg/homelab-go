// Package spa serves an app's React SPA and its PWA files (README.md §9). The app embeds its Vite
// build and passes it in. A binary built without it (dev, `go test ./...`) passes nil, and every
// page is a short notice pointing to the Vite dev server, so Go builds never need a frontend build.
package spa

import (
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/juanmonsalveg/homelab-go/httpx"
)

const (
	// Files under assets/ have content hashes in their names, so a new build never reuses a URL.
	immutable = "public, max-age=31536000, immutable"
	// Everything else, index.html, sw.js and the manifest included, is revalidated on every load,
	// so a deploy reaches the browser and the service worker at once.
	noCache = "no-cache"
)

// Handler serves GET and HEAD requests from files, the app's Vite build. A path that isn't a file
// gets index.html, so the SPA can route it, except under /assets/: a missing hashed file is a 404,
// never HTML that would be cached as a script for a year. With files nil, every page is the
// not-embedded notice, titled with the app's name.
func Handler(name string, files fs.FS, logger *slog.Logger) http.Handler {
	methodNotAllowed := httpx.MethodNotAllowed("GET, HEAD")
	notEmbedded := fmt.Sprintf(notEmbeddedPage, html.EscapeString(name))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			methodNotAllowed.ServeHTTP(w, r)
			return
		}
		if files == nil {
			serveNotEmbedded(w, notEmbedded)
			return
		}

		file := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if file == "" {
			file = "index.html"
		}
		if !exists(files, file) {
			if isAsset(file) {
				w.Header().Set("Cache-Control", noCache)
				http.NotFound(w, r)
				return
			}
			file = "index.html"
		}
		serveFile(w, r, files, file, logger)
	})
}

// exists reports whether name is a regular file in files. Directories count as missing.
func exists(files fs.FS, name string) bool {
	info, err := fs.Stat(files, name)
	return err == nil && !info.IsDir()
}

// isAsset reports whether the cleaned name is Vite's assets directory or a file in it.
func isAsset(name string) bool {
	return name == "assets" || strings.HasPrefix(name, "assets/")
}

func serveFile(w http.ResponseWriter, r *http.Request, files fs.FS, name string, logger *slog.Logger) {
	f, err := files.Open(name)
	if err != nil {
		internalError(w, r, logger, name, err)
		return
	}
	defer func() { _ = f.Close() }()
	content, ok := f.(io.ReadSeeker)
	if !ok {
		internalError(w, r, logger, name, errors.New("file can't seek"))
		return
	}

	h := w.Header()
	if isAsset(name) {
		h.Set("Cache-Control", immutable)
	} else {
		h.Set("Cache-Control", noCache)
	}
	if ct := contentType(name); ct != "" {
		h.Set("Content-Type", ct)
	}
	// Embedded files have no modification time, so no Last-Modified is sent.
	http.ServeContent(w, r, name, time.Time{}, content)
}

// contentType covers the extensions Go's built-in MIME table lacks. Without them the type would
// depend on the host's mime.types file, which the distroless image doesn't have. For the rest,
// ServeContent looks the type up by extension.
func contentType(name string) string {
	switch path.Ext(name) {
	case ".webmanifest":
		return "application/manifest+json"
	case ".ico":
		return "image/x-icon"
	default:
		return ""
	}
}

func internalError(w http.ResponseWriter, r *http.Request, logger *slog.Logger, name string, err error) {
	logger.LogAttrs(r.Context(), slog.LevelError, "serve SPA file",
		slog.String("file", name), slog.Any("error", err))
	httpx.WriteError(w, http.StatusInternalServerError, "internal", "Internal server error")
}

const notEmbeddedPage = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>%s: frontend not embedded</title></head>
<body>
<h1>Frontend not embedded</h1>
<p>This binary was built without its frontend (the embedweb tag), so it serves only the API.</p>
<p>For development, run <code>make dev</code> and open <a href="http://localhost:5173">http://localhost:5173</a>.
For the single binary with the frontend, run <code>make run</code>.</p>
</body>
</html>
`

func serveNotEmbedded(w http.ResponseWriter, page string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", noCache)
	_, _ = io.WriteString(w, page)
}
