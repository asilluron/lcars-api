package server

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/asilluron/lcars-api/internal/calendar"
)

const (
	defaultCalendarDays = 7
	maxCalendarDays     = 62
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
	// StartTime and EndTime are "HH:MM" labels for this day: set only when a
	// timed event starts or ends on this day, so a multi-day event shows its
	// start time on the first day and its end time on the last.
	StartTime *string `json:"start_time"`
	EndTime   *string `json:"end_time"`
}

// calendarHandler lists events grouped by day, in the calendar's timezone.
// Every day in the range is present, even with no events, so a display can
// render the response directly.
func calendarHandler(store *calendar.Store) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if store == nil {
			return &HTTPError{
				Code: http.StatusServiceUnavailable,
				Msg:  "calendar is not configured: set calendar.dir in the config file",
			}
		}
		loc := store.Location()
		now := time.Now().In(loc)
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

		q := r.URL.Query()
		from := today
		if v := q.Get("from"); v != "" {
			t, err := time.ParseInLocation(time.DateOnly, v, loc)
			if err != nil {
				return &HTTPError{Code: http.StatusBadRequest, Msg: `invalid "from": want a date like 2026-10-20`}
			}
			from = t
		}
		days := defaultCalendarDays
		if v := q.Get("days"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > maxCalendarDays {
				return &HTTPError{
					Code: http.StatusBadRequest,
					Msg:  fmt.Sprintf(`invalid "days": want a whole number from 1 to %d`, maxCalendarDays),
				}
			}
			days = n
		}

		events, loadedAt := store.Events(from, from.AddDate(0, 0, days))
		resp := calendarResponse{
			Timezone:  loc.String(),
			From:      from.Format(time.DateOnly),
			To:        from.AddDate(0, 0, days-1).Format(time.DateOnly),
			UpdatedAt: loadedAt.In(loc),
			Days:      make([]calendarDay, 0, days),
		}
		for i := range days {
			start, end := from.AddDate(0, 0, i), from.AddDate(0, 0, i+1)
			day := calendarDay{
				Date:    start.Format(time.DateOnly),
				Weekday: start.Weekday().String(),
				Today:   start.Equal(today),
				Events:  []calendarEvent{},
			}
			for _, ev := range events {
				if ev.Overlaps(start, end) {
					day.Events = append(day.Events, newCalendarEvent(ev, start, end))
				}
			}
			// All-day events first; events keep the store's order otherwise.
			slices.SortStableFunc(day.Events, func(a, b calendarEvent) int {
				switch {
				case a.AllDay == b.AllDay:
					return 0
				case a.AllDay:
					return -1
				default:
					return 1
				}
			})
			resp.Days = append(resp.Days, day)
		}
		return writeJSON(w, http.StatusOK, resp)
	}
}

// newCalendarEvent converts ev for display on the day [dayStart, dayEnd).
func newCalendarEvent(ev calendar.Event, dayStart, dayEnd time.Time) calendarEvent {
	out := calendarEvent{
		ID:       ev.ID,
		Title:    ev.Title,
		Calendar: ev.Calendar,
		Location: ev.Location,
		AllDay:   ev.AllDay,
		Start:    ev.Start,
		End:      ev.End,
	}
	if ev.AllDay {
		return out
	}
	if !ev.Start.Before(dayStart) && ev.Start.Before(dayEnd) {
		out.StartTime = new(ev.Start.Format("15:04"))
	}
	if ev.End.After(dayStart) && ev.End.Before(dayEnd) || ev.End.Equal(ev.Start) {
		out.EndTime = new(ev.End.Format("15:04"))
	} else if ev.End.Equal(dayEnd) {
		out.EndTime = new("24:00")
	}
	return out
}
