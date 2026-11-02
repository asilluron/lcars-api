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
	"time"

	"github.com/asilluron/lcars-api/internal/broker"
	"github.com/asilluron/lcars-api/internal/calendar"
	"github.com/asilluron/lcars-api/internal/cameras"
	"github.com/asilluron/lcars-api/internal/config"
	"github.com/asilluron/lcars-api/internal/devices"
	"github.com/asilluron/lcars-api/internal/locks"
	"github.com/asilluron/lcars-api/internal/natsapi"
	"github.com/asilluron/lcars-api/internal/server"
	"github.com/asilluron/lcars-api/internal/supabase"
	"github.com/asilluron/lcars-api/internal/training"
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
		labels server.CalendarLabelDB
		auth   *broker.Auth
	)
	if cfg.Supabase.Enabled() {
		db := supabase.New(cfg.Supabase.URL, cfg.Supabase.SecretKey)
		admins = db
		labels = db
		tokens = devices.NewStore(db, logger, func(id string) { bus.Disconnect(id) })
		if err := tokens.Load(ctx); err != nil {
			// Keep going: Run retries, and until then devices are refused.
			logger.Error("device tokens unavailable; devices are refused until they load", "err", err)
		}
		if cal != nil {
			if _, err := server.ReloadCalendarLabels(ctx, db, cal); err != nil {
				logger.Error("calendar labels unavailable", "err", err)
			}
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

	var doorLocks *locks.Client
	if cfg.Seam.Enabled() {
		base := cfg.Seam.URL
		if base == "" {
			base = locks.DefaultURL
		}
		doorLocks = locks.New(base, cfg.Seam.APIKey)
		logger.Info("locks enabled")
	} else {
		logger.Info("locks disabled: seam.api_key is not set")
	}

	svc, err := natsapi.Start(ctx, bus.Conn, logger, server.Version(), natsapi.Deps{
		Calendar: cal,
		Cameras:  cams,
		Locks:    doorLocks,
		Training: audio,
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
	if labels != nil && cal != nil {
		go refreshCalendarLabels(ctx, cfg.Supabase.RefreshInterval, labels, cal,
			natsapi.CalendarChanged(bus.Conn, logger), logger)
	}

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: server.New(logger, server.Deps{
			Calendar:        cal,
			Cameras:         cams,
			Locks:           doorLocks,
			NATS:            bus.Conn,
			Devices:         tokens,
			Admins:          admins,
			Labels:          labels,
			CalendarChanged: natsapi.CalendarChanged(bus.Conn, logger),
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

// refreshCalendarLabels re-reads labels until ctx is done, and publishes a
// calendar update when they change, so a display picks up a name edited
// directly in the database.
func refreshCalendarLabels(ctx context.Context, every time.Duration, db server.CalendarLabelDB, store *calendar.Store, notify func(time.Time), logger *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			changed, err := server.ReloadCalendarLabels(ctx, db, store)
			if err != nil {
				logger.ErrorContext(ctx, "calendar labels unavailable", "err", err)
				continue
			}
			if changed {
				logger.InfoContext(ctx, "calendar labels loaded")
				if notify != nil {
					notify(time.Now())
				}
			}
		}
	}
}

func newLogger(cfg config.Config, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}
