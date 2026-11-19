package auth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/juanmonsalveg/homelab-go/config"
	"github.com/juanmonsalveg/homelab-go/httpx"
)

var (
	testID  = uuid.MustParse("019a6b1e-0000-7000-8000-000000000001")
	otherID = uuid.MustParse("019a6b1e-0000-7000-8000-000000000002")
)

// fakeUsers records each call. UpsertUser answers with testID and OtherUsers with people, or both
// with err when it's set.
type fakeUsers struct {
	calls     []Identity
	people    []Person
	othersFor []uuid.UUID
	err       error
}

func (f *fakeUsers) UpsertUser(_ context.Context, id Identity) (User, error) {
	f.calls = append(f.calls, id)
	if f.err != nil {
		return User{}, f.err
	}
	return User{ID: testID, Username: id.Username, DisplayName: id.DisplayName, Email: id.Email}, nil
}

func (f *fakeUsers) OtherUsers(_ context.Context, userID uuid.UUID) ([]Person, error) {
	f.othersFor = append(f.othersFor, userID)
	return f.people, f.err
}

type result struct {
	rec    *httptest.ResponseRecorder
	logs   string
	called bool // whether the wrapped handler ran
	user   User // what it found in the context
	hasCtx bool
}

// serve sends one GET with the given headers through LogRequests and Middleware.
func serve(t *testing.T, users UserStore, dev *config.DevUser, headers map[string]string) result {
	t.Helper()
	var res result
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		res.called = true
		res.user, res.hasCtx = FromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	h := httpx.LogRequests(logger)(Middleware(users, dev, logger)(next))

	req := httptest.NewRequest(http.MethodGet, "/api/items", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res.rec = httptest.NewRecorder()
	h.ServeHTTP(res.rec, req)
	res.logs = logs.String()
	return res
}

func TestMiddlewareReadsHeaders(t *testing.T) {
	users := &fakeUsers{}
	res := serve(t, users, nil, map[string]string{
		"Remote-User":  "luis",
		"Remote-Name":  " Luis ",
		"Remote-Email": "luis@example.com",
	})

	if res.rec.Code != http.StatusNoContent || !res.called {
		t.Fatalf("status = %d, handler called = %v; want the request passed through", res.rec.Code, res.called)
	}
	wantID := Identity{Username: "luis", DisplayName: "Luis", Email: "luis@example.com"}
	if len(users.calls) != 1 || users.calls[0] != wantID {
		t.Errorf("upserts = %+v, want one with %+v", users.calls, wantID)
	}
	wantUser := User{ID: testID, Username: "luis", DisplayName: "Luis", Email: "luis@example.com"}
	if !res.hasCtx || res.user != wantUser {
		t.Errorf("context user = %+v (set: %v), want %+v", res.user, res.hasCtx, wantUser)
	}
	if !strings.Contains(res.logs, `"user":"luis"`) {
		t.Errorf("request log = %s, want the username", res.logs)
	}
}

func TestMiddlewareRequiresRemoteUser(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
	}{
		{"no headers", nil},
		{"only name and email", map[string]string{"Remote-Name": "Ana", "Remote-Email": "j@example.com"}},
		{"blank Remote-User", map[string]string{"Remote-User": "  "}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			users := &fakeUsers{}
			res := serve(t, users, nil, tt.headers)

			if res.rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", res.rec.Code)
			}
			if want := `{"error":{"code":"unauthenticated","message":"Not logged in"}}` + "\n"; res.rec.Body.String() != want {
				t.Errorf("body = %q, want %q", res.rec.Body.String(), want)
			}
			if res.called || len(users.calls) != 0 {
				t.Errorf("handler called = %v, upserts = %d; want neither", res.called, len(users.calls))
			}
		})
	}
}

// The dev fallback goes through config.Load, which decides when DEV_REMOTE_* count.
func TestMiddlewareDevFallback(t *testing.T) {
	devVars := map[string]string{
		"DEV_REMOTE_USER":  "ana",
		"DEV_REMOTE_NAME":  "Ana",
		"DEV_REMOTE_EMAIL": "ana@example.com",
	}
	tests := []struct {
		name     string
		appEnv   string
		devVars  map[string]string
		headers  map[string]string
		wantUser string // "" means 401
	}{
		{"development uses DEV_REMOTE_USER", "development", devVars, nil, "ana"},
		{"development prefers the header", "development", devVars, map[string]string{"Remote-User": "luis"}, "luis"},
		{"development without DEV_REMOTE_USER", "development", nil, nil, ""},
		{"production ignores DEV_REMOTE_USER", "production", devVars, nil, ""},
		{"unset APP_ENV is production", "", devVars, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars := map[string]string{
				"DATABASE_URL": "postgres://app:dev@localhost:5432/app",
				"APP_URL":      "http://localhost:5173",
				"APP_ENV":      tt.appEnv,
			}
			for k, v := range tt.devVars {
				vars[k] = v
			}
			cfg, err := config.Load(func(key string) string { return vars[key] })
			if err != nil {
				t.Fatal(err)
			}

			res := serve(t, &fakeUsers{}, cfg.DevUser, tt.headers)
			if tt.wantUser == "" {
				if res.rec.Code != http.StatusUnauthorized || res.called {
					t.Errorf("status = %d, handler called = %v; want 401", res.rec.Code, res.called)
				}
				return
			}
			if res.rec.Code != http.StatusNoContent || res.user.Username != tt.wantUser {
				t.Errorf("status = %d, user = %+v; want the request to run as %s", res.rec.Code, res.user, tt.wantUser)
			}
		})
	}
}

func TestMiddlewareDevFallbackUsesAllFields(t *testing.T) {
	users := &fakeUsers{}
	dev := &config.DevUser{Username: "ana", DisplayName: "Ana", Email: "ana@example.com"}
	serve(t, users, dev, nil)

	want := Identity{Username: "ana", DisplayName: "Ana", Email: "ana@example.com"}
	if len(users.calls) != 1 || users.calls[0] != want {
		t.Errorf("upserts = %+v, want one with %+v", users.calls, want)
	}
}

func TestMiddlewareUpsertFails(t *testing.T) {
	res := serve(t, &fakeUsers{err: errors.New("connection refused")}, nil, map[string]string{"Remote-User": "ana"})

	if res.rec.Code != http.StatusInternalServerError || res.called {
		t.Errorf("status = %d, handler called = %v; want 500 without calling the handler", res.rec.Code, res.called)
	}
	if want := `{"error":{"code":"internal","message":"Internal server error"}}` + "\n"; res.rec.Body.String() != want {
		t.Errorf("body = %q, want %q", res.rec.Body.String(), want)
	}
	for _, want := range []string{`"msg":"upsert user failed"`, "connection refused", `"msg":"request"`, `"user":"ana"`} {
		if !strings.Contains(res.logs, want) {
			t.Errorf("logs = %s, want them to contain %s", res.logs, want)
		}
	}
}

func TestFromContextWithoutUser(t *testing.T) {
	if u, ok := FromContext(t.Context()); ok {
		t.Errorf("FromContext() = %+v, true; want false", u)
	}
}

func TestNewContext(t *testing.T) {
	want := User{ID: testID, Username: "ana"}
	if got, ok := FromContext(NewContext(t.Context(), want)); !ok || got != want {
		t.Errorf("FromContext(NewContext(u)) = %+v, %v; want %+v, true", got, ok, want)
	}
}

func TestRequireUser(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	req := httptest.NewRequest(http.MethodGet, "/api/lists", nil)

	want := User{ID: testID, Username: "ana"}
	rec := httptest.NewRecorder()
	got, ok := RequireUser(rec, req.WithContext(NewContext(req.Context(), want)), logger)
	if !ok || got != want || rec.Body.Len() != 0 {
		t.Errorf("with a user: RequireUser = %+v, %v, response %q; want the user and no response", got, ok, rec.Body)
	}

	rec = httptest.NewRecorder()
	if _, ok := RequireUser(rec, req, logger); ok {
		t.Error("without a user: RequireUser returned ok")
	}
	if want := `{"error":{"code":"internal","message":"Internal server error"}}` + "\n"; rec.Code != http.StatusInternalServerError || rec.Body.String() != want {
		t.Errorf("without a user: response = %d %q, want 500 %q", rec.Code, rec.Body, want)
	}
	if !strings.Contains(logs.String(), `"level":"ERROR"`) || !strings.Contains(logs.String(), `"path":"/api/lists"`) {
		t.Errorf("logs = %s, want an error naming the path", logs.String())
	}
}

func TestMeHandler(t *testing.T) {
	var logs bytes.Buffer
	h := MeHandler(slog.New(slog.NewJSONHandler(&logs, nil)))
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)

	u := User{ID: testID, Username: "ana", DisplayName: "Ana", Email: "ana@example.com"}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(NewContext(req.Context(), u)))
	// No email: the UI doesn't need it.
	want := `{"id":"019a6b1e-0000-7000-8000-000000000001","username":"ana","displayName":"Ana"}` + "\n"
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Errorf("with a user: response = %d %q, want 200 %q", rec.Code, rec.Body, want)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("without a user: status = %d, want 500", rec.Code)
	}
}

func TestUsersHandler(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	req := httptest.NewRequest(http.MethodGet, "/api/users", nil)
	req = req.WithContext(NewContext(req.Context(), User{ID: testID, Username: "ana"}))

	users := &fakeUsers{people: []Person{
		{ID: otherID, Username: "luis", DisplayName: "Luis"},
		{ID: uuid.MustParse("019a7000-0000-7000-8000-00000000000b"), Username: "guest", DisplayName: ""},
	}}
	rec := httptest.NewRecorder()
	UsersHandler(users, logger).ServeHTTP(rec, req)
	// No email, as in GET /api/me.
	want := `{"users":[` +
		`{"id":"019a6b1e-0000-7000-8000-000000000002","username":"luis","displayName":"Luis"},` +
		`{"id":"019a7000-0000-7000-8000-00000000000b","username":"guest","displayName":""}]}` + "\n"
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Errorf("response = %d %q, want 200 %q", rec.Code, rec.Body, want)
	}
	if len(users.othersFor) != 1 || users.othersFor[0] != testID {
		t.Errorf("OtherUsers calls = %v, want one for the caller", users.othersFor)
	}

	// Nobody else yet is an empty array, not null.
	rec = httptest.NewRecorder()
	UsersHandler(&fakeUsers{}, logger).ServeHTTP(rec, req)
	if want := `{"users":[]}` + "\n"; rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Errorf("no one else: response = %d %q, want 200 %q", rec.Code, rec.Body, want)
	}

	rec = httptest.NewRecorder()
	UsersHandler(&fakeUsers{err: errors.New("connection refused")}, logger).ServeHTTP(rec, req)
	if want := `{"error":{"code":"internal","message":"Internal server error"}}` + "\n"; rec.Code != http.StatusInternalServerError || rec.Body.String() != want {
		t.Errorf("store failing: response = %d %q, want 500 %q", rec.Code, rec.Body, want)
	}
	if !strings.Contains(logs.String(), `"msg":"list users failed"`) || !strings.Contains(logs.String(), "connection refused") {
		t.Errorf("logs = %s, want the store's error", logs.String())
	}

	rec = httptest.NewRecorder()
	UsersHandler(users, logger).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/users", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("without a user: status = %d, want 500", rec.Code)
	}
}
