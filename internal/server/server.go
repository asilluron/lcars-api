// Package server wires up the HTTP routes and middleware for lcars-api.
package server

import (
	"log/slog"
	"net/http"

	"github.com/nats-io/nats.go"

	"github.com/asilluron/lcars-api/internal/calendar"
	"github.com/asilluron/lcars-api/internal/cameras"
	"github.com/asilluron/lcars-api/internal/devices"
	"github.com/asilluron/lcars-api/internal/weather"
)

// Deps are the data sources the handlers read from. A nil field disables the
// endpoints that need it; they answer 503 Service Unavailable.
type Deps struct {
	Calendar *calendar.Store
	// Cameras reads the Eufy proxy. If nil, camera endpoints answer 503.
	Cameras *cameras.Client
	// Weather reads the forecast service. If nil, GET /weather answers 503.
	Weather *weather.Client
	// NATS is the connection the API is served on, for /healthz.
	NATS *nats.Conn
	// Devices checks device tokens and manages them. If nil, devices don't
	// authenticate and the admin API answers 503.
	Devices *devices.Store
	// Admins signs admins in; it is required with Devices.
	Admins AdminAuth
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
	mux.Handle("GET /healthz", handle(logger, healthz(deps.NATS)))
	mux.Handle("GET /openapi.json", handle(logger, openAPI))
	mux.Handle("GET /docs", handle(logger, docs))

	// For devices: these need a device token when Supabase is configured.
	mux.Handle("GET /calendar", requireDevice(deps.Devices, handle(logger, calendarHandler(deps.Calendar))))
	mux.Handle("GET /cameras", requireDevice(deps.Devices, handle(logger, cameraList(logger, deps.Cameras))))
	mux.Handle("GET /cameras/{sn}/stream", requireDevice(deps.Devices, handle(logger, cameraStream(logger, deps.Cameras))))
	mux.Handle("GET /cameras/{sn}/snapshot", requireDevice(deps.Devices, handle(logger, cameraSnapshot(logger, deps.Cameras))))
	mux.Handle("GET /weather", requireDevice(deps.Devices, handle(logger, weatherHandler(logger, deps.Weather))))

	// The admin page, and the API it uses to manage device tokens.
	mux.Handle("GET /admin/", handle(logger, adminPage(adminFiles, adminBuilt)))
	mux.Handle("POST /admin/api/session", handle(logger, adminSignIn(logger, deps)))
	mux.Handle("GET /admin/api/tokens", handle(logger, requireAdmin(logger, deps, listTokens(logger, deps.Devices))))
	mux.Handle("POST /admin/api/tokens", handle(logger, requireAdmin(logger, deps, createToken(logger, deps.Devices))))
	mux.Handle("DELETE /admin/api/tokens/{id}",
		handle(logger, requireAdmin(logger, deps, revokeToken(logger, deps.Devices))))
}
