package server

import (
	"net/http"
	"runtime/debug"
)

type healthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// healthz reports that the process is up and serving requests.
func healthz(w http.ResponseWriter, _ *http.Request) error {
	return writeJSON(w, http.StatusOK, healthResponse{Status: "ok", Version: Version()})
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
