package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// HTTPError is an error that carries the status code to send to the client.
type HTTPError struct {
	Code int
	Msg  string
}

func (e *HTTPError) Error() string { return e.Msg }

// handlerFunc is an http.HandlerFunc that returns an error instead of writing
// error responses itself.
type handlerFunc func(w http.ResponseWriter, r *http.Request) error

// handle adapts an error-returning handler to http.Handler. An *HTTPError is
// sent to the client as-is; any other error is logged and becomes a 500.
func handle(logger *slog.Logger, h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := h(w, r)
		if err == nil {
			return
		}
		if he, ok := errors.AsType[*HTTPError](err); ok {
			writeError(w, he.Code, he.Msg)
			return
		}
		logger.ErrorContext(r.Context(), "handler failed",
			"err", err, "method", r.Method, "path", r.URL.Path)
		writeError(w, http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError))
	})
}

// writeJSON encodes v as the JSON response body with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	_ = writeJSON(w, status, map[string]string{"error": msg})
}
