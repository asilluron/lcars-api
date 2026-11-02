package server

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/asilluron/lcars-api/internal/reminders"
	"github.com/asilluron/lcars-api/internal/supabase"
)

type reminderResponse struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Accent string `json:"accent"`
	Done   bool   `json:"done"`
}

type remindersResponse struct {
	UpdatedAt time.Time          `json:"updated_at"`
	Reminders []reminderResponse `json:"reminders"`
}

func newRemindersResponse(items []reminders.Reminder, updated time.Time) remindersResponse {
	out := remindersResponse{
		UpdatedAt: updated.UTC(),
		Reminders: make([]reminderResponse, len(items)),
	}
	for i, item := range items {
		out.Reminders[i] = reminderResponse{
			ID: item.ID, Title: item.Title, Detail: item.Detail, Accent: item.Accent, Done: item.Done,
		}
	}
	return out
}

func remindersUnavailable() *HTTPError {
	return &HTTPError{Code: http.StatusServiceUnavailable, Msg: reminders.NotConfigured}
}

func reminderHTTPError(err error) error {
	var re *reminders.RequestError
	switch {
	case errors.As(err, &re):
		return &HTTPError{Code: http.StatusBadRequest, Msg: re.Msg}
	case errors.Is(err, reminders.ErrNotFound):
		return &HTTPError{Code: http.StatusNotFound, Msg: reminders.ErrNotFound.Error()}
	default:
		return err
	}
}

// listReminders is GET /reminders, the same list as lcars.reminders.list.
func listReminders(store *reminders.Store) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if store == nil {
			return remindersUnavailable()
		}
		items, updated, err := store.List(r.Context())
		if err != nil {
			return reminderHTTPError(err)
		}
		return writeJSON(w, http.StatusOK, newRemindersResponse(items, updated))
	}
}

// setReminder is POST /reminders/{id}. The id is a lookup key.
func setReminder(store *reminders.Store) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if store == nil {
			return remindersUnavailable()
		}
		var req struct {
			Done *bool `json:"done"`
		}
		if err := decodeJSON(w, r, &req); err != nil {
			return err
		}
		if req.Done == nil {
			return &HTTPError{Code: http.StatusBadRequest, Msg: `"done" is required`}
		}
		if err := store.SetDone(r.Context(), r.PathValue("id"), *req.Done); err != nil {
			return reminderHTTPError(err)
		}
		items, updated, err := store.List(r.Context())
		if err != nil {
			return reminderHTTPError(err)
		}
		return writeJSON(w, http.StatusOK, newRemindersResponse(items, updated))
	}
}

func listRemindersAdmin(store *reminders.Store) adminHandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, _ supabase.User) error {
		if store == nil {
			return remindersUnavailable()
		}
		items, updated, err := store.List(r.Context())
		if err != nil {
			return reminderHTTPError(err)
		}
		return writeJSON(w, http.StatusOK, newRemindersResponse(items, updated))
	}
}

func createReminder(logger *slog.Logger, store *reminders.Store) adminHandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, admin supabase.User) error {
		if store == nil {
			return remindersUnavailable()
		}
		var req struct {
			Title  string `json:"title"`
			Detail string `json:"detail"`
			Accent string `json:"accent"`
		}
		if err := decodeJSON(w, r, &req); err != nil {
			return err
		}
		item, err := store.Create(r.Context(), req.Title, req.Detail, req.Accent)
		if err != nil {
			return reminderHTTPError(err)
		}
		logger.InfoContext(r.Context(), "reminder created", "user_id", admin.ID)
		return writeJSON(w, http.StatusCreated, reminderResponse{
			ID: item.ID, Title: item.Title, Detail: item.Detail, Accent: item.Accent, Done: item.Done,
		})
	}
}

func deleteReminder(logger *slog.Logger, store *reminders.Store) adminHandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, admin supabase.User) error {
		if store == nil {
			return remindersUnavailable()
		}
		if err := store.Delete(r.Context(), r.PathValue("id")); err != nil {
			return reminderHTTPError(err)
		}
		logger.InfoContext(r.Context(), "reminder deleted", "user_id", admin.ID)
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
}
