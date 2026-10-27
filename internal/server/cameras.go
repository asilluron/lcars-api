package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/asilluron/lcars-api/internal/cameras"
)

func cameraList(logger *slog.Logger, src *cameras.Client) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		cams, err := loadCameras(r.Context(), src)
		if err != nil {
			return cameraHTTPError(logger, err)
		}
		if cams == nil {
			cams = []cameras.Camera{}
		}
		return writeJSON(w, http.StatusOK, map[string]any{"cameras": cams})
	}
}

func cameraStream(logger *slog.Logger, src *cameras.Client) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if src == nil {
			return &HTTPError{Code: http.StatusServiceUnavailable, Msg: cameras.NotConfigured}
		}
		body, err := src.OpenStream(r.Context(), r.PathValue("sn"))
		if err != nil {
			return cameraHTTPError(logger, err)
		}
		defer body.Close()

		// A live view outlives the server's write timeout. Clear both
		// deadlines before the first byte; the device closing the response
		// is what ends it.
		rc := http.NewResponseController(w)
		if err := rc.SetWriteDeadline(time.Time{}); err != nil {
			return err
		}
		if err := rc.SetReadDeadline(time.Time{}); err != nil {
			return err
		}
		w.Header().Set("Content-Type", "video/octet-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)

		buf := make([]byte, 32<<10)
		for {
			n, rerr := body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return nil
				}
				if ferr := rc.Flush(); ferr != nil {
					return nil
				}
			}
			if rerr != nil {
				if !errors.Is(rerr, io.EOF) && r.Context().Err() == nil {
					logger.Warn("camera stream ended", "err", rerr)
				}
				return nil
			}
		}
	}
}

func cameraSnapshot(logger *slog.Logger, src *cameras.Client) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if src == nil {
			return &HTTPError{Code: http.StatusServiceUnavailable, Msg: cameras.NotConfigured}
		}
		// A live-still fallback on the proxy can take longer than the
		// server's 15s write timeout. Clear both deadlines before waiting
		// on the proxy; the cameras client still bounds the wait.
		rc := http.NewResponseController(w)
		if err := rc.SetWriteDeadline(time.Time{}); err != nil {
			return err
		}
		if err := rc.SetReadDeadline(time.Time{}); err != nil {
			return err
		}
		data, err := src.Snapshot(r.Context(), r.PathValue("sn"))
		if err != nil {
			return cameraHTTPError(logger, err)
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		// The body is already committed. A write error is the client going
		// away, and a JSON error after the JPEG would not be readable.
		_, _ = w.Write(data) //nolint:gosec // JPEG from the configured camera proxy, not markup
		return nil
	}
}

// loadCameras reads the catalog. A nil client means cameras are not configured.
func loadCameras(ctx context.Context, src *cameras.Client) ([]cameras.Camera, error) {
	if src == nil {
		return nil, &cameras.StatusError{Code: http.StatusServiceUnavailable, Msg: cameras.NotConfigured}
	}
	return src.List(ctx)
}

// cameraHTTPError turns a client error into a response. A *cameras.StatusError
// is sent as-is, after its cause is logged. Anything else is returned so the
// handler wrapper logs it and answers 500.
func cameraHTTPError(logger *slog.Logger, err error) error {
	se, ok := errors.AsType[*cameras.StatusError](err)
	if !ok {
		return err
	}
	if se.Err != nil {
		logger.Warn("camera proxy", "err", se.Err)
	}
	return &HTTPError{Code: se.Code, Msg: se.Msg}
}
