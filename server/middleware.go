package server

import (
	"log/slog"
	"net/http"

	"github.com/juanmonsalveg/homelab-go/httpx"
)

// DefaultContentSecurityPolicy allows only the app's own scripts, styles and connections, plus
// inline styles for the <style> tags Mantine renders at runtime (README.md §6). Scripts stay at
// 'self'. frame-ancestors 'none' also stops other *.lab sites from framing the app. An app loosens
// it (Deps.ContentSecurityPolicy) only when something actually breaks.
const DefaultContentSecurityPolicy = "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'self'; form-action 'self'"

// securityHeaders sets the security headers on every response, /healthz and errors included.
func securityHeaders(csp string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "same-origin")
			h.Set("Content-Security-Policy", csp)
			next.ServeHTTP(w, r)
		})
	}
}

// requireSameOrigin is the CSRF guard (README.md §6). Authelia's login cookie is set for
// lab.juanmonsalve.dev, so the browser also sends it when another page, another *.lab site
// included, makes a request to the app. Every request that may change data (any method but GET,
// HEAD and OPTIONS) must therefore carry an Origin equal to appURL, which config normalizes to the
// form browsers send. A missing Origin is rejected too: browsers always send one with these
// methods.
//
// Safe methods pass unchecked, so their handlers must never change the app's data.
func requireSameOrigin(appURL string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
			default:
				if origin := r.Header.Get("Origin"); origin != appURL {
					// A wrong APP_URL on the server shows up here as every write failing.
					httpx.AddLogAttrs(r.Context(), slog.String("rejected_origin", origin))
					httpx.WriteError(w, http.StatusForbidden, "cross_origin", "Cross-origin request rejected")
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
