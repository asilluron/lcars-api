package server

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/asilluron/lcars-api/internal/calendar"
)

type calendarResponse struct {
	Timezone  string        `json:"timezone"`
	From      string        `json:"from"`
	To        string        `json:"to"`
	UpdatedAt time.Time     `json:"updated_at"`
	Days      []calendarDay `json:"days"`
}

type calendarDay struct {
	Date    string          `json:"date"`
	Weekday string          `json:"weekday"`
	Today   bool            `json:"today"`
	Events  []calendarEvent `json:"events"`
}

type calendarEvent struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Calendar string    `json:"calendar"`
	Location string    `json:"location"`
	AllDay   bool      `json:"all_day"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	// StartTime and EndTime are "HH:MM" labels for this day, or null; see
	// calendar.DayEvent.
	StartTime *string `json:"start_time"`
	EndTime   *string `json:"end_time"`
}

// calendarHandler lists events grouped by day, in the calendar's timezone.
// Every day in the range is present, even with no events, so a display can
// render the response directly. The same data is served over NATS on
// lcars.calendar.get (see internal/natsapi).
func calendarHandler(store *calendar.Store) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if store == nil {
			return &HTTPError{
				Code: http.StatusServiceUnavailable,
				Msg:  "calendar is not configured: set calendar.dir in the config file",
			}
		}
		q := r.URL.Query()
		days := 0 // the default
		if v := q.Get("days"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return &HTTPError{Code: http.StatusBadRequest, Msg: calendar.ErrInvalidDays.Msg}
			}
			days = n
		}
		agenda, err := store.Agenda(q.Get("from"), days, time.Now())
		if re, ok := errors.AsType[*calendar.RequestError](err); ok {
			return &HTTPError{Code: http.StatusBadRequest, Msg: re.Msg}
		} else if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, newCalendarResponse(agenda))
	}
}

func newCalendarResponse(a calendar.Agenda) calendarResponse {
	resp := calendarResponse{
		Timezone:  a.Location.String(),
		From:      a.From().Format(time.DateOnly),
		To:        a.To().Format(time.DateOnly),
		UpdatedAt: a.UpdatedAt,
		Days:      make([]calendarDay, 0, len(a.Days)),
	}
	for _, d := range a.Days {
		day := calendarDay{
			Date:    d.Date.Format(time.DateOnly),
			Weekday: d.Date.Weekday().String(),
			Today:   d.Today,
			Events:  make([]calendarEvent, 0, len(d.Events)),
		}
		for _, ev := range d.Events {
			day.Events = append(day.Events, calendarEvent{
				ID:        ev.ID,
				Title:     ev.Title,
				Calendar:  ev.Calendar,
				Location:  ev.Location,
				AllDay:    ev.AllDay,
				Start:     ev.Start,
				End:       ev.End,
				StartTime: nonEmpty(ev.StartTime),
				EndTime:   nonEmpty(ev.EndTime),
			})
		}
		resp.Days = append(resp.Days, day)
	}
	return resp
}

// nonEmpty returns a pointer to s, or nil if s is empty, for JSON null.
func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
