package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/asilluron/lcars-api/internal/calendar"
	"github.com/asilluron/lcars-api/internal/supabase"
)

// CalendarLabelDB stores the display names and lanes set on the admin page.
// *supabase.Client implements it.
type CalendarLabelDB interface {
	ListCalendarLabels(ctx context.Context) ([]supabase.CalendarLabel, error)
	UpsertCalendarLabel(ctx context.Context, row supabase.CalendarLabel) error
	DeleteCalendarLabel(ctx context.Context, id string) error
}

// ReloadCalendarLabels reads labels from db into store. It reports whether
// the labels in effect changed. A database error leaves the previous labels
// in place.
func ReloadCalendarLabels(ctx context.Context, db CalendarLabelDB, store *calendar.Store) (bool, error) {
	rows, err := db.ListCalendarLabels(ctx)
	if err != nil {
		return false, err
	}
	labels := make(map[string]calendar.Label, len(rows))
	for _, row := range rows {
		labels[row.ID] = calendar.Label{Name: row.Name, Category: row.Category}
	}
	return store.SetLabels(labels), nil
}

type calendarSource struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	Title    string `json:"title"`
	Path     string `json:"path"`
	Name     string `json:"name"`
	Category string `json:"category"`
}

func listCalendars(_ *slog.Logger, deps Deps) adminHandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, _ supabase.User) error {
		if err := calendarsReady(deps); err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, map[string][]calendarSource{
			"calendars": calendarSources(deps.Calendar),
		})
	}
}

type saveCalendarRequest struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
}

func saveCalendar(logger *slog.Logger, deps Deps) adminHandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, _ supabase.User) error {
		if err := calendarsReady(deps); err != nil {
			return err
		}
		var req saveCalendarRequest
		if err := decodeJSON(w, r, &req); err != nil {
			return err
		}
		req.ID = strings.TrimSpace(req.ID)
		req.Name = strings.TrimSpace(req.Name)
		req.Category = strings.ToLower(strings.TrimSpace(req.Category))
		if req.ID == "" || utf8.RuneCountInString(req.ID) > 200 {
			return &HTTPError{Code: http.StatusBadRequest, Msg: `invalid "id"`}
		}
		if utf8.RuneCountInString(req.Name) > calendar.MaxLabelLen || strings.ContainsFunc(req.Name, unicode.IsControl) {
			return &HTTPError{Code: http.StatusBadRequest, Msg: `invalid "name": must be at most 40 characters`}
		}
		if req.Category != "" && !calendar.KnownCategory(req.Category) {
			return &HTTPError{Code: http.StatusBadRequest, Msg: `invalid "category": want work, band, personal or house`}
		}
		if !knownCalendar(deps.Calendar, req.ID) {
			return &HTTPError{Code: http.StatusNotFound, Msg: "no such calendar"}
		}

		var err error
		if req.Name == "" && req.Category == "" {
			err = deps.Labels.DeleteCalendarLabel(r.Context(), req.ID)
			if errors.Is(err, supabase.ErrNotFound) {
				err = nil
			}
		} else {
			err = deps.Labels.UpsertCalendarLabel(r.Context(), supabase.CalendarLabel{
				ID: req.ID, Name: req.Name, Category: req.Category,
			})
		}
		if err != nil {
			return authUnavailable(r.Context(), logger, err)
		}
		changed, err := ReloadCalendarLabels(r.Context(), deps.Labels, deps.Calendar)
		if err != nil {
			return authUnavailable(r.Context(), logger, err)
		}
		if changed && deps.CalendarChanged != nil {
			deps.CalendarChanged(time.Now())
		}
		for _, entry := range calendarSources(deps.Calendar) {
			if entry.ID == req.ID {
				return writeJSON(w, http.StatusOK, entry)
			}
		}
		return &HTTPError{Code: http.StatusNotFound, Msg: "no such calendar"}
	}
}

func calendarsReady(deps Deps) error {
	if deps.Calendar == nil {
		return &HTTPError{
			Code: http.StatusServiceUnavailable,
			Msg:  "calendar is not configured: set calendar.dir in the config file",
		}
	}
	if deps.Labels == nil {
		return errAdminDisabled
	}
	return nil
}

func knownCalendar(store *calendar.Store, id string) bool {
	for _, entry := range store.Catalog() {
		if entry.ID == id {
			return true
		}
	}
	return false
}

func calendarSources(store *calendar.Store) []calendarSource {
	out := make([]calendarSource, 0)
	for _, entry := range store.Catalog() {
		var name, category string
		if label, ok := store.Label(entry.ID); ok {
			name, category = label.Name, label.Category
		}
		out = append(out, calendarSource{
			ID:       entry.ID,
			Source:   entry.Source,
			Title:    entry.Title,
			Path:     entry.Path,
			Name:     name,
			Category: category,
		})
	}
	return out
}
