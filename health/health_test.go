package health

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// fakeDB answers SELECT 1 with err (nil means success) and records the query and its deadline.
type fakeDB struct {
	err         error
	sql         string
	hasDeadline bool
	deadline    time.Time
}

func (f *fakeDB) QueryRow(ctx context.Context, sql string, _ ...any) pgx.Row {
	f.sql = sql
	f.deadline, f.hasDeadline = ctx.Deadline()
	return fakeRow{err: f.err}
}

type fakeRow struct{ err error }

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*int) = 1
	return nil
}

func serve(t *testing.T, db DB, logger *slog.Logger) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler(db, logger).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	return rec
}

func TestHandlerOK(t *testing.T) {
	db := &fakeDB{}
	rec := serve(t, db, slog.New(slog.DiscardHandler))
	end := time.Now()

	if rec.Code != http.StatusOK || rec.Body.String() != "{\"status\":\"ok\"}\n" {
		t.Errorf("response = %d %q, want 200 {\"status\":\"ok\"}", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if db.sql != "SELECT 1" {
		t.Errorf("query = %q, want SELECT 1", db.sql)
	}
	// The query started before end, so its deadline can't be later than end + timeout.
	if !db.hasDeadline || db.deadline.After(end.Add(timeout)) {
		t.Errorf("query deadline = %v after the response (set: %v), want at most %v", db.deadline.Sub(end), db.hasDeadline, timeout)
	}
}

func TestHandlerDatabaseDown(t *testing.T) {
	var logs bytes.Buffer
	secret := errors.New("dial tcp 10.0.0.5:5432: connection refused")
	rec := serve(t, &fakeDB{err: secret}, slog.New(slog.NewJSONHandler(&logs, nil)))

	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != "{\"status\":\"unavailable\"}\n" {
		t.Errorf("response = %d %q, want 503 {\"status\":\"unavailable\"}", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logs.String(), "connection refused") || !strings.Contains(logs.String(), `"level":"WARN"`) {
		t.Errorf("logs = %s, want a warning with the error", logs.String())
	}
}
