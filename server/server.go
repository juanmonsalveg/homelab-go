// Package server builds an app's HTTP handler around its own routes and runs the HTTP server:
// plain HTTP on 0.0.0.0:$PORT behind Traefik, with timeouts and a graceful shutdown that fits
// inside Docker's 10 s stop window (README.md §6, §8).
package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/juanmonsalveg/homelab-go/auth"
	"github.com/juanmonsalveg/homelab-go/config"
	"github.com/juanmonsalveg/homelab-go/health"
	"github.com/juanmonsalveg/homelab-go/httpx"
	"github.com/juanmonsalveg/homelab-go/spa"
)

const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 120 * time.Second
	// shutdownTimeout leaves time to close the database pool before `docker stop` kills the
	// process at 10 s.
	shutdownTimeout = 8 * time.Second
)

// Deps is what NewHandler needs from the app. app.Main fills it in.
type Deps struct {
	// Name is the app's name, for the page a binary without its frontend serves.
	Name   string
	Logger *slog.Logger
	// DB answers /healthz's SELECT 1. *pgxpool.Pool satisfies it.
	DB health.DB
	// Users records the user behind each request and lists the people for GET /api/users.
	Users auth.UserStore
	// AppURL is the origin requests that change data must come from (config.Base.AppURL).
	AppURL string
	// DevUser is config.Base.DevUser: nil unless APP_ENV=development.
	DevUser *config.DevUser
	// Web is the SPA build: nil in a binary built without it.
	Web fs.FS
	// ContentSecurityPolicy replaces DefaultContentSecurityPolicy when it isn't empty.
	ContentSecurityPolicy string
}

// NewHandler builds the app's handler. Outermost first: request log, recover, security headers,
// /healthz, auth, CSRF, then the routes (README.md §6).
//
// routes registers the app's own routes, which all run behind auth and CSRF. They go on the same
// mux as GET /api/me, GET /api/users, the JSON 404 for any other /api path and the SPA on /, so an
// app must not register those patterns itself (ServeMux panics on a conflict). routes may be nil.
func NewHandler(d Deps, routes func(mux *http.ServeMux)) http.Handler {
	// Every route except /healthz requires a logged-in user.
	app := http.NewServeMux()
	app.Handle("GET /api/me", auth.MeHandler(d.Logger))
	app.Handle("/api/me", httpx.MethodNotAllowed("GET, HEAD"))
	app.Handle("GET /api/users", auth.UsersHandler(d.Users, d.Logger))
	app.Handle("/api/users", httpx.MethodNotAllowed("GET, HEAD"))
	// Unknown API paths get a JSON 404 rather than the SPA. "/api" is registered too, or ServeMux
	// would redirect it to "/api/".
	apiNotFound := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "No such API route")
	})
	app.Handle("/api/", apiNotFound)
	app.Handle("/api", apiNotFound)
	app.Handle("/", spa.Handler(d.Name, d.Web, d.Logger))
	if routes != nil {
		routes(app)
	}

	var protected http.Handler = app
	protected = requireSameOrigin(d.AppURL)(protected)
	protected = auth.Middleware(d.Users, d.DevUser, d.Logger)(protected)

	// /healthz is mounted on the top-level mux, outside auth, so it can never require a login.
	// The GET pattern also matches HEAD (`curl -I`). Other methods get their 405 here too, instead of
	// falling through to "/" and auth.
	root := http.NewServeMux()
	root.Handle("GET /healthz", health.Handler(d.DB, d.Logger))
	root.Handle("/healthz", httpx.MethodNotAllowed("GET, HEAD"))
	root.Handle("/", protected)

	csp := d.ContentSecurityPolicy
	if csp == "" {
		csp = DefaultContentSecurityPolicy
	}
	var h http.Handler = root
	h = securityHeaders(csp)(h)
	h = httpx.Recover(d.Logger)(h)
	h = httpx.LogRequests(d.Logger)(h)
	return h
}

// Server wraps http.Server with the app's timeouts and shutdown behaviour.
type Server struct {
	srv             *http.Server
	logger          *slog.Logger
	shutdownTimeout time.Duration
}

// New returns a server for 0.0.0.0:port. Binding 127.0.0.1 instead would make Traefik answer 502.
func New(port int, handler http.Handler, logger *slog.Logger) *Server {
	return &Server{
		srv: &http.Server{
			Addr:              net.JoinHostPort("0.0.0.0", strconv.Itoa(port)),
			Handler:           handler,
			ReadHeaderTimeout: readHeaderTimeout,
			ReadTimeout:       readTimeout,
			WriteTimeout:      writeTimeout,
			IdleTimeout:       idleTimeout,
			ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		},
		logger:          logger,
		shutdownTimeout: shutdownTimeout,
	}
}

// ListenAndServe listens on the server's address and serves until ctx is done.
func (s *Server) ListenAndServe(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.srv.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.srv.Addr, err)
	}
	return s.Serve(ctx, ln)
}

// Serve serves on ln until ctx is done, then shuts down gracefully: it stops accepting
// connections and waits for in-flight requests, for at most the shutdown timeout. Requests still
// running after that have their connections closed. It returns nil after a clean shutdown.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.srv.Serve(ln) }()
	s.logger.Info("listening", slog.String("addr", ln.Addr().String()))

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	// A string, because the JSON handler would print a Duration as nanoseconds.
	s.logger.Info("shutting down", slog.String("timeout", s.shutdownTimeout.String()))
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.shutdownTimeout)
	defer cancel()

	var err error
	if shutdownErr := s.srv.Shutdown(shutdownCtx); shutdownErr != nil {
		err = fmt.Errorf("graceful shutdown: %w", shutdownErr)
		if closeErr := s.srv.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close: %w", closeErr))
		}
	}
	if e := <-serveErr; !errors.Is(e, http.ErrServerClosed) {
		err = errors.Join(err, fmt.Errorf("serve: %w", e))
	}
	return err
}
