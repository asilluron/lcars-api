// Package broker connects lcars-api to NATS: either to an existing NATS server
// or to an embedded one that it runs in-process.
package broker

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/asilluron/lcars-api/internal/config"
)

// startTimeout is how long the embedded server may take to accept clients.
const startTimeout = 10 * time.Second

// Broker is a NATS connection, plus the embedded server it is connected to,
// if any.
type Broker struct {
	Conn *nats.Conn

	server *server.Server // nil when connected to an external server
	closed chan struct{}  // closed once Conn is closed
}

// Start connects to cfg.URL if set. Otherwise it starts an embedded NATS
// server listening on cfg.Addr and connects to it in-process. The connection
// reconnects forever if it drops; Close stops it.
func Start(cfg config.NATS, logger *slog.Logger) (*Broker, error) {
	b := &Broker{closed: make(chan struct{})}
	opts := []nats.Option{
		nats.Name("lcars-api"),
		nats.MaxReconnects(-1),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			logger.Warn("nats disconnected", "err", err)
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			logger.Info("nats reconnected", "server", redact(nc.ConnectedUrl()))
		}),
		nats.ErrorHandler(func(_ *nats.Conn, sub *nats.Subscription, err error) {
			subject := ""
			if sub != nil {
				subject = sub.Subject
			}
			logger.Error("nats error", "subject", subject, "err", err)
		}),
		nats.ClosedHandler(func(*nats.Conn) { close(b.closed) }),
	}

	serverURL := cfg.URL
	if serverURL == "" {
		ns, err := startServer(cfg.Addr, logger)
		if err != nil {
			return nil, err
		}
		b.server = ns
		serverURL = ns.ClientURL()
		opts = append(opts, nats.InProcessServer(ns))
		logger.Info("nats server listening", "addr", ns.Addr().String())
	}

	nc, err := nats.Connect(serverURL, opts...)
	if err != nil {
		if b.server != nil {
			b.server.Shutdown()
		}
		return nil, fmt.Errorf("connect to NATS at %s: %w", redact(serverURL), err)
	}
	b.Conn = nc
	if b.server == nil {
		logger.Info("nats connected", "server", redact(nc.ConnectedUrl()))
	}
	return b, nil
}

// Close drains the connection, letting in-flight requests finish, then stops
// the embedded server if there is one. It gives up draining when ctx is done.
func (b *Broker) Close(ctx context.Context) error {
	var err error
	if drainErr := b.Conn.Drain(); drainErr == nil {
		select {
		case <-b.closed:
		case <-ctx.Done():
			err = fmt.Errorf("drain NATS connection: %w", ctx.Err())
		}
	}
	b.Conn.Close()
	if b.server != nil {
		b.server.Shutdown()
		b.server.WaitForShutdown()
	}
	return err
}

// startServer runs an embedded NATS server on addr ("host:port"; port 0
// picks a free one) and waits until it accepts clients.
func startServer(addr string, logger *slog.Logger) (*server.Server, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("nats address: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("nats address: invalid port %q", portStr)
	}
	if port == 0 {
		port = server.RANDOM_PORT // 0 would mean the default port, 4222
	}

	ns, err := server.NewServer(&server.Options{
		ServerName: "lcars-api",
		Host:       host,
		Port:       port,
		NoSigs:     true, // signals are handled by main
	})
	if err != nil {
		return nil, fmt.Errorf("start NATS server: %w", err)
	}
	log := &serverLogger{logger: logger.With("component", "nats-server")}
	ns.SetLoggerV2(log, false, false, false)
	ns.Start()
	// Wait until it is ready, or has failed (e.g. the port is taken).
	for deadline := time.Now().Add(startTimeout); time.Now().Before(deadline); {
		if ns.ReadyForConnections(50 * time.Millisecond) {
			return ns, nil
		}
		if msg := log.fatal.Load(); msg != nil {
			ns.Shutdown()
			return nil, fmt.Errorf("start NATS server on %s: %s", addr, *msg)
		}
	}
	ns.Shutdown()
	return nil, fmt.Errorf("start NATS server on %s: not ready after %s", addr, startTimeout)
}

// serverLogger sends the embedded server's logs to slog. Its notices (a
// startup banner, mostly) are logged at debug level; lcars-api logs its own
// summary. Fatal errors are logged and kept for startServer to report,
// instead of exiting the process.
type serverLogger struct {
	logger *slog.Logger
	fatal  atomic.Pointer[string]
}

func (l *serverLogger) Noticef(format string, v ...any) { l.logger.Debug(fmt.Sprintf(format, v...)) }
func (l *serverLogger) Warnf(format string, v ...any)   { l.logger.Warn(fmt.Sprintf(format, v...)) }
func (l *serverLogger) Errorf(format string, v ...any)  { l.logger.Error(fmt.Sprintf(format, v...)) }

func (l *serverLogger) Debugf(format string, v ...any) { l.logger.Debug(fmt.Sprintf(format, v...)) }

func (l *serverLogger) Tracef(format string, v ...any) { l.logger.Debug(fmt.Sprintf(format, v...)) }

func (l *serverLogger) Fatalf(format string, v ...any) {
	msg := fmt.Sprintf(format, v...)
	l.fatal.Store(&msg)
	l.logger.Error(msg)
}

// redact hides passwords in a comma-separated list of NATS URLs, for logging.
func redact(urls string) string {
	parts := strings.Split(urls, ",")
	for i, p := range parts {
		if u, err := url.Parse(strings.TrimSpace(p)); err == nil && u.User != nil {
			if _, hasPassword := u.User.Password(); hasPassword {
				parts[i] = u.Redacted()
			} else {
				// A bare user is often a token, so hide it too.
				u.User = url.User("xxxxx")
				parts[i] = u.String()
			}
		}
	}
	return strings.Join(parts, ",")
}
