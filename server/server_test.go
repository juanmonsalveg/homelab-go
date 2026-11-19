package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/juanmonsalveg/homelab-go/auth"
	"github.com/juanmonsalveg/homelab-go/config"
	"github.com/juanmonsalveg/homelab-go/health"
	"github.com/juanmonsalveg/homelab-go/httpx"
)

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// fakeDB answers every query with err, or with 1 when err is nil.
type fakeDB struct{ err error }

func (f fakeDB) QueryRow(context.Context, string, ...any) pgx.Row { return fakeRow(f) }

type fakeRow struct{ err error }

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*int) = 1
	return nil
}

func TestNew(t *testing.T) {
	s := New(3000, http.NotFoundHandler(), discardLogger())
	if s.srv.Addr != "0.0.0.0:3000" {
		t.Errorf("Addr = %q, want 0.0.0.0:3000", s.srv.Addr)
	}
	if s.srv.ReadHeaderTimeout != 5*time.Second || s.srv.ReadTimeout != 15*time.Second ||
		s.srv.WriteTimeout != 30*time.Second || s.srv.IdleTimeout != 120*time.Second {
		t.Errorf("timeouts = %v/%v/%v/%v, want 5s/15s/30s/2m",
			s.srv.ReadHeaderTimeout, s.srv.ReadTimeout, s.srv.WriteTimeout, s.srv.IdleTimeout)
	}
	if s.shutdownTimeout != 8*time.Second {
		t.Errorf("shutdownTimeout = %v, want 8s", s.shutdownTimeout)
	}
}

const testAppURL = "https://app.lab.juanmonsalve.dev"

var testUserID = uuid.MustParse("019a6b1e-0000-7000-8000-000000000001")

// fakeUsers counts upserts, answers with a fixed id and knows nobody else.
type fakeUsers struct{ calls int }

func (f *fakeUsers) UpsertUser(_ context.Context, id auth.Identity) (auth.User, error) {
	f.calls++
	return auth.User{ID: testUserID, Username: id.Username}, nil
}

func (f *fakeUsers) OtherUsers(context.Context, uuid.UUID) ([]auth.Person, error) { return nil, nil }

// itemRoutes stands in for an app's route table: a collection, one item and an action on it, each
// with its 405 like an app's. The handlers answer with the caller's username, or 204.
func itemRoutes(mux *http.ServeMux) {
	ok := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			u, ok := auth.RequireUser(w, r, discardLogger())
			if !ok {
				return
			}
			if status == http.StatusNoContent {
				w.WriteHeader(status)
				return
			}
			httpx.WriteJSON(w, status, map[string]string{"user": u.Username})
		}
	}
	mux.Handle("GET /api/items", ok(http.StatusOK))
	mux.Handle("POST /api/items", ok(http.StatusCreated))
	mux.Handle("/api/items", httpx.MethodNotAllowed("GET, HEAD, POST"))
	mux.Handle("PATCH /api/items/{id}", ok(http.StatusOK))
	mux.Handle("DELETE /api/items/{id}", ok(http.StatusNoContent))
	mux.Handle("/api/items/{id}", httpx.MethodNotAllowed("PATCH, DELETE"))
	mux.Handle("PUT /api/items/{id}/done", ok(http.StatusNoContent))
	mux.Handle("/api/items/{id}/done", httpx.MethodNotAllowed("PUT"))
}

// newTestHandler returns NewHandler in production mode with fakes for the database and the
// item routes.
func newTestHandler(logger *slog.Logger, db health.DB, users auth.UserStore) http.Handler {
	return NewHandler(Deps{Name: "App", Logger: logger, DB: db, Users: users, AppURL: testAppURL}, itemRoutes)
}

func TestNewHandler(t *testing.T) {
	var buf bytes.Buffer
	h := newTestHandler(slog.New(slog.NewJSONHandler(&buf, nil)), fakeDB{}, &fakeUsers{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/nope", nil)
	req.Header.Set("Remote-User", "ana")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	for _, want := range []string{`"msg":"request"`, `"status":404`, `"user":"ana"`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("request log = %s, want it to contain %s", buf.String(), want)
		}
	}
}

// TestNewHandlerAuthAndCSRF checks the wiring: which requests reach the routes.
func TestNewHandlerAuthAndCSRF(t *testing.T) {
	const item = "/api/items/019a7000-0000-7000-8000-000000000001"
	tests := []struct {
		name       string
		method     string
		path       string
		headers    map[string]string
		wantStatus int
		wantUpsert bool
	}{
		{"no Remote-User", http.MethodGet, "/api/items", nil, http.StatusUnauthorized, false},
		{"no Remote-User on the SPA", http.MethodGet, "/", nil, http.StatusUnauthorized, false},
		{"no Remote-User on an unknown API path", http.MethodGet, "/api/nope", nil, http.StatusUnauthorized, false},
		{"/healthz/ isn't /healthz", http.MethodGet, "/healthz/", nil, http.StatusUnauthorized, false},
		{"logged in", http.MethodGet, "/api/items", map[string]string{"Remote-User": "ana"}, http.StatusOK, true},
		{"production ignores DEV_REMOTE_USER", http.MethodGet, "/api/items", nil, http.StatusUnauthorized, false},
		{"write from the app's origin", http.MethodPost, "/api/items",
			map[string]string{"Remote-User": "ana", "Origin": testAppURL}, http.StatusCreated, true},
		{"write without Origin", http.MethodPost, "/api/items",
			map[string]string{"Remote-User": "ana"}, http.StatusForbidden, true},
		{"edit from another *.lab site", http.MethodPatch, item,
			map[string]string{"Remote-User": "ana", "Origin": "https://status.lab.juanmonsalve.dev"}, http.StatusForbidden, true},
		{"delete without Origin", http.MethodDelete, item,
			map[string]string{"Remote-User": "ana"}, http.StatusForbidden, true},
		{"PUT from another *.lab site", http.MethodPut, item + "/done",
			map[string]string{"Remote-User": "ana", "Origin": "https://status.lab.juanmonsalve.dev"}, http.StatusForbidden, true},
		{"CSRF runs before the 405", http.MethodPost, "/api/users",
			map[string]string{"Remote-User": "ana"}, http.StatusForbidden, true},
		{"a write to an unknown path is checked too", http.MethodPost, "/api/nope",
			map[string]string{"Remote-User": "ana", "Origin": "https://evil.example"}, http.StatusForbidden, true},
		{"auth runs before CSRF", http.MethodPost, "/api/items",
			map[string]string{"Origin": "https://evil.example"}, http.StatusUnauthorized, false},
		{"no Remote-User on the users API", http.MethodGet, "/api/users", nil, http.StatusUnauthorized, false},
		{"no Remote-User on /api/me", http.MethodGet, "/api/me", nil, http.StatusUnauthorized, false},
		{"/healthz needs no login", http.MethodGet, "/healthz", nil, http.StatusOK, false},
		{"/healthz ignores Remote-User", http.MethodGet, "/healthz", map[string]string{"Remote-User": "ana"}, http.StatusOK, false},
		{"/healthz rejects other methods without auth", http.MethodPost, "/healthz", nil, http.StatusMethodNotAllowed, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// DEV_REMOTE_USER is set here, but production mode must ignore it.
			cfg, err := config.Load(func(key string) string {
				return map[string]string{
					"DATABASE_URL":    "postgres://app:dev@localhost:5432/app",
					"APP_URL":         testAppURL,
					"DEV_REMOTE_USER": "ana",
				}[key]
			})
			if err != nil {
				t.Fatal(err)
			}
			users := &fakeUsers{}
			h := NewHandler(Deps{Name: "App", Logger: discardLogger(), DB: fakeDB{}, Users: users, AppURL: cfg.AppURL, DevUser: cfg.DevUser}, itemRoutes)

			req := httptest.NewRequest(tt.method, tt.path, nil)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body %q)", rec.Code, tt.wantStatus, rec.Body)
			}
			if got := users.calls > 0; got != tt.wantUpsert {
				t.Errorf("upserted the user = %v, want %v", got, tt.wantUpsert)
			}
			if rec.Code >= 400 && rec.Header().Get("Content-Type") != "application/json" {
				t.Errorf("Content-Type = %q, want a JSON error", rec.Header().Get("Content-Type"))
			}
			if rec.Code == http.StatusMethodNotAllowed && rec.Header().Get("Allow") != "GET, HEAD" {
				t.Errorf("Allow = %q, want GET, HEAD", rec.Header().Get("Allow"))
			}
			if rec.Header().Get("Content-Security-Policy") == "" {
				t.Error("no security headers on the response")
			}
		})
	}
}

func TestNewHandlerDevFallback(t *testing.T) {
	users := &fakeUsers{}
	h := NewHandler(Deps{
		Logger:  discardLogger(),
		DB:      fakeDB{},
		Users:   users,
		AppURL:  "http://localhost:5173",
		DevUser: &config.DevUser{Username: "ana"},
	}, itemRoutes)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/items", nil))
	if rec.Code != http.StatusOK || users.calls != 1 || !strings.Contains(rec.Body.String(), `"user":"ana"`) {
		t.Errorf("response = %d %q, upserts = %d; want 200 as ana after one upsert", rec.Code, rec.Body, users.calls)
	}
}

func TestNewHandlerWithoutRoutes(t *testing.T) {
	h := NewHandler(Deps{Logger: discardLogger(), DB: fakeDB{}, Users: &fakeUsers{}, AppURL: testAppURL}, nil)
	for path, want := range map[string]int{"/api/me": http.StatusOK, "/api/users": http.StatusOK, "/api/items": http.StatusNotFound} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Remote-User", "ana")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
	}
}

func TestNewHandlerContentSecurityPolicy(t *testing.T) {
	for _, tt := range []struct{ override, want string }{
		{"", DefaultContentSecurityPolicy},
		{"default-src 'self'; img-src 'self' https://tiles.example", "default-src 'self'; img-src 'self' https://tiles.example"},
	} {
		h := NewHandler(Deps{Logger: discardLogger(), DB: fakeDB{}, Users: &fakeUsers{}, AppURL: testAppURL, ContentSecurityPolicy: tt.override}, nil)
		// /healthz, the 401 and the routes all get it.
		for _, path := range []string{"/healthz", "/api/me"} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if got := rec.Header().Get("Content-Security-Policy"); got != tt.want {
				t.Errorf("override %q: %s CSP = %q, want %q", tt.override, path, got, tt.want)
			}
		}
	}
}

func TestAPIRoutes(t *testing.T) {
	const item = "/api/items/019a7000-0000-7000-8000-000000000001"
	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantAllow  string
	}{
		{"an app route", http.MethodGet, "/api/items", http.StatusOK, ""},
		{"an app write", http.MethodDelete, item, http.StatusNoContent, ""},
		{"an app route's 405", http.MethodPut, "/api/items", http.StatusMethodNotAllowed, "GET, HEAD, POST"},
		{"list users", http.MethodGet, "/api/users", http.StatusOK, ""},
		{"HEAD list users", http.MethodHead, "/api/users", http.StatusOK, ""},
		{"other methods on /api/users", http.MethodPost, "/api/users", http.StatusMethodNotAllowed, "GET, HEAD"},
		{"a user by id isn't a route", http.MethodGet, "/api/users/019a6b1e-0000-7000-8000-000000000002", http.StatusNotFound, ""},
		{"who I am", http.MethodGet, "/api/me", http.StatusOK, ""},
		{"HEAD who I am", http.MethodHead, "/api/me", http.StatusOK, ""},
		{"other methods on /api/me", http.MethodPatch, "/api/me", http.StatusMethodNotAllowed, "GET, HEAD"},
		{"nothing under /api/me", http.MethodGet, "/api/me/lists", http.StatusNotFound, ""},
		{"unknown path under an item", http.MethodGet, item + "/nope", http.StatusNotFound, ""},
		{"/api/items/ isn't an item", http.MethodGet, "/api/items/", http.StatusNotFound, ""},
		{"unknown API path", http.MethodGet, "/api/nope", http.StatusNotFound, ""},
		{"API root", http.MethodGet, "/api/", http.StatusNotFound, ""},
		{"API root without a slash isn't redirected", http.MethodGet, "/api", http.StatusNotFound, ""},
		{"unknown write", http.MethodPost, "/api/nope", http.StatusNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestHandler(discardLogger(), fakeDB{}, &fakeUsers{})
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.Header.Set("Remote-User", "ana")
			req.Header.Set("Origin", testAppURL)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body %q)", rec.Code, tt.wantStatus, rec.Body)
			}
			if ct := rec.Header().Get("Content-Type"); tt.wantStatus != http.StatusNoContent && ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			if got := rec.Header().Get("Allow"); got != tt.wantAllow {
				t.Errorf("Allow = %q, want %q", got, tt.wantAllow)
			}
			if tt.wantStatus == http.StatusNotFound {
				if want := `{"error":{"code":"not_found","message":"No such API route"}}` + "\n"; rec.Body.String() != want {
					t.Errorf("body = %q, want %q", rec.Body, want)
				}
			}
		})
	}
}

// TestSPA checks the SPA's place in the wiring; package spa tests the file serving itself.
func TestSPA(t *testing.T) {
	files := fstest.MapFS{
		"index.html":           {Data: []byte("<!doctype html><title>App</title>")},
		"assets/index-abc.js":  {Data: []byte("console.log('app')")},
		"manifest.webmanifest": {Data: []byte(`{"name":"App"}`)},
	}
	tests := []struct {
		name       string
		web        fstest.MapFS
		method     string
		path       string
		loggedIn   bool
		wantStatus int
		wantBody   string
	}{
		{"index", files, http.MethodGet, "/", true, http.StatusOK, "<!doctype html>"},
		{"SPA route", files, http.MethodGet, "/some/page", true, http.StatusOK, "<!doctype html>"},
		{"asset", files, http.MethodGet, "/assets/index-abc.js", true, http.StatusOK, "console.log"},
		{"manifest needs a login too", files, http.MethodGet, "/manifest.webmanifest", false, http.StatusUnauthorized, `"unauthenticated"`},
		{"index needs a login", files, http.MethodGet, "/", false, http.StatusUnauthorized, `"unauthenticated"`},
		{"unknown API path is never the SPA", files, http.MethodGet, "/api/nope", true, http.StatusNotFound, `"not_found"`},
		{"/healthz is never the SPA", files, http.MethodGet, "/healthz", false, http.StatusOK, `{"status":"ok"}`},
		{"writes to the SPA", files, http.MethodPost, "/", true, http.StatusMethodNotAllowed, `"method_not_allowed"`},
		{"no frontend: the notice, with the app's name", nil, http.MethodGet, "/", true, http.StatusOK, "<title>App: frontend not embedded</title>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Deps{Name: "App", Logger: discardLogger(), DB: fakeDB{}, Users: &fakeUsers{}, AppURL: testAppURL}
			if tt.web != nil {
				d.Web = tt.web
			}
			h := NewHandler(d, itemRoutes)
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.Header.Set("Origin", testAppURL)
			if tt.loggedIn {
				req.Header.Set("Remote-User", "ana")
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus || !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("got %d %q, want %d containing %q", rec.Code, rec.Body, tt.wantStatus, tt.wantBody)
			}
			if rec.Header().Get("Content-Security-Policy") == "" {
				t.Error("no security headers on the response")
			}
		})
	}
}

func TestHealthz(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		dbErr      error
		wantStatus int
		wantBody   string
	}{
		{"GET healthy", http.MethodGet, nil, http.StatusOK, "{\"status\":\"ok\"}\n"},
		{"GET database down", http.MethodGet, errors.New("connection refused"), http.StatusServiceUnavailable, "{\"status\":\"unavailable\"}\n"},
		{"HEAD healthy", http.MethodHead, nil, http.StatusOK, ""},
		{"HEAD database down", http.MethodHead, errors.New("connection refused"), http.StatusServiceUnavailable, ""},
		{"POST not allowed", http.MethodPost, nil, http.StatusMethodNotAllowed, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A real server, so HEAD gets net/http's real behaviour (headers, no body).
			srv := httptest.NewServer(newTestHandler(discardLogger(), fakeDB{err: tt.dbErr}, &fakeUsers{}))
			t.Cleanup(srv.Close)

			// No Remote-User header: /healthz must work without a login.
			req, err := http.NewRequestWithContext(t.Context(), tt.method, srv.URL+"/healthz", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if tt.wantBody != "" && string(body) != tt.wantBody {
				t.Errorf("body = %q, want %q", body, tt.wantBody)
			}
			if tt.method == http.MethodHead && len(body) != 0 {
				t.Errorf("HEAD body = %q, want none", body)
			}
		})
	}
}

// startServer runs s.Serve on a loopback port and returns its base URL, a cancel func that
// triggers shutdown, and a channel that receives Serve's result.
func startServer(t *testing.T, s *Server) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	return "http://" + ln.Addr().String(), cancel, done
}

// waitUntilRefused waits until the server stops accepting connections, which Shutdown does first.
func waitUntilRefused(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			return
		}
		_ = conn.Close()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server still accepting connections after shutdown started")
}

func TestServeShutsDownGracefully(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	s := New(0, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, "done")
	}), discardLogger())
	baseURL, cancel, done := startServer(t, s)

	type result struct {
		body string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.Get(baseURL + "/slow")
		if err != nil {
			got <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		got <- result{string(b), err}
	}()

	<-entered
	cancel()
	waitUntilRefused(t, strings.TrimPrefix(baseURL, "http://"))

	select {
	case err := <-done:
		t.Fatalf("Serve returned %v while a request was still in flight", err)
	default:
	}

	close(release)
	if r := <-got; r.err != nil || r.body != "done" {
		t.Errorf("in-flight request = %q, %v; want it to complete", r.body, r.err)
	}
	if err := <-done; err != nil {
		t.Errorf("Serve() = %v, want nil", err)
	}
}

func TestServeShutdownTimeout(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	s := New(0, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	}), discardLogger())
	s.shutdownTimeout = 50 * time.Millisecond
	baseURL, cancel, done := startServer(t, s)

	clientErr := make(chan error, 1)
	go func() {
		resp, err := http.Get(baseURL + "/stuck")
		if err == nil {
			_ = resp.Body.Close()
		}
		clientErr <- err
	}()

	<-entered
	cancel()
	start := time.Now()
	err := <-done
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Serve() = %v, want a context.DeadlineExceeded error", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Serve took %v to give up, want about the shutdown timeout", elapsed)
	}
	if err := <-clientErr; err == nil {
		t.Error("stuck request succeeded, want its connection closed")
	}
}

func TestListenAndServeAddressInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	s := New(ln.Addr().(*net.TCPAddr).Port, http.NotFoundHandler(), discardLogger())
	if err := s.ListenAndServe(t.Context()); err == nil || !strings.Contains(err.Error(), "listen on 0.0.0.0:") {
		t.Errorf("ListenAndServe() = %v, want a listen error", err)
	}
}
