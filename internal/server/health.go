package server

import (
	"net/http"
	"runtime/debug"

	"github.com/nats-io/nats.go"
)

type healthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	// NATS is the state of the NATS connection the API is served on:
	// "connected" or "disconnected". It is omitted if there is none.
	NATS string `json:"nats,omitempty"`
}

// healthz reports that the process is up, and, since the API is served over
// NATS, answers 503 while the NATS connection is down.
func healthz(nc *nats.Conn) handlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) error {
		resp := healthResponse{Status: "ok", Version: Version()}
		if nc == nil {
			return writeJSON(w, http.StatusOK, resp)
		}
		if !nc.IsConnected() {
			resp.Status, resp.NATS = "unavailable", "disconnected"
			return writeJSON(w, http.StatusServiceUnavailable, resp)
		}
		resp.NATS = "connected"
		return writeJSON(w, http.StatusOK, resp)
	}
}

// version is set at build time for builds without VCS info (e.g. Docker):
//
//	-ldflags "-X github.com/asilluron/lcars-api/internal/server.version=v1.2.3"
var version string

// Version returns the build version: the -ldflags value if set, otherwise the
// module version the Go toolchain stamps from VCS on `go build`, otherwise
// "(devel)" for `go run` and tests.
func Version() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}
