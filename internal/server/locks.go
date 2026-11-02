package server

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/asilluron/lcars-api/internal/locks"
)

func lockList(logger *slog.Logger, src *locks.Client) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if src == nil {
			return &HTTPError{Code: http.StatusServiceUnavailable, Msg: locks.NotConfigured}
		}
		list, err := src.List(r.Context())
		if err != nil {
			return lockHTTPError(logger, err)
		}
		if list == nil {
			list = []locks.Lock{}
		}
		return writeJSON(w, http.StatusOK, map[string]any{"locks": list})
	}
}

func lockDoor(logger *slog.Logger, src *locks.Client) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if src == nil {
			return &HTTPError{Code: http.StatusServiceUnavailable, Msg: locks.NotConfigured}
		}
		if err := src.LockDoor(r.Context(), r.PathValue("id")); err != nil {
			return lockHTTPError(logger, err)
		}
		return writeJSON(w, http.StatusOK, map[string]any{
			"id":     r.PathValue("id"),
			"locked": true,
		})
	}
}

func unlockDoor(logger *slog.Logger, src *locks.Client) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if src == nil {
			return &HTTPError{Code: http.StatusServiceUnavailable, Msg: locks.NotConfigured}
		}
		if err := src.UnlockDoor(r.Context(), r.PathValue("id")); err != nil {
			return lockHTTPError(logger, err)
		}
		return writeJSON(w, http.StatusOK, map[string]any{
			"id":     r.PathValue("id"),
			"locked": false,
		})
	}
}

func lockHTTPError(logger *slog.Logger, err error) error {
	se, ok := errors.AsType[*locks.StatusError](err)
	if !ok {
		return err
	}
	if se.Err != nil {
		logger.Warn("seam", "err", se.Err)
	}
	return &HTTPError{Code: se.Code, Msg: se.Msg}
}
