package database_test

import (
	"bytes"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juanmonsalveg/homelab-go/database"
	"github.com/juanmonsalveg/homelab-go/database/dbtest"
)

// migrations stands in for an app's db/migrations: Up-only goose files, as the template writes them.
var migrations = fstest.MapFS{
	"00001_create_users.sql": {Data: []byte(`-- +goose Up
CREATE TABLE users (
    id       uuid PRIMARY KEY DEFAULT uuidv7(),
    username text NOT NULL UNIQUE
);
`)},
	"00002_create_notes.sql": {Data: []byte(`-- +goose Up
CREATE TABLE notes (
    id      uuid PRIMARY KEY DEFAULT uuidv7(),
    user_id uuid NOT NULL REFERENCES users (id),
    body    text NOT NULL
);
`)},
}

// Integration test: needs TEST_DATABASE_URL (make test-integration).
func TestMigrateFromZero(t *testing.T) {
	cfg, err := database.PoolConfig(dbtest.URL(t), 2)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	before, err := database.MigrationStatus(t.Context(), pool, migrations)
	if err != nil {
		t.Fatalf("MigrationStatus: %v", err)
	}
	if len(before) != 2 || before[0].File != "00001_create_users.sql" || before[1].File != "00002_create_notes.sql" {
		t.Fatalf("migrations = %+v, want 00001_create_users.sql then 00002_create_notes.sql", before)
	}
	for _, m := range before {
		if !m.AppliedAt.IsZero() {
			t.Errorf("%s applied before Migrate ran", m.File)
		}
	}

	var logs bytes.Buffer
	if err := database.Migrate(t.Context(), pool, migrations, slog.New(slog.NewJSONHandler(&logs, nil))); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if n := strings.Count(logs.String(), `"msg":"applied migration"`); n != len(before) {
		t.Errorf("logged %d applied migrations, want %d: %s", n, len(before), logs.String())
	}
	applied, err := database.MigrationStatus(t.Context(), pool, migrations)
	if err != nil {
		t.Fatalf("MigrationStatus: %v", err)
	}
	for _, m := range applied {
		if m.AppliedAt.IsZero() {
			t.Errorf("%s still pending after Migrate", m.File)
		}
	}

	// The tables belong to the role that ran the migrations (the app's own), never to a superuser.
	var owner, currentUser string
	err = pool.QueryRow(t.Context(),
		`SELECT tableowner, current_user FROM pg_tables WHERE schemaname = current_schema() AND tablename = 'users'`,
	).Scan(&owner, &currentUser)
	if err != nil {
		t.Fatalf("read the users table owner: %v", err)
	}
	if owner != currentUser {
		t.Errorf("users is owned by %q, want %q", owner, currentUser)
	}

	// A second run is a no-op.
	logs.Reset()
	if err := database.Migrate(t.Context(), pool, migrations, slog.New(slog.NewJSONHandler(&logs, nil))); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if strings.Contains(logs.String(), `"msg":"applied migration"`) || !strings.Contains(logs.String(), `"applied":0`) {
		t.Errorf("second Migrate logs = %s, want nothing applied", logs.String())
	}
	again, err := database.MigrationStatus(t.Context(), pool, migrations)
	if err != nil {
		t.Fatalf("MigrationStatus: %v", err)
	}
	if !reflect.DeepEqual(again, applied) {
		t.Errorf("status changed after a no-op Migrate:\n%+v\nwant\n%+v", again, applied)
	}
}
