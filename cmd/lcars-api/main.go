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
	"slices"
	"syscall"

	"github.com/asilluron/lcars-api/internal/broker"
	"github.com/asilluron/lcars-api/internal/calendar"
	"github.com/asilluron/lcars-api/internal/cameras"
	"github.com/asilluron/lcars-api/internal/config"
	"github.com/asilluron/lcars-api/internal/devices"
	"github.com/asilluron/lcars-api/internal/natsapi"
	"github.com/asilluron/lcars-api/internal/server"
	"github.com/asilluron/lcars-api/internal/supabase"
	"github.com/asilluron/lcars-api/internal/training"
	"github.com/asilluron/lcars-api/internal/weather"
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

	var audio *training.Writer
	if cfg.Training.Dir != "" {
		audio, err = training.Open(cfg.Training.Dir, cfg.Training.MaxBytes, logger)
		if err != nil {
			return fmt.Errorf("open training directory: %w", err)
		}
		// Released on the way out, including when a later startup step fails.
		defer func() { _ = audio.Close() }()
	} else {
		logger.Info("training disabled: training.dir is not set")
	}

	// Device tokens, when Supabase is configured. bus is set below; tokens
	// can only be removed once the servers are running.
	var (
		bus    *broker.Broker
		tokens *devices.Store
		admins server.AdminAuth
		auth   *broker.Auth
	)
	if cfg.Supabase.Enabled() {
		db := supabase.New(cfg.Supabase.URL, cfg.Supabase.SecretKey)
		admins = db
		tokens = devices.NewStore(db, logger, func(id string) { bus.Disconnect(id) })
		if err := tokens.Load(ctx); err != nil {
			// Keep going: Run retries, and until then devices are refused.
			logger.Error("device tokens unavailable; devices are refused until they load", "err", err)
		}
		auth = &broker.Auth{
			Check:     tokens.Check,
			Publish:   natsapi.Requests,
			Subscribe: append(slices.Clone(natsapi.Events), "_INBOX.>"),
		}
		logger.Info("device authentication enabled", "supabase", cfg.Supabase.URL)
	} else {
		logger.Warn("device authentication disabled: supabase.url is not set, so anyone on the network can use the API")
	}

	bus, err = broker.Start(cfg.NATS, auth, logger)
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

	var cams *cameras.Client
	if cfg.Eufy.URL != "" {
		cams = cameras.New(cfg.Eufy.URL)
		logger.Info("cameras enabled")
	} else {
		logger.Info("cameras disabled: eufy.url is not set")
	}

	var forecast *weather.Client
	if cfg.Weather.Enabled() {
		forecast = weather.New(cfg.Weather.URL, *cfg.Weather.Latitude, *cfg.Weather.Longitude)
		logger.Info("weather enabled")
	} else {
		logger.Info("weather disabled: weather.latitude and weather.longitude are not set")
	}

	svc, err := natsapi.Start(ctx, bus.Conn, logger, server.Version(), natsapi.Deps{
		Calendar: cal,
		Cameras:  cams,
		Training: audio,
		Weather:  forecast,
	})
	if err != nil {
		return err
	}
	defer func() { _ = svc.Stop() }()
	if cal != nil {
		go cal.Run(ctx, cfg.Calendar.RefreshInterval, natsapi.CalendarChanged(bus.Conn, logger))
	}
	if tokens != nil {
		go tokens.Run(ctx, cfg.Supabase.RefreshInterval)
	}

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: server.New(logger, server.Deps{
			Calendar: cal,
			Cameras:  cams,
			Weather:  forecast,
			NATS:     bus.Conn,
			Devices:  tokens,
			Admins:   admins,
		}),
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
