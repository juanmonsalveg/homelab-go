package httpx

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// LogRequests logs one line per request: method, path, status, duration and client IP, plus any
// attributes inner handlers add with AddLogAttrs (auth adds the username).
func LogRequests(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			extra := &logAttrs{}
			ctx := context.WithValue(r.Context(), logAttrsKey{}, extra)
			rec := &statusRecorder{ResponseWriter: w}

			next.ServeHTTP(rec, r.WithContext(ctx))

			attrs := append([]slog.Attr{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status()),
				slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
				slog.String("client_ip", clientIP(r)),
			}, extra.get()...)
			logger.LogAttrs(ctx, slog.LevelInfo, "request", attrs...)
		})
	}
}

// AddLogAttrs adds attributes to the current request's log line. It does nothing outside LogRequests.
func AddLogAttrs(ctx context.Context, attrs ...slog.Attr) {
	if la, ok := ctx.Value(logAttrsKey{}).(*logAttrs); ok {
		la.add(attrs)
	}
}

type logAttrsKey struct{}

// logAttrs is shared between LogRequests and the handlers it wraps, which may run other goroutines.
type logAttrs struct {
	mu    sync.Mutex
	attrs []slog.Attr
}

func (la *logAttrs) add(attrs []slog.Attr) {
	la.mu.Lock()
	defer la.mu.Unlock()
	la.attrs = append(la.attrs, attrs...)
}

func (la *logAttrs) get() []slog.Attr {
	la.mu.Lock()
	defer la.mu.Unlock()
	return la.attrs
}

// clientIP returns the address Traefik saw, for logging only: the last X-Forwarded-For entry, which
// Traefik appends (the equivalent of Express's `trust proxy 1`). Without the header, as in dev and
// for the container healthcheck, it falls back to the connection's address.
func clientIP(r *http.Request) string {
	if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
		last := xff[len(xff)-1]
		if i := strings.LastIndexByte(last, ','); i >= 0 {
			last = last[i+1:]
		}
		if ip := strings.TrimSpace(last); ip != "" {
			return ip
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// statusRecorder remembers the status code a handler sent.
type statusRecorder struct {
	http.ResponseWriter
	code int // 0 until the handler writes the header or body
}

func (sr *statusRecorder) WriteHeader(code int) {
	// 1xx responses (e.g. 103 Early Hints) come before the real status.
	if sr.code == 0 && code >= 200 {
		sr.code = code
	}
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Write(b []byte) (int, error) {
	if sr.code == 0 {
		sr.code = http.StatusOK
	}
	return sr.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer (Flush, deadlines).
func (sr *statusRecorder) Unwrap() http.ResponseWriter { return sr.ResponseWriter }

func (sr *statusRecorder) status() int {
	if sr.code == 0 {
		return http.StatusOK // net/http sends 200 when a handler writes nothing
	}
	return sr.code
}
