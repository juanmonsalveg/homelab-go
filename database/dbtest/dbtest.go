// Package dbtest gives integration tests a schema of their own in the TEST_DATABASE_URL database,
// an app's local <app>_test (README.md §10). Tests that use it are skipped when TEST_DATABASE_URL
// isn't set, which keeps `make test` free of databases; `make test-integration` sets it.
package dbtest

import (
	"context"
	"crypto/rand"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juanmonsalveg/homelab-go/database"
)

// URL returns TEST_DATABASE_URL pointed at a new, empty schema through search_path, so tests can
// run in parallel and always start from zero. The schema is dropped when the test ends.
func URL(t testing.TB) string {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL isn't set; run make test-integration")
	}

	cfg, err := database.PoolConfig(base, 1)
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL: %v", err)
	}
	conn, err := pgx.ConnectConfig(t.Context(), cfg.ConnConfig)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL (is the database up? make db-up): %v", err)
	}
	schema := "test_" + strings.ToLower(rand.Text())
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := conn.Exec(t.Context(), "CREATE SCHEMA "+ident); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		// t.Context() is already canceled when cleanups run.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := conn.Exec(ctx, "DROP SCHEMA "+ident+" CASCADE"); err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
		_ = conn.Close(ctx)
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema) // pgx sends unknown URL parameters as session settings
	u.RawQuery = q.Encode()
	return u.String()
}

// Pool returns a pool on a new schema with every migration in migrations applied, closed when the
// test ends. An app passes its own, db.Migrations().
func Pool(t testing.TB, migrations fs.FS) *pgxpool.Pool {
	t.Helper()
	cfg, err := database.PoolConfig(URL(t), 5)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close) // runs before URL's cleanup drops the schema
	if err := database.Migrate(t.Context(), pool, migrations, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}
