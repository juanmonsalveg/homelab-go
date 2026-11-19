// Package health serves /healthz: 200 when the database answers SELECT 1, otherwise 503 (README.md
// §6). Uptime Kuma and the container healthcheck call it, and Traefik routes to the app only while
// it's healthy. It's open: Authelia lets it through without a login, so it never reads the
// Remote-* headers and never shows error details.
package health

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/juanmonsalveg/homelab-go/httpx"
)

// timeout keeps /healthz well inside the container healthcheck's own 3 s.
const timeout = 2 * time.Second

// DB is what the check needs from the database. *pgxpool.Pool satisfies it.
type DB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type response struct {
	Status string `json:"status"`
}

// Handler answers GET (and so HEAD) /healthz.
func Handler(db DB, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		w.Header().Set("Cache-Control", "no-store")
		var one int
		if err := db.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
			// The details go only to the logs: anyone on the LAN can call this endpoint.
			logger.LogAttrs(ctx, slog.LevelWarn, "health check failed", slog.Any("error", err))
			httpx.WriteJSON(w, http.StatusServiceUnavailable, response{Status: "unavailable"})
			return
		}
		httpx.WriteJSON(w, http.StatusOK, response{Status: "ok"})
	})
}
