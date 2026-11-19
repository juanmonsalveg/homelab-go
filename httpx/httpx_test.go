package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestLogger returns a JSON logger and the buffer it writes to.
func newTestLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// logLines decodes each JSON log line in buf.
func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for line := range strings.Lines(buf.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		lines = append(lines, m)
	}
	return lines
}

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusNotFound, "not_found", "No such route")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	want := `{"error":{"code":"not_found","message":"No such route"}}` + "\n"
	if rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	rec := httptest.NewRecorder()
	MethodNotAllowed("GET, HEAD").ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/items", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q, want GET, HEAD", got)
	}
	want := `{"error":{"code":"method_not_allowed","message":"Method not allowed"}}` + "\n"
	if rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}
}

func TestWriteJSONUnencodable(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusOK, map[string]any{"bad": make(chan int)})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestLogRequests(t *testing.T) {
	logger, buf := newTestLogger()
	h := LogRequests(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		AddLogAttrs(r.Context(), slog.String("user", "ana"))
		w.WriteHeader(http.StatusTeapot)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/items?secret=1", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	h.ServeHTTP(httptest.NewRecorder(), req)

	lines := logLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want 1: %s", len(lines), buf)
	}
	got := lines[0]
	want := map[string]any{
		"msg":       "request",
		"method":    "GET",
		"path":      "/api/items",
		"status":    float64(http.StatusTeapot),
		"client_ip": "203.0.113.9",
		"user":      "ana",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if _, ok := got["duration_ms"].(float64); !ok {
		t.Errorf("duration_ms = %v, want a number", got["duration_ms"])
	}
}

func TestLogRequestsImplicitStatus(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"writes nothing", func(http.ResponseWriter, *http.Request) {}},
		{"writes body only", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }},
		{"sends 103 first", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusEarlyHints)
			_, _ = w.Write([]byte("ok"))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, buf := newTestLogger()
			LogRequests(logger)(tt.handler).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
			if got := logLines(t, buf)[0]["status"]; got != float64(http.StatusOK) {
				t.Errorf("status = %v, want 200", got)
			}
		})
	}
}

func TestAddLogAttrsOutsideLogRequests(t *testing.T) {
	// Must not panic when nothing collects the attributes.
	AddLogAttrs(t.Context(), slog.String("user", "x"))
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		xff        []string
		remoteAddr string
		want       string
	}{
		{"no header", nil, "192.0.2.1:54321", "192.0.2.1"},
		{"single entry", []string{"203.0.113.9"}, "172.18.0.2:80", "203.0.113.9"},
		{"chain uses the entry Traefik appended", []string{"198.51.100.7, 203.0.113.9"}, "172.18.0.2:80", "203.0.113.9"},
		{"repeated headers use the last", []string{"198.51.100.7", "203.0.113.9"}, "172.18.0.2:80", "203.0.113.9"},
		{"empty entry falls back", []string{"203.0.113.9, "}, "192.0.2.1:54321", "192.0.2.1"},
		{"IPv6 remote address", nil, "[2001:db8::1]:54321", "2001:db8::1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tt.remoteAddr
			for _, v := range tt.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := clientIP(r); got != tt.want {
				t.Errorf("clientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRecover(t *testing.T) {
	logger, buf := newTestLogger()
	h := LogRequests(logger)(Recover(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/items", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if want := `{"error":{"code":"internal","message":"Internal server error"}}` + "\n"; rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}

	lines := logLines(t, buf)
	if len(lines) != 2 {
		t.Fatalf("got %d log lines, want 2 (panic, request): %s", len(lines), buf)
	}
	if lines[0]["level"] != "ERROR" || lines[0]["panic"] != "boom" || !strings.Contains(lines[0]["stack"].(string), "TestRecover") {
		t.Errorf("panic log line = %v", lines[0])
	}
	if lines[1]["msg"] != "request" || lines[1]["status"] != float64(http.StatusInternalServerError) {
		t.Errorf("request log line = %v, want status 500", lines[1])
	}
}

func TestRecoverAfterResponseStarted(t *testing.T) {
	logger, buf := newTestLogger()
	h := Recover(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("partial"))
		panic("boom")
	}))

	v := recoverFrom(func() { h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil)) })
	if err, ok := v.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
		t.Errorf("panic = %v, want http.ErrAbortHandler", v)
	}
	if !strings.Contains(buf.String(), "boom") {
		t.Errorf("the original panic wasn't logged: %s", buf)
	}
}

func TestRecoverPassesAbortHandlerThrough(t *testing.T) {
	logger, buf := newTestLogger()
	h := Recover(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	v := recoverFrom(func() { h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil)) })
	if err, ok := v.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
		t.Errorf("panic = %v, want http.ErrAbortHandler", v)
	}
	if buf.Len() != 0 {
		t.Errorf("logged a deliberate abort: %s", buf)
	}
}

func recoverFrom(f func()) (v any) {
	defer func() { v = recover() }()
	f()
	return nil
}
