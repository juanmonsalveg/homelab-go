package config

import (
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

// env builds a getenv from a map, starting from the two required variables.
func env(overrides map[string]string) func(string) string {
	vars := map[string]string{
		"DATABASE_URL": "postgres://app:secret@localhost:5432/app",
		"APP_URL":      "https://app.lab.juanmonsalve.dev",
	}
	for k, v := range overrides {
		vars[k] = v
	}
	return func(key string) string { return vars[key] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Base{
		DatabaseURL:    "postgres://app:secret@localhost:5432/app",
		AppURL:         "https://app.lab.juanmonsalve.dev",
		Port:           3000,
		Env:            Production,
		DBMaxConns:     5,
		MigrateOnStart: true,
		LogLevel:       slog.LevelInfo,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load() =\n%+v\nwant\n%+v", cfg, want)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"PORT":             "8080",
		"DB_MAX_CONNS":     "10",
		"MIGRATE_ON_START": "false",
		"LOG_LEVEL":        "DEBUG",
		"APP_ENV":          " production ",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 8080 || cfg.DBMaxConns != 10 || cfg.MigrateOnStart || cfg.LogLevel != slog.LevelDebug || cfg.Env != Production {
		t.Errorf("Load() = %+v", cfg)
	}
}

func TestLoadDevUser(t *testing.T) {
	devVars := map[string]string{
		"DEV_REMOTE_USER":  "ana",
		"DEV_REMOTE_NAME":  "Ana",
		"DEV_REMOTE_EMAIL": "ana@example.com",
	}
	withEnv := func(appEnv string) map[string]string {
		m := map[string]string{"APP_ENV": appEnv}
		for k, v := range devVars {
			m[k] = v
		}
		return m
	}

	tests := []struct {
		name string
		vars map[string]string
		want *DevUser
	}{
		{"development", withEnv("development"), &DevUser{"ana", "Ana", "ana@example.com"}},
		{"production ignores DEV_REMOTE_*", withEnv("production"), nil},
		{"unset APP_ENV is production", withEnv(""), nil},
		{"development without DEV_REMOTE_USER", map[string]string{"APP_ENV": "development", "DEV_REMOTE_NAME": "Ana"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(env(tt.vars))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !reflect.DeepEqual(cfg.DevUser, tt.want) {
				t.Errorf("DevUser = %+v, want %+v", cfg.DevUser, tt.want)
			}
		})
	}
}

func TestLoadAppURL(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"https://app.lab.juanmonsalve.dev", "https://app.lab.juanmonsalve.dev"},
		{"https://app.lab.juanmonsalve.dev/", "https://app.lab.juanmonsalve.dev"},
		{"HTTPS://App.Lab.JuanMonsalve.dev", "https://app.lab.juanmonsalve.dev"},
		{"https://app.lab.juanmonsalve.dev:443", "https://app.lab.juanmonsalve.dev"},
		{"http://localhost:80/", "http://localhost"},
		{"http://localhost:5173", "http://localhost:5173"},
		{"https://localhost:8443", "https://localhost:8443"},
	}
	for _, tt := range tests {
		cfg, err := Load(env(map[string]string{"APP_URL": tt.in}))
		if err != nil {
			t.Errorf("APP_URL=%q: %v", tt.in, err)
			continue
		}
		if cfg.AppURL != tt.want {
			t.Errorf("APP_URL=%q: AppURL = %q, want %q", tt.in, cfg.AppURL, tt.want)
		}
	}
}

func TestLoadInvalid(t *testing.T) {
	tests := []struct {
		name    string
		vars    map[string]string
		wantErr string
	}{
		{"missing DATABASE_URL", map[string]string{"DATABASE_URL": ""}, "DATABASE_URL is required"},
		{"DATABASE_URL not postgres", map[string]string{"DATABASE_URL": "mysql://app:secret@localhost/app"}, "DATABASE_URL must be"},
		{"DATABASE_URL keyword form", map[string]string{"DATABASE_URL": "host=localhost user=app password=secret"}, "DATABASE_URL must be"},
		{"DATABASE_URL unparseable", map[string]string{"DATABASE_URL": "postgres://app:secret@[bad/app"}, "DATABASE_URL must be"},
		{"missing APP_URL", map[string]string{"APP_URL": ""}, "APP_URL is required"},
		{"APP_URL with path", map[string]string{"APP_URL": "https://example.com/app"}, "APP_URL must be"},
		{"APP_URL with query", map[string]string{"APP_URL": "https://example.com/?a=b"}, "APP_URL must be"},
		{"APP_URL with fragment", map[string]string{"APP_URL": "https://example.com/#x"}, "APP_URL must be"},
		{"APP_URL with credentials", map[string]string{"APP_URL": "https://user:pw@example.com"}, "APP_URL must be"},
		{"APP_URL without scheme", map[string]string{"APP_URL": "app.lab.juanmonsalve.dev"}, "APP_URL must be"},
		{"APP_URL other scheme", map[string]string{"APP_URL": "ftp://example.com"}, "APP_URL must be"},
		{"PORT not a number", map[string]string{"PORT": "http"}, "PORT must be"},
		{"PORT zero", map[string]string{"PORT": "0"}, "PORT must be"},
		{"PORT too big", map[string]string{"PORT": "65536"}, "PORT must be"},
		{"APP_ENV wrong case", map[string]string{"APP_ENV": "Development"}, "APP_ENV must be"},
		{"APP_ENV unknown", map[string]string{"APP_ENV": "staging"}, "APP_ENV must be"},
		{"DB_MAX_CONNS above cap", map[string]string{"DB_MAX_CONNS": "11"}, "DB_MAX_CONNS must be"},
		{"DB_MAX_CONNS zero", map[string]string{"DB_MAX_CONNS": "0"}, "DB_MAX_CONNS must be"},
		{"DB_MAX_CONNS not a number", map[string]string{"DB_MAX_CONNS": "five"}, "DB_MAX_CONNS must be"},
		{"MIGRATE_ON_START not a bool", map[string]string{"MIGRATE_ON_START": "sometimes"}, "MIGRATE_ON_START must be"},
		{"LOG_LEVEL unknown", map[string]string{"LOG_LEVEL": "loud"}, "LOG_LEVEL must be"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(env(tt.vars))
			if err == nil {
				t.Fatalf("Load() = %+v, want error containing %q", cfg, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Errorf("error leaks the database password: %q", err)
			}
		})
	}
}

func TestLoadReportsEveryProblem(t *testing.T) {
	_, err := Load(func(string) string { return "" })
	if err == nil {
		t.Fatal("Load with no variables succeeded")
	}
	for _, want := range []string{"DATABASE_URL is required", "APP_URL is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

func TestPort(t *testing.T) {
	port, err := Port(func(string) string { return "" })
	if err != nil || port != 3000 {
		t.Errorf("Port() with PORT unset = %d, %v; want 3000, nil", port, err)
	}
	port, err = Port(func(string) string { return "8080" })
	if err != nil || port != 8080 {
		t.Errorf("Port() with PORT=8080 = %d, %v; want 8080, nil", port, err)
	}
}
