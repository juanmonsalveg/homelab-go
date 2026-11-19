// Package app is a homelab app's main function (README.md §2, §8). An app describes itself in a
// Spec, and Main gives its binary the subcommands every app has: serve, which loads the config,
// connects to Postgres, runs the migrations and serves HTTP until SIGTERM; migrate; and
// healthcheck, for the container's healthcheck.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juanmonsalveg/homelab-go/auth"
	"github.com/juanmonsalveg/homelab-go/config"
	"github.com/juanmonsalveg/homelab-go/database"
	"github.com/juanmonsalveg/homelab-go/server"
)

// Spec is what an app tells Main about itself.
type Spec struct {
	// Name is the app's name, lowercase: the binary and compose service (`todo`), and what the
	// usage text and the not-embedded page call it. Required.
	Name string
	// Migrations are the app's goose migrations, rooted at its db/migrations directory. The app
	// embeds them itself, since go:embed only reaches files under the embedding package. Required.
	Migrations fs.FS
	// Web is the app's Vite build: nil in a binary built without it (no embedweb tag).
	Web fs.FS
	// Users returns the store auth records users in, on the app's pool. Required.
	Users func(pool *pgxpool.Pool) auth.UserStore
	// Routes registers the app's own routes. Every one runs behind auth and CSRF, next to GET
	// /api/me, GET /api/users, the JSON 404 for other /api paths and the SPA, which an app doesn't
	// register itself (server.NewHandler). nil means the app has no routes of its own yet.
	Routes func(mux *http.ServeMux, d Deps)
	// ContentSecurityPolicy replaces server.DefaultContentSecurityPolicy when it isn't empty.
	ContentSecurityPolicy string
}

// Deps is what Routes gets to build the app's handlers with.
type Deps struct {
	// Pool is the app's database pool. Main closes it after the server stops.
	Pool   *pgxpool.Pool
	Logger *slog.Logger
	Config config.Base
}

// Main runs the subcommand in os.Args and returns the exit code: 0 on success, 1 on failure and 2
// for a usage error. An app's main function is `os.Exit(app.Main(spec))`.
func Main(spec Spec) int {
	return run(context.Background(), spec, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
}

const usageText = `Usage: %[1]s [command]

Commands:
  serve                Run the HTTP server (the default)
  migrate [up|status]  Apply pending migrations (the default) or list their status
  healthcheck          Exit 0 if this container's /healthz answers 200, otherwise 1

Configuration comes from environment variables: see .env.example and ARCHITECTURE.md §5.
`

func run(ctx context.Context, spec Spec, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if err := spec.validate(); err != nil {
		_, _ = fmt.Fprintf(stderr, "app: %v\n", err)
		return 1
	}
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}

	switch cmd {
	case "serve":
		if len(args) > 0 {
			return usageError(spec, stderr, "serve takes no arguments")
		}
		return serve(ctx, spec, getenv, stderr)
	case "migrate":
		return migrate(ctx, spec, args, getenv, stdout, stderr)
	case "healthcheck":
		if len(args) > 0 {
			return usageError(spec, stderr, "healthcheck takes no arguments")
		}
		return healthcheck(ctx, spec.Name, getenv, stderr)
	case "help", "-h", "-help", "--help":
		_, _ = fmt.Fprintf(stdout, usageText, spec.Name)
		return 0
	default:
		return usageError(spec, stderr, fmt.Sprintf("unknown command %q", cmd))
	}
}

// validate reports a Spec missing a required field: a wiring bug, found by the first run.
func (s Spec) validate() error {
	var problems []error
	if s.Name == "" {
		problems = append(problems, errors.New("Spec.Name is required"))
	}
	if s.Migrations == nil {
		problems = append(problems, errors.New("Spec.Migrations is required"))
	}
	if s.Users == nil {
		problems = append(problems, errors.New("Spec.Users is required"))
	}
	return errors.Join(problems...)
}

func usageError(spec Spec, stderr io.Writer, msg string) int {
	_, _ = fmt.Fprintf(stderr, "%s: %s\n\n", spec.Name, msg)
	_, _ = fmt.Fprintf(stderr, usageText, spec.Name)
	return 2
}

// serve runs the startup sequence (README.md §4: config, database, migrations, HTTP) and then
// serves until SIGTERM or SIGINT.
func serve(ctx context.Context, spec Spec, getenv func(string) string, stderr io.Writer) int {
	cfg, err := loadConfig(spec.Name, getenv, stderr)
	if err != nil {
		return 1
	}
	logger := newLogger(stderr, cfg)
	ctx, stop := notifyContext(ctx)
	defer stop()

	logger.Info("starting", slog.String("app", spec.Name), slog.String("env", string(cfg.Env)), slog.String("app_url", cfg.AppURL))
	if cfg.DevUser != nil {
		logger.Warn("development login fallback active: requests without Remote-User run as DEV_REMOTE_USER",
			slog.String("user", cfg.DevUser.Username))
	}
	pool, err := connect(ctx, cfg, logger)
	if err != nil {
		return startupFailed(ctx, logger, err)
	}
	if cfg.MigrateOnStart {
		if err := database.Migrate(ctx, pool, spec.Migrations, logger); err != nil {
			pool.Close()
			return startupFailed(ctx, logger, err)
		}
	}

	var routes func(*http.ServeMux)
	if spec.Routes != nil {
		d := Deps{Pool: pool, Logger: logger, Config: cfg}
		routes = func(mux *http.ServeMux) { spec.Routes(mux, d) }
	}
	handler := server.NewHandler(server.Deps{
		Name:                  spec.Name,
		Logger:                logger,
		DB:                    pool,
		Users:                 spec.Users(pool),
		AppURL:                cfg.AppURL,
		DevUser:               cfg.DevUser,
		Web:                   spec.Web,
		ContentSecurityPolicy: spec.ContentSecurityPolicy,
	}, routes)
	srv := server.New(cfg.Port, handler, logger)
	err = srv.ListenAndServe(ctx)
	pool.Close()
	if err != nil {
		logger.Error("server stopped with an error", slog.Any("error", err))
		return 1
	}
	logger.Info("stopped")
	return 0
}

// startupFailed reports a failed startup step. A signal during startup (say, `docker stop` while
// waiting for Postgres) is a requested stop, not a failure.
func startupFailed(ctx context.Context, logger *slog.Logger, err error) int {
	if ctx.Err() != nil {
		logger.Info("stopped before startup finished", slog.Any("reason", context.Cause(ctx)))
		return 0
	}
	logger.Error("startup failed", slog.Any("error", err))
	return 1
}

// migrate applies the pending migrations (up) or prints every migration's state (status), as
// `docker compose run --rm <app> migrate status` on the server.
func migrate(ctx context.Context, spec Spec, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	action := "up"
	if len(args) > 0 {
		action = args[0]
	}
	if len(args) > 1 || (action != "up" && action != "status") {
		return usageError(spec, stderr, "migrate takes one optional argument: up or status")
	}

	cfg, err := loadConfig(spec.Name, getenv, stderr)
	if err != nil {
		return 1
	}
	logger := newLogger(stderr, cfg)
	ctx, stop := notifyContext(ctx)
	defer stop()

	pool, err := connect(ctx, cfg, logger)
	if err != nil {
		logger.Error("migrate failed", slog.Any("error", err))
		return 1
	}
	defer pool.Close()

	if action == "status" {
		err = printMigrationStatus(ctx, pool, spec.Migrations, stdout)
	} else {
		err = database.Migrate(ctx, pool, spec.Migrations, logger)
	}
	if err != nil {
		logger.Error("migrate failed", slog.Any("error", err))
		return 1
	}
	return 0
}

func printMigrationStatus(ctx context.Context, pool *pgxpool.Pool, migrations fs.FS, stdout io.Writer) error {
	states, err := database.MigrationStatus(ctx, pool, migrations)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "VERSION\tAPPLIED AT (UTC)\tFILE")
	for _, s := range states {
		applied := "pending"
		if !s.AppliedAt.IsZero() {
			applied = s.AppliedAt.UTC().Format(time.DateTime)
		}
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\n", s.Version, applied, s.File)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("print migration status: %w", err)
	}
	return nil
}

// notifyContext is canceled by SIGTERM (`docker stop`) or SIGINT (Ctrl-C in `make dev`). Once it
// is, the default handlers come back, so a second Ctrl-C exits at once.
func notifyContext(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt)
	context.AfterFunc(ctx, stop)
	return ctx, stop
}

// connect opens the pool, waiting for Postgres for about a minute (README.md §4).
func connect(ctx context.Context, cfg config.Base, logger *slog.Logger) (*pgxpool.Pool, error) {
	poolCfg, err := database.PoolConfig(cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return nil, err
	}
	return database.Connect(ctx, poolCfg, logger)
}

// loadConfig loads the configuration and, if it's invalid, prints every problem to stderr.
// This happens before the logger exists, since the logger's format and level come from the config.
func loadConfig(name string, getenv func(string) string, stderr io.Writer) (config.Base, error) {
	cfg, err := config.Load(getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: invalid configuration:\n  - %s\n", name, strings.ReplaceAll(err.Error(), "\n", "\n  - "))
	}
	return cfg, err
}

// newLogger logs JSON in production (read with `docker compose logs`) and text in development.
func newLogger(w io.Writer, cfg config.Base) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.Env == config.Development {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}
