// Package config loads and validates the settings every homelab app shares from environment
// variables (README.md §3). An app with settings of its own reads them next to these.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
)

// Env is the mode switch (APP_ENV).
type Env string

const (
	Production  Env = "production"
	Development Env = "development"
)

// MaxDBMaxConns is the hard cap on DB_MAX_CONNS: the server's 100 Postgres connections are shared
// by every app. Package database enforces it too.
const MaxDBMaxConns = 10

const (
	defaultPort       = 3000
	defaultDBMaxConns = 5
)

// Base holds the validated shared settings. Load is the only way to build one.
type Base struct {
	// DatabaseURL is DATABASE_URL as given. It contains the password: never log it.
	DatabaseURL string
	// AppURL is the app's origin with no trailing slash, e.g. https://todo.lab.juanmonsalve.dev.
	// The CSRF check compares Origin headers against it, and absolute URLs are built from it.
	AppURL         string
	Port           int
	Env            Env
	DBMaxConns     int
	MigrateOnStart bool
	LogLevel       slog.Level
	// DevUser stands in for Authelia's Remote-* headers. Load sets it only when Env is Development
	// and DEV_REMOTE_USER is set, so production ignores the DEV_REMOTE_* variables.
	DevUser *DevUser
}

// DevUser is the identity used in development when a request has no Remote-User header.
type DevUser struct {
	Username    string
	DisplayName string
	Email       string
}

// Load reads the configuration through getenv (os.Getenv in production).
// It reports every invalid or missing variable at once, one per line.
func Load(getenv func(string) string) (Base, error) {
	get := func(key string) string { return strings.TrimSpace(getenv(key)) }

	var problems []error
	cfg := Base{
		DBMaxConns:     defaultDBMaxConns,
		MigrateOnStart: true,
		LogLevel:       slog.LevelInfo,
	}

	var err error
	if cfg.DatabaseURL, err = parseDatabaseURL(get("DATABASE_URL")); err != nil {
		problems = append(problems, err)
	}
	if cfg.AppURL, err = parseAppURL(get("APP_URL")); err != nil {
		problems = append(problems, err)
	}
	if cfg.Port, err = Port(getenv); err != nil {
		problems = append(problems, err)
	}

	switch v := get("APP_ENV"); v {
	case "", string(Production):
		cfg.Env = Production
	case string(Development):
		cfg.Env = Development
	default:
		// Fail loudly instead of guessing: a typo such as "Development" must not run as either mode.
		problems = append(problems, fmt.Errorf("APP_ENV must be %q or %q, got %q", Production, Development, v))
	}

	if v := get("DB_MAX_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxDBMaxConns {
			problems = append(problems, fmt.Errorf("DB_MAX_CONNS must be a whole number from 1 to %d, got %q", MaxDBMaxConns, v))
		} else {
			cfg.DBMaxConns = n
		}
	}

	if v := get("MIGRATE_ON_START"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			problems = append(problems, fmt.Errorf("MIGRATE_ON_START must be true or false, got %q", v))
		} else {
			cfg.MigrateOnStart = b
		}
	}

	if v := get("LOG_LEVEL"); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			problems = append(problems, fmt.Errorf("LOG_LEVEL must be debug, info, warn or error, got %q", v))
		}
	}

	if cfg.Env == Development {
		if user := get("DEV_REMOTE_USER"); user != "" {
			cfg.DevUser = &DevUser{
				Username:    user,
				DisplayName: get("DEV_REMOTE_NAME"),
				Email:       get("DEV_REMOTE_EMAIL"),
			}
		}
	}

	if len(problems) > 0 {
		return Base{}, errors.Join(problems...)
	}
	return cfg, nil
}

// Port reads PORT (default 3000). The healthcheck subcommand uses it on its own, so it works
// without the rest of the configuration.
func Port(getenv func(string) string) (int, error) {
	v := strings.TrimSpace(getenv("PORT"))
	if v == "" {
		return defaultPort, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("PORT must be a whole number from 1 to 65535, got %q", v)
	}
	return n, nil
}

func parseDatabaseURL(v string) (string, error) {
	if v == "" {
		return "", errors.New("DATABASE_URL is required")
	}
	// Never include v or the parse error in the message: both can contain the password.
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return "", errors.New("DATABASE_URL must be a URL like postgres://user:password@host:5432/dbname")
	}
	return v, nil
}

// parseAppURL accepts an origin (scheme and host, optional port and trailing slash) and returns it
// in the form browsers send in the Origin header: lowercase, no default port, no trailing slash.
func parseAppURL(v string) (string, error) {
	if v == "" {
		return "", errors.New("APP_URL is required (e.g. https://todo.lab.juanmonsalve.dev)")
	}
	invalid := fmt.Errorf("APP_URL must be an http or https origin such as https://todo.lab.juanmonsalve.dev, with no path, query or credentials, got %q", v)
	u, err := url.Parse(v)
	if err != nil {
		return "", invalid
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", invalid
	}
	host := strings.ToLower(u.Host)
	switch u.Scheme {
	case "https":
		host = strings.TrimSuffix(host, ":443")
	case "http":
		host = strings.TrimSuffix(host, ":80")
	}
	return u.Scheme + "://" + host, nil
}
