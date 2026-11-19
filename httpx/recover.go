package httpx

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recover turns a panic in a handler into a logged error and a JSON 500, so one bad request
// doesn't take the connection down silently. Put it inside LogRequests, so the request line
// still shows the 500.
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &statusRecorder{ResponseWriter: w}
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				// net/http uses this panic to abort a response on purpose; let it through.
				if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(v)
				}
				logger.LogAttrs(r.Context(), slog.LevelError, "panic serving request",
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String("panic", fmt.Sprint(v)),
					slog.String("stack", string(debug.Stack())),
				)
				if rec.code != 0 {
					// Part of the response is already out; a 500 can't replace it. Abort the
					// connection so the client sees a broken response, not a truncated "success".
					panic(http.ErrAbortHandler)
				}
				WriteError(w, http.StatusInternalServerError, "internal", "Internal server error")
			}()
			next.ServeHTTP(rec, r)
		})
	}
}
