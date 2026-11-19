// Package auth identifies the user behind each request from the Remote-* headers Authelia adds
// (through Traefik's forward-auth), records them through the app's UserStore and puts them in the
// request context (README.md §5). The app has no login of its own.
//
// The headers can be trusted because Traefik replaces any Remote-* header a browser sends with
// Authelia's values and the app publishes no ports. That holds only while the app's router keeps
// its authelia@docker middleware label.
package auth

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/juanmonsalveg/homelab-go/config"
	"github.com/juanmonsalveg/homelab-go/httpx"
)

// The headers Authelia sets on every request it lets through. Remote-Groups isn't read yet.
const (
	headerUser  = "Remote-User"
	headerName  = "Remote-Name"
	headerEmail = "Remote-Email"
)

// Identity is who Authelia says the request is from, or the development stand-in.
type Identity struct {
	Username    string
	DisplayName string
	Email       string
}

// User is the logged-in user, as the app's users table has them.
type User struct {
	ID          uuid.UUID
	Username    string
	DisplayName string
	Email       string
}

// Person is someone else who has opened the app, as GET /api/users lists them. It has no email.
type Person struct {
	ID          uuid.UUID
	Username    string
	DisplayName string
}

// UserStore keeps the people who have opened the app, in the app's own users table. The template's
// db/queries/users.sql has the two queries behind it (README.md §5).
type UserStore interface {
	// UpsertUser records the user behind a request, refreshing their name and email, and returns
	// their row. It runs on every request behind Middleware, so it must be one statement that two
	// first requests for the same username can run at once.
	UpsertUser(ctx context.Context, id Identity) (User, error)
	// OtherUsers returns everyone in the users table except userID, by display name and then
	// username.
	OtherUsers(ctx context.Context, userID uuid.UUID) ([]Person, error)
}

// Middleware requires a logged-in user on every request it wraps. It reads the user from the
// Remote-* headers, or from dev when Remote-User is missing, and answers 401 when there's neither.
// It then upserts the user (the one write a GET may cause, which the homelab requires) and stores
// the row in the context, where FromContext finds it.
//
// dev is config.Base.DevUser, which config.Load sets only when APP_ENV=development, so production
// never falls back.
func Middleware(users UserStore, dev *config.DevUser, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := identify(r, dev)
			if !ok {
				httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "Not logged in")
				return
			}
			ctx := r.Context()
			httpx.AddLogAttrs(ctx, slog.String("user", id.Username))

			user, err := users.UpsertUser(ctx, id)
			if err != nil {
				logger.LogAttrs(ctx, slog.LevelError, "upsert user failed",
					slog.String("user", id.Username), slog.Any("error", err))
				httpx.WriteError(w, http.StatusInternalServerError, "internal", "Internal server error")
				return
			}
			next.ServeHTTP(w, r.WithContext(NewContext(ctx, user)))
		})
	}
}

// identify returns the request's user from Authelia's headers, falling back to dev when the request
// has no Remote-User. A Remote-User of only whitespace counts as missing.
func identify(r *http.Request, dev *config.DevUser) (Identity, bool) {
	if username := strings.TrimSpace(r.Header.Get(headerUser)); username != "" {
		return Identity{
			Username:    username,
			DisplayName: strings.TrimSpace(r.Header.Get(headerName)),
			Email:       strings.TrimSpace(r.Header.Get(headerEmail)),
		}, true
	}
	if dev != nil && dev.Username != "" {
		return Identity{Username: dev.Username, DisplayName: dev.DisplayName, Email: dev.Email}, true
	}
	return Identity{}, false
}

type contextKey struct{}

// NewContext returns a copy of ctx that carries u. Middleware uses it; handler tests use it to
// stand in for a logged-in user.
func NewContext(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, contextKey{}, u)
}

// FromContext returns the user Middleware stored in ctx. ok is false outside Middleware.
func FromContext(ctx context.Context) (u User, ok bool) {
	u, ok = ctx.Value(contextKey{}).(User)
	return u, ok
}

// RequireUser returns the request's user for a handler behind Middleware. Without one, the handler
// was mounted outside Middleware (a wiring bug), so it logs, answers 500 and returns false, and
// the handler should return.
func RequireUser(w http.ResponseWriter, r *http.Request, logger *slog.Logger) (User, bool) {
	u, ok := FromContext(r.Context())
	if !ok {
		logger.LogAttrs(r.Context(), slog.LevelError, "handler reached without a logged-in user",
			slog.String("path", r.URL.Path))
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "Internal server error")
	}
	return u, ok
}

// personJSON is a person in GET /api/me and GET /api/users. The email stays out of both: the UI
// doesn't need it.
type personJSON struct {
	ID          uuid.UUID `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"displayName"`
}

type usersResponse struct {
	Users []personJSON `json:"users"`
}

// MeHandler answers GET /api/me with the logged-in user: {"id","username","displayName"}. The
// shell shows the name, and an app uses the id wherever the UI needs to know which person is the
// caller.
func MeHandler(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := RequireUser(w, r, logger)
		if !ok {
			return
		}
		httpx.WriteJSON(w, http.StatusOK, personJSON{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName})
	}
}

// UsersHandler answers GET /api/users with everyone who has opened the app except the caller, by
// display name: {"users":[{"id","username","displayName"}]}. In an app the household shares, these
// are the people something can be shared with or belong to.
func UsersHandler(users UserStore, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := RequireUser(w, r, logger)
		if !ok {
			return
		}
		people, err := users.OtherUsers(r.Context(), u.ID)
		if err != nil {
			logger.LogAttrs(r.Context(), slog.LevelError, "list users failed", slog.Any("error", err))
			httpx.WriteError(w, http.StatusInternalServerError, "internal", "Internal server error")
			return
		}
		resp := usersResponse{Users: make([]personJSON, 0, len(people))}
		for _, p := range people {
			resp.Users = append(resp.Users, personJSON(p))
		}
		httpx.WriteJSON(w, http.StatusOK, resp)
	}
}
