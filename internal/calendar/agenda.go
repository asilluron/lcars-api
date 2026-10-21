package calendar

import (
	"fmt"
	"slices"
	"time"
)

const (
	// DefaultDays is how many days an Agenda covers when none are asked for.
	DefaultDays = 7
	// MaxDays is the most days one Agenda can cover.
	MaxDays = 62
)

// RequestError reports an invalid Agenda request. Its message is meant for
// the client that made the request.
type RequestError struct{ Msg string }

func (e *RequestError) Error() string { return e.Msg }

var (
	// ErrInvalidFrom is returned for a start date that isn't YYYY-MM-DD.
	ErrInvalidFrom = &RequestError{`invalid "from": want a date like 2026-10-20`}
	// ErrInvalidDays is returned for a day count out of range.
	ErrInvalidDays = &RequestError{fmt.Sprintf(`invalid "days": want a whole number from 1 to %d`, MaxDays)}
)

// Agenda is the events in a range of days, grouped by day and ready to draw.
type Agenda struct {
	Location *time.Location
	// UpdatedAt is when the .ics files were last read.
	UpdatedAt time.Time
	// Days has one entry per day in the range, even days with no events.
	Days []Day
}

// From returns the first day of the agenda.
func (a Agenda) From() time.Time { return a.Days[0].Date }

// To returns the last day of the agenda (inclusive).
func (a Agenda) To() time.Time { return a.Days[len(a.Days)-1].Date }

// Day is one day of an Agenda.
type Day struct {
	// Date is midnight at the start of the day, in the Agenda's location.
	Date  time.Time
	Today bool
	// Events overlap this day: all-day events first, then by start time.
	Events []DayEvent
}

// DayEvent is an event as shown on one day.
type DayEvent struct {
	Event
	// StartTime and EndTime are "HH:MM" labels for this day, or "". They are
	// set only when a timed event starts or ends on this day, so a multi-day
	// event shows its start time on the first day and its end time on the
	// last. An event ending at midnight ends at "24:00".
	StartTime string
	EndTime   string
}

// Agenda returns the events for days days starting on from, a YYYY-MM-DD date
// in the store's location. An empty from means the day containing now, and
// days == 0 means DefaultDays. Invalid arguments return a *RequestError.
func (s *Store) Agenda(from string, days int, now time.Time) (Agenda, error) {
	loc := s.loc
	now = now.In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	start := today
	if from != "" {
		t, err := time.ParseInLocation(time.DateOnly, from, loc)
		if err != nil {
			return Agenda{}, ErrInvalidFrom
		}
		start = t
	}
	switch {
	case days == 0:
		days = DefaultDays
	case days < 0 || days > MaxDays:
		return Agenda{}, ErrInvalidDays
	}

	events, loadedAt := s.Events(start, start.AddDate(0, 0, days))
	a := Agenda{Location: loc, UpdatedAt: loadedAt.In(loc), Days: make([]Day, 0, days)}
	for i := range days {
		dayStart, dayEnd := start.AddDate(0, 0, i), start.AddDate(0, 0, i+1)
		day := Day{Date: dayStart, Today: dayStart.Equal(today), Events: []DayEvent{}}
		for _, ev := range events {
			if ev.Overlaps(dayStart, dayEnd) {
				day.Events = append(day.Events, newDayEvent(ev, dayStart, dayEnd))
			}
		}
		// All-day events first; events keep the store's order otherwise.
		slices.SortStableFunc(day.Events, func(a, b DayEvent) int {
			return compareBool(b.AllDay, a.AllDay)
		})
		a.Days = append(a.Days, day)
	}
	return a, nil
}

// newDayEvent shows ev on the day [dayStart, dayEnd).
func newDayEvent(ev Event, dayStart, dayEnd time.Time) DayEvent {
	out := DayEvent{Event: ev}
	if ev.AllDay {
		return out
	}
	if !ev.Start.Before(dayStart) && ev.Start.Before(dayEnd) {
		out.StartTime = ev.Start.Format("15:04")
	}
	if ev.End.After(dayStart) && ev.End.Before(dayEnd) || ev.End.Equal(ev.Start) {
		out.EndTime = ev.End.Format("15:04")
	} else if ev.End.Equal(dayEnd) {
		out.EndTime = "24:00"
	}
	return out
}
