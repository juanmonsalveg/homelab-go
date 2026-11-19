// Package database connects to Postgres and runs the app's migrations (README.md §4). The homelab
// server is shared: the pool stays small, TLS is off and the first connection is retried, because
// after a reboot the app can start before Postgres.
package database

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juanmonsalveg/homelab-go/config"
)

const maxConnIdleTime = 5 * time.Minute

// PoolConfig builds the pool settings from DATABASE_URL. Without an sslmode parameter it adds
// sslmode=disable: the homelab Postgres has no TLS, and pgx's default (prefer) would try TLS first.
func PoolConfig(databaseURL string, maxConns int) (*pgxpool.Config, error) {
	if maxConns < 1 || maxConns > config.MaxDBMaxConns {
		return nil, fmt.Errorf("max connections must be from 1 to %d, got %d", config.MaxDBMaxConns, maxConns)
	}
	// Errors never include the URL or the parse error: both can contain the password.
	u, err := url.Parse(databaseURL)
	if err != nil {
		return nil, errors.New("database URL is invalid")
	}
	if q := u.Query(); !q.Has("sslmode") {
		q.Set("sslmode", "disable")
		u.RawQuery = q.Encode()
	}
	cfg, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		return nil, errors.New("database URL is invalid")
	}
	cfg.MaxConns = int32(maxConns)
	cfg.MinConns = 0
	cfg.MaxConnIdleTime = maxConnIdleTime
	return cfg, nil
}

// retryPolicy is how long Connect keeps trying: delays start at initialDelay and double up to
// maxDelay, until total has passed. Each attempt gets attemptTimeout.
type retryPolicy struct {
	initialDelay   time.Duration
	maxDelay       time.Duration
	total          time.Duration
	attemptTimeout time.Duration
}

// connectRetry covers Postgres starting after the app at boot (about 60 s). If that isn't enough,
// the app exits 1 and `restart: unless-stopped` brings it back.
func connectRetry() retryPolicy {
	return retryPolicy{
		initialDelay:   500 * time.Millisecond,
		maxDelay:       5 * time.Second,
		total:          60 * time.Second,
		attemptTimeout: 5 * time.Second,
	}
}

// Connect opens a pool and waits until the database answers, retrying with backoff.
func Connect(ctx context.Context, cfg *pgxpool.Config, logger *slog.Logger) (*pgxpool.Pool, error) {
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := waitUntilReady(ctx, pool.Ping, connectRetry(), logger); err != nil {
		pool.Close()
		return nil, err
	}
	logger.Info("connected to the database",
		slog.String("host", cfg.ConnConfig.Host),
		slog.String("database", cfg.ConnConfig.Database),
		slog.String("user", cfg.ConnConfig.User),
	)
	return pool, nil
}

// waitUntilReady calls ping until it succeeds, the retry policy runs out or ctx is done.
func waitUntilReady(ctx context.Context, ping func(context.Context) error, policy retryPolicy, logger *slog.Logger) error {
	deadline := time.Now().Add(policy.total)
	delay := policy.initialDelay
	for attempt := 1; ; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, policy.attemptTimeout)
		err := ping(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("connect to the database: %w", context.Cause(ctx))
		}
		if time.Now().Add(delay).After(deadline) {
			return fmt.Errorf("connect to the database: gave up after %d attempts: %w", attempt, err)
		}
		logger.Warn("database not ready, retrying",
			slog.Int("attempt", attempt),
			slog.String("retry_in", delay.String()),
			slog.Any("error", err),
		)

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("connect to the database: %w", context.Cause(ctx))
		case <-timer.C:
		}
		delay = min(delay*2, policy.maxDelay)
	}
}
