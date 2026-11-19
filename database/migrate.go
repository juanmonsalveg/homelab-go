package database

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// Migrate applies the pending goose migrations in migrations, the app's db/migrations directory
// (README.md §4). It runs them as the role in DATABASE_URL, the app's own, so the tables it creates
// are owned by that role. Each SQL migration runs in its own transaction, and a Postgres advisory
// lock keeps a startup run and a manual `<app> migrate` from racing.
func Migrate(ctx context.Context, pool *pgxpool.Pool, migrations fs.FS, logger *slog.Logger) error {
	return withProvider(pool, migrations, func(p *goose.Provider) error {
		results, err := p.Up(ctx)
		for _, r := range results {
			if r.Error == nil {
				logger.Info("applied migration",
					slog.String("file", r.Source.Path),
					slog.String("duration", r.Duration.String()),
				)
			}
		}
		if err != nil {
			return fmt.Errorf("apply migrations: %w", err)
		}
		version, err := p.GetDBVersion(ctx)
		if err != nil {
			return fmt.Errorf("read the schema version: %w", err)
		}
		logger.Info("database schema up to date", slog.Int64("version", version), slog.Int("applied", len(results)))
		return nil
	})
}

// MigrationState is one migration and whether it has been applied.
type MigrationState struct {
	Version int64
	File    string
	// AppliedAt is when the migration ran; zero means it's still pending.
	AppliedAt time.Time
}

// MigrationStatus lists every migration in migrations, in order, with its state.
func MigrationStatus(ctx context.Context, pool *pgxpool.Pool, migrations fs.FS) ([]MigrationState, error) {
	var states []MigrationState
	err := withProvider(pool, migrations, func(p *goose.Provider) error {
		statuses, err := p.Status(ctx)
		if err != nil {
			return fmt.Errorf("read migration status: %w", err)
		}
		for _, s := range statuses {
			state := MigrationState{Version: s.Source.Version, File: s.Source.Path}
			if s.State == goose.StateApplied {
				state.AppliedAt = s.AppliedAt
			}
			states = append(states, state)
		}
		return nil
	})
	return states, err
}

// withProvider runs fn with a goose provider on a database/sql handle of its own (goose doesn't
// take a pgx pool). The handle holds at most one connection, the one goose locks and migrates on,
// and is closed afterwards: closing ends the session, which also releases the advisory lock.
func withProvider(pool *pgxpool.Pool, migrations fs.FS, fn func(*goose.Provider) error) (err error) {
	sqlDB := stdlib.OpenDB(*pool.Config().ConnConfig)
	sqlDB.SetMaxOpenConns(1)
	defer func() {
		if closeErr := sqlDB.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close the migration connection: %w", closeErr))
		}
	}()

	// Wait up to 5 minutes for the lock (goose's default), but check every second instead of every
	// 5 s, so a run queued behind another starts soon after it. Gives up when ctx is done.
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockTimeout(1, 300))
	if err != nil {
		return fmt.Errorf("create the migration lock: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations,
		goose.WithSessionLocker(locker),
		goose.WithDisableGlobalRegistry(true), // SQL migrations only; no package-level registry
	)
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	return fn(p)
}
