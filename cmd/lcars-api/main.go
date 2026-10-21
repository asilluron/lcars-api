// Command lcars-api serves the LCARS home API over NATS, with a small HTTP
// server alongside for health checks, docs and JSON endpoints.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/asilluron/lcars-api/internal/broker"
	"github.com/asilluron/lcars-api/internal/calendar"
	"github.com/asilluron/lcars-api/internal/config"
	"github.com/asilluron/lcars-api/internal/natsapi"
	"github.com/asilluron/lcars-api/internal/server"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Getenv, os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "lcars-api:", err)
		os.Exit(1)
	}
}

// run starts the servers and blocks until ctx is cancelled and they have
// shut down. Dependencies are passed in so tests can drive it end to end.
func run(ctx context.Context, getenv func(string) string, stdout io.Writer) (err error) {
	cfg, err := config.Load(getenv)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := newLogger(cfg, stdout)
	slog.SetDefault(logger)
	if cfg.File != "" {
		logger.Info("config loaded", "file", cfg.File)
	}

	var cal *calendar.Store
	if cfg.Calendar.Dir != "" {
		cal = calendar.NewStore(cfg.Calendar.Dir, cfg.Location, logger)
		if err := cal.Load(); err != nil {
			return fmt.Errorf("load calendar: %w", err)
		}
	} else {
		logger.Info("calendar disabled: calendar.dir is not set")
	}

	bus, err := broker.Start(cfg.NATS, logger)
	if err != nil {
		return err
	}
	// From here on, shutdown must close the broker: it may run the embedded
	// NATS server.
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
		defer cancel()
		if closeErr := bus.Close(closeCtx); closeErr != nil && err == nil {
			err = fmt.Errorf("shutdown: %w", closeErr)
		}
		if err == nil {
			logger.Info("server stopped")
		}
	}()

	svc, err := natsapi.Start(bus.Conn, logger, server.Version(), natsapi.Deps{Calendar: cal})
	if err != nil {
		return err
	}
	defer func() { _ = svc.Stop() }()
	if cal != nil {
		go cal.Run(ctx, cfg.Calendar.RefreshInterval, natsapi.CalendarChanged(bus.Conn, logger))
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.New(logger, server.Deps{Calendar: cal, NATS: bus.Conn}),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", cfg.Addr, "version", server.Version())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("listen: %w", err)
	case <-ctx.Done():
	}

	logger.Info("shutting down", "timeout", cfg.ShutdownTimeout)
	// ctx is already cancelled; keep its values but give shutdown its own deadline.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil // the deferred functions stop the NATS service and broker
}

func newLogger(cfg config.Config, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}
