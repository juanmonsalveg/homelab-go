package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juanmonsalveg/homelab-go/httpx"
)

func TestSecurityHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	securityHeaders(DefaultContentSecurityPolicy)(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	want := map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "same-origin",
		"Content-Security-Policy": "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'self'; form-action 'self'",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

// Mantine's runtime <style> tags need inline styles (D9), but inline scripts must stay blocked: no
// script-src of its own, so scripts fall back to default-src 'self', and 'unsafe-inline' only in
// style-src.
func TestContentSecurityPolicyAllowsInlineStylesOnly(t *testing.T) {
	directives := map[string]string{}
	for d := range strings.SplitSeq(DefaultContentSecurityPolicy, ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(d), " ")
		directives[name] = value
	}
	if got := directives["default-src"]; got != "'self'" {
		t.Errorf("default-src = %q, want 'self'", got)
	}
	if got := directives["style-src"]; got != "'self' 'unsafe-inline'" {
		t.Errorf("style-src = %q, want 'self' 'unsafe-inline'", got)
	}
	for name, value := range directives {
		if strings.HasPrefix(name, "script-src") {
			t.Errorf("%s = %q, want no script directive", name, value)
		}
		if name != "style-src" && strings.Contains(value, "'unsafe-inline'") {
			t.Errorf("%s = %q allows inline content", name, value)
		}
	}
}

func TestRequireSameOrigin(t *testing.T) {
	tests := []struct {
		name   string
		method string
		origin string // "" means no Origin header
		wantOK bool
	}{
		{"GET without Origin", http.MethodGet, "", true},
		{"GET from another site", http.MethodGet, "https://evil.example", true},
		{"HEAD from another site", http.MethodHead, "https://evil.example", true},
		{"OPTIONS from another site", http.MethodOptions, "https://evil.example", true},
		{"POST from the app", http.MethodPost, testAppURL, true},
		{"PUT from the app", http.MethodPut, testAppURL, true},
		{"PATCH from the app", http.MethodPatch, testAppURL, true},
		{"DELETE from the app", http.MethodDelete, testAppURL, true},
		{"POST without Origin", http.MethodPost, "", false},
		{"POST from another site", http.MethodPost, "https://evil.example", false},
		{"POST from another *.lab site", http.MethodPost, "https://status.lab.juanmonsalve.dev", false},
		{"POST over plain HTTP", http.MethodPost, "http://app.lab.juanmonsalve.dev", false},
		{"POST from an opaque origin", http.MethodPost, "null", false},
		{"POST with a lookalike suffix", http.MethodPost, testAppURL + ".evil.example", false},
		{"PUT from another site", http.MethodPut, "https://evil.example", false},
		{"PATCH from another site", http.MethodPatch, "https://evil.example", false},
		{"DELETE from another site", http.MethodDelete, "https://evil.example", false},
		{"other methods are checked too", "PROPPATCH", "https://evil.example", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			h := requireSameOrigin(testAppURL)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))

			req := httptest.NewRequest(tt.method, "/api/items", nil)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if called != tt.wantOK {
				t.Errorf("handler called = %v, want %v", called, tt.wantOK)
			}
			if tt.wantOK {
				return
			}
			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
			if want := `{"error":{"code":"cross_origin","message":"Cross-origin request rejected"}}` + "\n"; rec.Body.String() != want {
				t.Errorf("body = %q, want %q", rec.Body.String(), want)
			}
		})
	}
}

func TestRequireSameOriginLogsRejectedOrigin(t *testing.T) {
	var buf bytes.Buffer
	h := httpx.LogRequests(slog.New(slog.NewJSONHandler(&buf, nil)))(requireSameOrigin(testAppURL)(http.NotFoundHandler()))

	req := httptest.NewRequest(http.MethodPost, "/api/items", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !strings.Contains(buf.String(), `"rejected_origin":"http://localhost:5173"`) {
		t.Errorf("request log = %s, want the rejected origin", buf.String())
	}
}
