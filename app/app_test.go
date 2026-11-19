package app

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juanmonsalveg/homelab-go/auth"
	"github.com/juanmonsalveg/homelab-go/config"
	"github.com/juanmonsalveg/homelab-go/database/dbtest"
	"github.com/juanmonsalveg/homelab-go/httpx"
)

// migrations stands in for an app's db/migrations.
var migrations = fstest.MapFS{
	"00001_create_users.sql": {Data: []byte(`-- +goose Up
CREATE TABLE users (
    id       uuid PRIMARY KEY DEFAULT uuidv7(),
    username text NOT NULL UNIQUE
);
`)},
}

// fakeUsers stands in for an app's users store: everyone gets the same id.
type fakeUsers struct{}

func (fakeUsers) UpsertUser(_ context.Context, id auth.Identity) (auth.User, error) {
	return auth.User{ID: uuid.MustParse("019a6b1e-0000-7000-8000-000000000001"), Username: id.Username}, nil
}

func (fakeUsers) OtherUsers(context.Context, uuid.UUID) ([]auth.Person, error) { return nil, nil }

// testSpec is a small app named notes, with one route of its own.
func testSpec() Spec {
	return Spec{
		Name:       "notes",
		Migrations: migrations,
		Users:      func(*pgxpool.Pool) auth.UserStore { return fakeUsers{} },
		Routes: func(mux *http.ServeMux, d Deps) {
			mux.HandleFunc("GET /api/notes", func(w http.ResponseWriter, _ *http.Request) {
				var n int
				if err := d.Pool.QueryRow(context.Background(), "SELECT count(*) FROM users").Scan(&n); err != nil {
					httpx.WriteError(w, http.StatusInternalServerError, "internal", err.Error())
					return
				}
				httpx.WriteJSON(w, http.StatusOK, map[string]any{"users": n, "appUrl": d.Config.AppURL})
			})
		},
	}
}

func noEnv(string) string { return "" }

func runCmd(t *testing.T, getenv func(string) string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(t.Context(), testSpec(), args, getenv, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestRunInvalidSpec(t *testing.T) {
	var stderr bytes.Buffer
	code := run(t.Context(), Spec{}, []string{"help"}, noEnv, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	for _, want := range []string{"Spec.Name is required", "Spec.Migrations is required", "Spec.Users is required"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
		}
	}
}

func TestRunUsage(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string
	}{
		{"help", []string{"help"}, 0, ""},
		{"unknown command", []string{"frobnicate"}, 2, `unknown command "frobnicate"`},
		{"serve with arguments", []string{"serve", "now"}, 2, "serve takes no arguments"},
		{"healthcheck with arguments", []string{"healthcheck", "now"}, 2, "healthcheck takes no arguments"},
		{"migrate unknown action", []string{"migrate", "down"}, 2, "up or status"},
		{"migrate too many arguments", []string{"migrate", "up", "now"}, 2, "up or status"},
		{"migrate with invalid config", []string{"migrate"}, 1, "DATABASE_URL is required"},
		{"migrate status with invalid config", []string{"migrate", "status"}, 1, "DATABASE_URL is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runCmd(t, noEnv, tt.args...)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr: %s)", code, tt.wantCode, stderr)
			}
			if !strings.Contains(stderr, tt.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantErr)
			}
			if tt.wantCode == 0 && !strings.Contains(stdout, "Usage: notes [command]") {
				t.Errorf("stdout = %q, want the usage text", stdout)
			}
		})
	}
}

func TestServeInvalidConfig(t *testing.T) {
	// No arguments means serve.
	code, _, stderr := runCmd(t, noEnv)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	for _, want := range []string{"notes: invalid configuration", "  - DATABASE_URL is required", "  - APP_URL is required"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr, want)
		}
	}
}

// Integration test: needs TEST_DATABASE_URL (make test-integration).
func TestMigrateCommand(t *testing.T) {
	vars := map[string]string{
		"DATABASE_URL": dbtest.URL(t),
		"APP_URL":      "http://localhost:5173",
		"APP_ENV":      "development",
	}
	getenv := func(key string) string { return vars[key] }

	code, stdout, _ := runCmd(t, getenv, "migrate", "status")
	if code != 0 || !regexp.MustCompile(`(?m)^1 +pending +00001_create_users\.sql$`).MatchString(stdout) {
		t.Fatalf("migrate status before = %d\n%s\nwant 0 and 00001 pending", code, stdout)
	}

	code, _, stderr := runCmd(t, getenv, "migrate")
	if code != 0 || !strings.Contains(stderr, "applied migration") || !strings.Contains(stderr, "00001_create_users.sql") {
		t.Fatalf("migrate = %d, stderr:\n%s\nwant 0 and 00001 applied", code, stderr)
	}

	code, stdout, _ = runCmd(t, getenv, "migrate", "status")
	if code != 0 || !strings.HasPrefix(stdout, "VERSION") || strings.Contains(stdout, "pending") ||
		!strings.Contains(stdout, "00001_create_users.sql") {
		t.Errorf("migrate status after = %d\n%s\nwant 0 and 00001 applied", code, stdout)
	}

	code, _, stderr = runCmd(t, getenv, "migrate", "up")
	if code != 0 || strings.Contains(stderr, "applied migration") {
		t.Errorf("second migrate = %d, stderr:\n%s\nwant 0 and nothing applied", code, stderr)
	}
}

// envWithPort returns a getenv that sets PORT to the port of a URL like http://127.0.0.1:1234.
func envWithPort(t *testing.T, rawURL string) func(string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return func(key string) string {
		if key == "PORT" {
			return u.Port()
		}
		return ""
	}
}

func TestHealthcheck(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		wantCode int
	}{
		{"healthy", http.StatusOK, 0},
		{"unavailable", http.StatusServiceUnavailable, 1},
		{"redirect", http.StatusFound, 1},
		{"not found", http.StatusNotFound, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				if tt.status == http.StatusFound {
					w.Header().Set("Location", "/healthz")
				}
				w.WriteHeader(tt.status)
			}))
			t.Cleanup(srv.Close)

			code, _, stderr := runCmd(t, envWithPort(t, srv.URL), "healthcheck")
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr: %s)", code, tt.wantCode, stderr)
			}
			if gotPath != "/healthz" {
				t.Errorf("requested %q, want /healthz", gotPath)
			}
		})
	}
}

func TestHealthcheckNothingListening(t *testing.T) {
	code, _, stderr := runCmd(t, envWithPort(t, "http://127.0.0.1:"+freePort(t)), "healthcheck")
	if code != 1 || !strings.Contains(stderr, "notes: healthcheck") {
		t.Errorf("exit code = %d, stderr = %q; want 1 and an error message", code, stderr)
	}
}

func TestHealthcheckInvalidPort(t *testing.T) {
	code, _, stderr := runCmd(t, func(string) string { return "nope" }, "healthcheck")
	if code != 1 || !strings.Contains(stderr, "PORT must be") {
		t.Errorf("exit code = %d, stderr = %q; want 1 and a PORT error", code, stderr)
	}
}

func TestNewLogger(t *testing.T) {
	var buf bytes.Buffer
	newLogger(&buf, config.Base{Env: config.Production}).Info("hello")
	if !strings.HasPrefix(buf.String(), "{") {
		t.Errorf("production log = %q, want JSON", buf.String())
	}

	buf.Reset()
	newLogger(&buf, config.Base{Env: config.Development}).Info("hello")
	if !strings.HasPrefix(buf.String(), "time=") {
		t.Errorf("development log = %q, want text", buf.String())
	}

	buf.Reset()
	newLogger(&buf, config.Base{Env: config.Production, LogLevel: slog.LevelWarn}).Info("hidden")
	if buf.Len() != 0 {
		t.Errorf("info logged at warn level: %q", buf.String())
	}
}

// freePort returns a loopback port nothing is listening on, for serve's PORT.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	_ = ln.Close()
	return port
}

// Integration test: needs TEST_DATABASE_URL (make test-integration). It runs serve the way an
// app's container does: migrations at startup, /healthz, an app route on the pool, then SIGTERM's
// path, a canceled context, ending in exit 0.
func TestServe(t *testing.T) {
	port := freePort(t)
	vars := map[string]string{
		"DATABASE_URL": dbtest.URL(t),
		"APP_URL":      "https://notes.lab.juanmonsalve.dev",
		"PORT":         port,
	}
	ctx, cancel := context.WithCancel(t.Context())
	var stderr syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, testSpec(), []string{"serve"}, func(k string) string { return vars[k] }, &bytes.Buffer{}, &stderr)
	}()

	base := "http://127.0.0.1:" + port
	waitHealthy(t, base, done, &stderr)

	get := func(path string, headers map[string]string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var body bytes.Buffer
		_, _ = body.ReadFrom(resp.Body)
		return resp.StatusCode, body.String()
	}

	// The route runs on the app's pool, after the migrations made the users table.
	if code, body := get("/api/notes", map[string]string{"Remote-User": "ana"}); code != http.StatusOK ||
		body != `{"appUrl":"https://notes.lab.juanmonsalve.dev","users":0}`+"\n" {
		t.Errorf("GET /api/notes = %d %q, want 200 with no users and the app's URL", code, body)
	}
	if code, _ := get("/api/notes", nil); code != http.StatusUnauthorized {
		t.Errorf("GET /api/notes without Remote-User = %d, want 401", code)
	}
	if code, body := get("/api/me", map[string]string{"Remote-User": "ana"}); code != http.StatusOK || !strings.Contains(body, `"username":"ana"`) {
		t.Errorf("GET /api/me = %d %q, want 200 as ana", code, body)
	}
	if code, body := get("/", map[string]string{"Remote-User": "ana"}); code != http.StatusOK || !strings.Contains(body, "<title>notes: frontend not embedded</title>") {
		t.Errorf("GET / = %d %q, want the not-embedded notice with the app's name", code, body)
	}

	cancel()
	if code := <-done; code != 0 {
		t.Errorf("serve exited %d after the stop, want 0; logs:\n%s", code, stderr.String())
	}
	for _, want := range []string{`"msg":"applied migration"`, `"app":"notes"`, `"msg":"shutting down"`, `"msg":"stopped"`} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("logs don't contain %s:\n%s", want, stderr.String())
		}
	}
}

// waitHealthy waits until base/healthz answers 200, failing early if serve exits first.
func waitHealthy(t *testing.T, base string, done <-chan int, logs *syncBuffer) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case code := <-done:
			t.Fatalf("serve exited %d before it was healthy; logs:\n%s", code, logs.String())
		default:
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("serve wasn't healthy after 15 s; logs:\n%s", logs.String())
}

// syncBuffer is a bytes.Buffer that serve's goroutine and the test can share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
