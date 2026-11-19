package spa

import (
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

const indexHTML = `<!doctype html><title>App</title><div id="root"></div>`

// buildFS looks like an app's Vite build.
var buildFS = fstest.MapFS{
	"index.html":                {Data: []byte(indexHTML)},
	"sw.js":                     {Data: []byte("self.skipWaiting()")},
	"workbox-27cf6706.js":       {Data: []byte("// workbox")},
	"manifest.webmanifest":      {Data: []byte(`{"name":"App"}`)},
	"favicon.ico":               {Data: []byte("\x00\x00\x01\x00")},
	"favicon.svg":               {Data: []byte("<svg></svg>")},
	"pwa-192x192.png":           {Data: []byte("\x89PNG\r\n\x1a\n")},
	"assets/index-BhcFijKM.js":  {Data: []byte("console.log('app')")},
	"assets/index-Z44OEHm8.css": {Data: []byte("body{}")},
}

func serve(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestHandler(t *testing.T) {
	tests := []struct {
		name            string
		path            string
		wantStatus      int
		wantBody        string
		wantContentType string
		wantCache       string
	}{
		{"root is index.html", "/", 200, indexHTML, "text/html; charset=utf-8", noCache},
		{"index.html by name", "/index.html", 200, indexHTML, "text/html; charset=utf-8", noCache},
		{"SPA route falls back to index.html", "/lists/groceries", 200, indexHTML, "text/html; charset=utf-8", noCache},
		{"unknown file outside assets falls back too", "/robots.txt", 200, indexHTML, "text/html; charset=utf-8", noCache},
		{"hashed script", "/assets/index-BhcFijKM.js", 200, "console.log('app')", "text/javascript; charset=utf-8", immutable},
		{"hashed stylesheet", "/assets/index-Z44OEHm8.css", 200, "body{}", "text/css; charset=utf-8", immutable},
		{"missing hashed file is a 404, not HTML", "/assets/index-old.js", 404, "404 page not found\n", "text/plain; charset=utf-8", noCache},
		{"assets directory is a 404", "/assets/", 404, "404 page not found\n", "text/plain; charset=utf-8", noCache},
		{"service worker is revalidated", "/sw.js", 200, "self.skipWaiting()", "text/javascript; charset=utf-8", noCache},
		{"workbox runtime outside assets is revalidated", "/workbox-27cf6706.js", 200, "// workbox", "text/javascript; charset=utf-8", noCache},
		{"manifest", "/manifest.webmanifest", 200, `{"name":"App"}`, "application/manifest+json", noCache},
		{"favicon.ico", "/favicon.ico", 200, "\x00\x00\x01\x00", "image/x-icon", noCache},
		{"favicon.svg", "/favicon.svg", 200, "<svg></svg>", "image/svg+xml", noCache},
		{"icon", "/pwa-192x192.png", 200, "\x89PNG\r\n\x1a\n", "image/png", noCache},
	}
	h := Handler("App", buildFS, slog.New(slog.DiscardHandler))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(t, h, http.MethodGet, tt.path)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if rec.Body.String() != tt.wantBody {
				t.Errorf("body = %q, want %q", rec.Body, tt.wantBody)
			}
			if got := rec.Header().Get("Content-Type"); got != tt.wantContentType {
				t.Errorf("Content-Type = %q, want %q", got, tt.wantContentType)
			}
			if got := rec.Header().Get("Cache-Control"); got != tt.wantCache {
				t.Errorf("Cache-Control = %q, want %q", got, tt.wantCache)
			}
		})
	}
}

func TestHandlerHead(t *testing.T) {
	rec := serve(t, Handler("App", buildFS, slog.New(slog.DiscardHandler)), http.MethodHead, "/")
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("HEAD / = %d with body %q, want 200 and no body", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/html", got)
	}
}

func TestHandlerOtherMethods(t *testing.T) {
	for _, files := range []fs.FS{buildFS, nil} {
		rec := serve(t, Handler("App", files, slog.New(slog.DiscardHandler)), http.MethodPost, "/")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("POST / (embedded %v) = %d, Allow %q; want 405 with Allow: GET, HEAD",
				files != nil, rec.Code, rec.Header().Get("Allow"))
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want a JSON error", ct)
		}
	}
}

func TestHandlerNotEmbedded(t *testing.T) {
	h := Handler("Notes & co", nil, slog.New(slog.DiscardHandler))
	for _, p := range []string{"/", "/lists/groceries", "/sw.js"} {
		rec := serve(t, h, http.MethodGet, p)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "make dev") {
			t.Errorf("GET %s = %d %q, want the not-embedded notice", p, rec.Code, rec.Body)
		}
		if want := "<title>Notes &amp; co: frontend not embedded</title>"; !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET %s = %q, want the app's name in the title, escaped: %s", p, rec.Body, want)
		}
		if got := rec.Header().Get("Cache-Control"); got != noCache {
			t.Errorf("Cache-Control = %q, want %q", got, noCache)
		}
	}
}

func TestHandlerMissingIndex(t *testing.T) {
	rec := serve(t, Handler("App", fstest.MapFS{}, slog.New(slog.DiscardHandler)), http.MethodGet, "/")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 when the build has no index.html", rec.Code)
	}
}
