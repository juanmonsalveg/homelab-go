package dbtest_test

import (
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"

	"github.com/juanmonsalveg/homelab-go/database/dbtest"
)

var migrations = fstest.MapFS{
	"00001_create_users.sql": {Data: []byte("-- +goose Up\nCREATE TABLE users (id uuid PRIMARY KEY DEFAULT uuidv7(), username text NOT NULL UNIQUE);\n")},
}

// Integration test: needs TEST_DATABASE_URL (make test-integration).
func TestPoolGivesEachTestItsOwnSchema(t *testing.T) {
	// Skips the test when TEST_DATABASE_URL isn't set, and gives the checks below a connection.
	conn, err := pgx.Connect(t.Context(), dbtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(t.Context()) })

	var schemas []string
	for _, name := range []string{"first", "second"} {
		t.Run(name, func(t *testing.T) {
			pool := dbtest.Pool(t, migrations)
			var schema string
			var rows int
			if err := pool.QueryRow(t.Context(), "SELECT current_schema(), (SELECT count(*) FROM users)").Scan(&schema, &rows); err != nil {
				t.Fatalf("the migrations' users table: %v", err)
			}
			if rows != 0 {
				t.Errorf("users has %d rows, want a new, empty table", rows)
			}
			if _, err := pool.Exec(t.Context(), "INSERT INTO users (username) VALUES ('ana')"); err != nil {
				t.Fatal(err)
			}
			schemas = append(schemas, schema)
		})
	}

	if len(schemas) != 2 || schemas[0] == schemas[1] {
		t.Fatalf("schemas = %q, want two different ones", schemas)
	}
	// Each subtest's cleanup has run, so both schemas are gone.
	var left int
	if err := conn.QueryRow(t.Context(), "SELECT count(*) FROM pg_namespace WHERE nspname = ANY($1)", schemas).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d of the subtests' schemas %q are still there, want them dropped", left, schemas)
	}
}
