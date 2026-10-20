// Package server wires up the HTTP routes and middleware for lcars-api.
package server

import (
	"log/slog"
	"net/http"

	"github.com/asilluron/lcars-api/internal/calendar"
)

// Deps are the data sources the handlers read from. A nil field disables the
// endpoints that need it; they answer 503 Service Unavailable.
type Deps struct {
	Calendar *calendar.Store
}

// New returns the root handler with all routes and middleware attached.
func New(logger *slog.Logger, deps Deps) http.Handler {
	mux := http.NewServeMux()
	addRoutes(mux, logger, deps)

	var h http.Handler = mux
	h = logRequests(logger, h)
	h = recoverPanics(logger, h)
	return h
}

// router is the part of *http.ServeMux that addRoutes uses, so tests can
// record the registered routes.
type router interface {
	Handle(pattern string, handler http.Handler)
}

// addRoutes is the single place to see every route the API exposes. Each route
// must also be documented in api/openapi.json; TestRoutesMatchOpenAPI checks.
func addRoutes(mux router, logger *slog.Logger, deps Deps) {
	mux.Handle("GET /healthz", handle(logger, healthz))
	mux.Handle("GET /calendar", handle(logger, calendarHandler(deps.Calendar)))
	mux.Handle("GET /openapi.json", handle(logger, openAPI))
	mux.Handle("GET /docs", handle(logger, docs))
}
