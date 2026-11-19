// Package httpx holds the HTTP helpers for an app's handlers and for this module's own (README.md
// §7): JSON responses and errors, request bodies, the request log and panic recovery. It imports no
// other package of this module, so any of them can use it (server imports auth, and auth needs
// AddLogAttrs, so these can't live in server).
package httpx

import (
	"encoding/json"
	"net/http"
)

// WriteJSON writes v as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		// Only a programming error (an unsupported type) gets here.
		status = http.StatusInternalServerError
		body = []byte(`{"error":{"code":"internal","message":"Internal server error"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

// WriteError writes the API's error shape: {"error":{"code":"...","message":"..."}}.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message}})
}

// MethodNotAllowed answers every request with a JSON 405 and an Allow header listing allow, for
// example "GET, HEAD". Mount it on a method-less pattern next to the route's method patterns, so
// other methods get the API's error shape instead of net/http's plain-text 405.
func MethodNotAllowed(allow string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", allow)
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
	})
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
