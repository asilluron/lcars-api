// Package broker connects lcars-api to NATS: either to an existing NATS server
// or to an embedded one that it runs in-process.
package broker

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
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

// Auth makes the embedded NATS server require credentials from devices.
type Auth struct {
	// Check reports whether a device's user and password are valid.
	Check func(user, password string) bool
	// Publish and Subscribe are the subjects devices may publish and
	// subscribe to (wildcards allowed).
	Publish, Subscribe []string
}

// serviceUser is the user lcars-api itself connects to the embedded server
// as. Its password is random for each run.
const serviceUser = "lcars-api"

// Start connects to cfg.URL if set. Otherwise it starts an embedded NATS
// server listening on cfg.Addr and connects to it in-process. The connection
// reconnects forever if it drops; Close stops it.
//
// If auth is non-nil, the embedded server only accepts devices that pass
// auth.Check. It does not apply to an external server, which has its own
// authentication.
func Start(cfg config.NATS, auth *Auth, logger *slog.Logger) (*Broker, error) {
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
	switch {
	case serverURL == "":
		var a *authenticator
		if auth != nil {
			a = newAuthenticator(*auth)
			opts = append(opts, nats.UserInfo(serviceUser, a.servicePassword))
		}
		ns, err := startServer(cfg, a, logger)
		if err != nil {
			return nil, err
		}
		b.server = ns
		// ClientURL uses tls:// once a certificate is set. The in-process
		// pipe is not wrapped in TLS, so connect to it as nats://.
		clientURL := ns.ClientURL()
		serverURL = inProcessURL(clientURL)
		opts = append(opts, nats.InProcessServer(ns))
		logger.Info("nats server listening",
			"addr", ns.Addr().String(),
			"auth", auth != nil,
			"tls", strings.HasPrefix(clientURL, "tls://"),
		)
	case auth != nil:
		logger.Warn("device tokens are not checked by an external NATS server (nats.url); " +
			"configure authentication on that server")
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

// Disconnect closes the embedded server's connections from user, such as a
// device whose token was revoked. It does nothing for an external server.
func (b *Broker) Disconnect(user string) {
	if b.server == nil {
		return
	}
	conns, err := b.server.Connz(&server.ConnzOptions{User: user, Username: true})
	if err != nil {
		return
	}
	for _, c := range conns.Conns {
		_ = b.server.DisconnectClientByID(c.Cid) // fails only if already gone
	}
}

// inProcessURL rewrites a tls ClientURL to nats. The embedded server's
// in-process pipe speaks the plaintext NATS handshake; network clients
// still use tls:// and the certificate.
func inProcessURL(raw string) string {
	if rest, ok := strings.CutPrefix(raw, "tls://"); ok {
		return "nats://" + rest
	}
	return raw
}

// authenticator checks the credentials of clients of the embedded server.
type authenticator struct {
	auth            Auth
	servicePassword string
	devicePerms     *server.Permissions
}

func newAuthenticator(auth Auth) *authenticator {
	return &authenticator{
		auth:            auth,
		servicePassword: rand.Text() + rand.Text(), // 260 bits
		devicePerms: &server.Permissions{
			Publish:   &server.SubjectPermission{Allow: auth.Publish},
			Subscribe: &server.SubjectPermission{Allow: auth.Subscribe},
		},
	}
}

// Check implements server.Authentication.
func (a *authenticator) Check(c server.ClientAuthentication) bool {
	opts := c.GetOpts()
	switch {
	case opts.Username == serviceUser:
		if subtle.ConstantTimeCompare([]byte(opts.Password), []byte(a.servicePassword)) != 1 {
			return false
		}
		c.RegisterUser(&server.User{Username: serviceUser}) // no restrictions
		return true
	case a.auth.Check(opts.Username, opts.Password):
		c.RegisterUser(&server.User{Username: opts.Username, Permissions: a.devicePerms})
		return true
	}
	return false
}

// startServer runs an embedded NATS server on cfg.Addr ("host:port"; port 0
// picks a free one) and waits until it accepts clients. If auth is non-nil,
// clients must pass it.
func startServer(cfg config.NATS, auth *authenticator, logger *slog.Logger) (*server.Server, error) {
	certFile, keyFile, err := prepareTLS(cfg, logger)
	if err != nil {
		return nil, err
	}
	host, portStr, err := net.SplitHostPort(cfg.Addr)
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

	sopts := &server.Options{
		ServerName: "lcars-api",
		Host:       host,
		Port:       port,
		NoSigs:     true, // signals are handled by main
	}
	if certFile != "" {
		tlsConfig, err := server.GenTLSConfig(&server.TLSConfigOpts{
			CertFile: certFile,
			KeyFile:  keyFile,
		})
		if err != nil {
			return nil, fmt.Errorf("nats tls: %w", err)
		}
		sopts.TLSConfig = tlsConfig
		sopts.TLSTimeout = 2
	}
	if auth != nil {
		sopts.CustomClientAuthentication = auth
	}
	ns, err := server.NewServer(sopts)
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
			return nil, fmt.Errorf("start NATS server on %s: %s", cfg.Addr, *msg)
		}
	}
	ns.Shutdown()
	return nil, fmt.Errorf("start NATS server on %s: not ready after %s", cfg.Addr, startTimeout)
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
