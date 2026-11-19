package database

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestPoolConfig(t *testing.T) {
	tests := []struct {
		name        string
		url         string
		wantSSLMode string
	}{
		{"adds sslmode=disable", "postgres://app:secret@postgres:5432/app", "disable"},
		{"keeps other parameters", "postgres://app:secret@postgres:5432/app?application_name=app", "disable"},
		{"keeps an explicit sslmode", "postgres://app:secret@postgres:5432/app?sslmode=require", "require"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := PoolConfig(tt.url, 7)
			if err != nil {
				t.Fatalf("PoolConfig: %v", err)
			}
			// sslmode=disable leaves no TLS config; any mode that may use TLS sets one.
			if gotTLS := cfg.ConnConfig.TLSConfig != nil; gotTLS != (tt.wantSSLMode != "disable") {
				t.Errorf("TLS configured = %v, want sslmode=%s", gotTLS, tt.wantSSLMode)
			}
			if len(cfg.ConnConfig.Fallbacks) != 0 {
				t.Errorf("%d fallback configs, want none (sslmode=prefer would add a plaintext fallback)", len(cfg.ConnConfig.Fallbacks))
			}
			if cfg.MaxConns != 7 || cfg.MinConns != 0 || cfg.MaxConnIdleTime != 5*time.Minute {
				t.Errorf("MaxConns, MinConns, MaxConnIdleTime = %d, %d, %v; want 7, 0, 5m",
					cfg.MaxConns, cfg.MinConns, cfg.MaxConnIdleTime)
			}
			if cfg.ConnConfig.Host != "postgres" || cfg.ConnConfig.Database != "app" || cfg.ConnConfig.User != "app" {
				t.Errorf("host, database, user = %q, %q, %q", cfg.ConnConfig.Host, cfg.ConnConfig.Database, cfg.ConnConfig.User)
			}
		})
	}
}

func TestPoolConfigKeepsRuntimeParams(t *testing.T) {
	cfg, err := PoolConfig("postgres://app:secret@localhost/app?search_path=test_abc", 1)
	if err != nil {
		t.Fatalf("PoolConfig: %v", err)
	}
	if got := cfg.ConnConfig.RuntimeParams["search_path"]; got != "test_abc" {
		t.Errorf("search_path = %q, want test_abc", got)
	}
}

func TestPoolConfigMaxConnsCap(t *testing.T) {
	for _, n := range []int{0, -1, 11} {
		if _, err := PoolConfig("postgres://app:secret@localhost/app", n); err == nil {
			t.Errorf("PoolConfig(maxConns=%d) succeeded, want an error", n)
		}
	}
	if _, err := PoolConfig("postgres://app:secret@localhost/app", 10); err != nil {
		t.Errorf("PoolConfig(maxConns=10) = %v, want no error", err)
	}
}

func TestPoolConfigInvalidURLHidesPassword(t *testing.T) {
	for _, url := range []string{
		"postgres://app:hunter2@localhost:notaport/app",
		"postgres://app:hunter2@localhost/app?connect_timeout=soon",
	} {
		_, err := PoolConfig(url, 5)
		if err == nil {
			t.Errorf("PoolConfig(%q) succeeded, want an error", url)
			continue
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("error %q contains the password", err)
		}
	}
}

// fastRetry keeps the retry tests quick: delays of 1, 2, 4, 4, ... ms for at most 50 ms.
func fastRetry() retryPolicy {
	return retryPolicy{
		initialDelay:   time.Millisecond,
		maxDelay:       4 * time.Millisecond,
		total:          50 * time.Millisecond,
		attemptTimeout: time.Second,
	}
}

func TestWaitUntilReadyRetriesUntilSuccess(t *testing.T) {
	var logs bytes.Buffer
	calls := 0
	ping := func(ctx context.Context) error {
		calls++
		if _, ok := ctx.Deadline(); !ok {
			t.Error("ping called without a deadline")
		}
		if calls < 3 {
			return errors.New("connection refused")
		}
		return nil
	}

	err := waitUntilReady(t.Context(), ping, fastRetry(), slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("waitUntilReady = %v, want nil", err)
	}
	if calls != 3 {
		t.Errorf("ping called %d times, want 3", calls)
	}
	if n := strings.Count(logs.String(), "database not ready, retrying"); n != 2 {
		t.Errorf("logged %d retries, want 2: %s", n, logs.String())
	}
}

func TestWaitUntilReadyGivesUp(t *testing.T) {
	pingErr := errors.New("connection refused")
	calls := 0
	start := time.Now()
	err := waitUntilReady(t.Context(), func(context.Context) error {
		calls++
		return pingErr
	}, fastRetry(), slog.New(slog.DiscardHandler))

	if !errors.Is(err, pingErr) {
		t.Errorf("waitUntilReady = %v, want it to wrap the last ping error", err)
	}
	if calls < 3 {
		t.Errorf("ping called %d times, want several attempts", calls)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("gave up after %v, want about the 50 ms total", elapsed)
	}
}

func TestWaitUntilReadyStopsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	policy := fastRetry()
	policy.initialDelay, policy.maxDelay, policy.total = time.Hour, time.Hour, 2*time.Hour

	done := make(chan error, 1)
	go func() {
		done <- waitUntilReady(ctx, func(context.Context) error { return errors.New("refused") }, policy, slog.New(slog.DiscardHandler))
	}()
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("waitUntilReady = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitUntilReady kept waiting after the context was canceled")
	}
}
