// Package natsapi serves the lcars-api API over NATS. It is a NATS service
// (see github.com/nats-io/nats.go/micro), so it answers the standard
// $SRV.PING, $SRV.INFO and $SRV.STATS discovery requests too.
//
// Message bodies are protobuf messages from proto/lcars/v1. Errors are
// replies with an empty body and the Nats-Service-Error-Code (an HTTP-style
// status such as "400") and Nats-Service-Error (a message) headers.
package natsapi

import (
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
	"google.golang.org/protobuf/proto"

	"github.com/asilluron/lcars-api/internal/calendar"
)

// ServiceName is the NATS service name, for $SRV.PING.lcars and friends.
const ServiceName = "lcars"

// Subjects. Every subject must be listed in Subjects, defined in the Rust
// crate (rust/src/lib.rs), documented in the README, and covered by the e2e
// tests; tests check all four.
const (
	// SubjectCalendarGet answers a GetCalendarRequest with a
	// GetCalendarResponse.
	SubjectCalendarGet = "lcars.calendar.get"
	// SubjectCalendarUpdated carries a CalendarUpdated event whenever the
	// calendar files change.
	SubjectCalendarUpdated = "lcars.calendar.updated"
)

// Subjects lists every subject the API answers requests on or publishes to.
var Subjects = []string{SubjectCalendarGet, SubjectCalendarUpdated}

// Deps are the data sources the handlers read from. A nil field makes the
// endpoints that need it answer 503.
type Deps struct {
	Calendar *calendar.Store
}

// Start registers the service on nc. version is the build version; it is
// reported in $SRV.INFO.
func Start(nc *nats.Conn, logger *slog.Logger, version string, deps Deps) (micro.Service, error) {
	svc, err := micro.AddService(nc, micro.Config{
		Name:        ServiceName,
		Version:     serviceVersion(version),
		Description: "LCARS home API",
		Metadata:    map[string]string{"build": version},
	})
	if err != nil {
		return nil, fmt.Errorf("add NATS service: %w", err)
	}
	if err := addEndpoints(svc, logger, deps); err != nil {
		_ = svc.Stop()
		return nil, fmt.Errorf("add NATS endpoints: %w", err)
	}
	return svc, nil
}

// addEndpoints is the single place to see every request subject the API
// answers.
func addEndpoints(svc micro.Service, logger *slog.Logger, deps Deps) error {
	return svc.AddEndpoint("calendar_get", handle(logger, calendarGet(deps.Calendar)),
		micro.WithEndpointSubject(SubjectCalendarGet),
		micro.WithEndpointMetadata(map[string]string{
			"request":  "lcars.v1.GetCalendarRequest",
			"response": "lcars.v1.GetCalendarResponse",
		}))
}

// Error is an error reply. Code is an HTTP-style status code and Msg is sent
// to the client as-is.
type Error struct {
	Code int
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// handlerFunc handles one request, returning the reply message.
type handlerFunc func(req micro.Request) (proto.Message, error)

// handle adapts h to micro.Handler: it encodes the reply, turns errors into
// error replies (an *Error as-is; any other error is logged and becomes a
// 500), recovers panics, and logs one line per request.
func handle(logger *slog.Logger, h handlerFunc) micro.Handler {
	return micro.HandlerFunc(func(req micro.Request) {
		start := time.Now()
		status := 200
		reply := func(err error) {
			e, ok := errors.AsType[*Error](err)
			if !ok {
				logger.Error("nats handler failed", "subject", req.Subject(), "err", err)
				e = &Error{Code: 500, Msg: "Internal Server Error"}
			}
			status = e.Code
			if err := req.Error(strconv.Itoa(e.Code), e.Msg, nil); err != nil {
				logger.Warn("nats reply failed", "subject", req.Subject(), "err", err)
			}
		}
		defer func() {
			if v := recover(); v != nil {
				logger.Error("panic in nats handler", "panic", v, "subject", req.Subject())
				reply(&Error{Code: 500, Msg: "Internal Server Error"})
			}
			logger.Info("nats request",
				slog.String("subject", req.Subject()),
				slog.Int("status", status),
				slog.Duration("duration", time.Since(start)),
			)
		}()

		msg, err := h(req)
		var data []byte
		if err == nil {
			data, err = proto.Marshal(msg)
		}
		if err != nil {
			reply(err)
			return
		}
		if err := req.Respond(data); err != nil {
			logger.Warn("nats reply failed", "subject", req.Subject(), "err", err)
		}
	})
}

// semVer is the version format NATS services require.
var semVer = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)

// serviceVersion turns a build version such as "v1.2.3" into the semantic
// version a NATS service needs, or "0.0.0-dev" if it isn't one.
func serviceVersion(v string) string {
	if v = strings.TrimPrefix(v, "v"); semVer.MatchString(v) {
		return v
	}
	return "0.0.0-dev"
}
