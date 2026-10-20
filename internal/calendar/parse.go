package calendar

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"
)

// series is one VEVENT: a single event, or a recurring one and its rule.
type series struct {
	uid      string
	title    string
	location string
	calendar string

	start  time.Time
	allDay bool
	days   int           // length of an all-day event, in calendar days
	dur    time.Duration // length of a timed event

	recur *rrule.Set // nil for a single event
}

// end returns when the occurrence starting at start ends (exclusive).
func (s *series) end(start time.Time) time.Time {
	if s.allDay {
		return start.AddDate(0, 0, s.days)
	}
	return start.Add(s.dur)
}

// maxLen is an upper bound on the length of any occurrence, used to find
// occurrences that start before a range but run into it.
func (s *series) maxLen() time.Duration {
	if s.allDay {
		return time.Duration(s.days)*24*time.Hour + time.Hour // DST slack
	}
	return s.dur
}

// parsed is what one .ics file contributes to a calendar.
type parsed struct {
	series []*series
	// replaced lists, by UID, occurrences of a recurring event that another
	// VEVENT with a RECURRENCE-ID overrides or cancels.
	replaced map[string][]time.Time
}

// parse reads every VCALENDAR in r. Times without a timezone (floating times
// and all-day dates) are interpreted in loc. calendar names the events'
// calendar unless the file sets X-WR-CALNAME.
func parse(r io.Reader, calendar string, loc *time.Location) (*parsed, error) {
	out := &parsed{replaced: map[string][]time.Time{}}
	dec := ical.NewDecoder(r)
	for {
		cal, err := dec.Decode()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		name := calendar
		if v, err := cal.Props.Text("X-WR-CALNAME"); err == nil && strings.TrimSpace(v) != "" {
			name = strings.TrimSpace(v)
		}
		for _, ev := range cal.Events() {
			if err := out.add(ev, name, loc); err != nil {
				uid, _ := ev.Props.Text(ical.PropUID)
				return nil, fmt.Errorf("event %q: %w", uid, err)
			}
		}
	}
	return out, nil
}

func (p *parsed) add(ev ical.Event, calendar string, loc *time.Location) error {
	fixTimezones(ev.Component)

	uid, _ := ev.Props.Text(ical.PropUID)
	status, _ := ev.Status()
	if rid := ev.Props.Get(ical.PropRecurrenceID); rid != nil && uid != "" {
		t, err := rid.DateTime(loc)
		if err != nil {
			return fmt.Errorf("RECURRENCE-ID: %w", err)
		}
		p.replaced[uid] = append(p.replaced[uid], t)
	}
	if status == ical.EventCancelled {
		return nil
	}

	startProp := ev.Props.Get(ical.PropDateTimeStart)
	if startProp == nil {
		return errors.New("missing DTSTART")
	}
	start, err := startProp.DateTime(loc)
	if err != nil {
		return fmt.Errorf("DTSTART: %w", err)
	}
	end, err := ev.DateTimeEnd(loc)
	if err != nil {
		return fmt.Errorf("DTEND: %w", err)
	}

	s := &series{
		uid:      uid,
		calendar: calendar,
		start:    start,
		allDay:   isDate(startProp),
	}
	s.title, _ = ev.Props.Text(ical.PropSummary)
	s.location, _ = ev.Props.Text(ical.PropLocation)
	if s.allDay {
		s.days = max(1, daysBetween(start, end))
	} else {
		s.dur = max(0, end.Sub(start))
	}

	s.recur, err = recurrence(ev.Component, start, loc)
	if err != nil {
		return err
	}
	p.series = append(p.series, s)
	return nil
}

// recurrence returns the event's recurrence set, or nil if it does not repeat.
// Unlike ical.Component.RecurrenceSet it handles comma-separated EXDATE and
// RDATE lists, which some clients (including Apple Calendar) write, and reads
// a date-only UNTIL in the event's timezone rather than UTC, so the last
// occurrence of an all-day series isn't dropped.
func recurrence(comp *ical.Component, start time.Time, loc *time.Location) (*rrule.Set, error) {
	var opt *rrule.ROption
	if prop := comp.Props.Get(ical.PropRecurrenceRule); prop != nil {
		var err error
		if opt, err = rrule.StrToROptionInLocation(prop.Value, start.Location()); err != nil {
			return nil, fmt.Errorf("RRULE: %w", err)
		}
	}
	rdates, err := dateList(comp.Props[ical.PropRecurrenceDates], loc)
	if err != nil {
		return nil, fmt.Errorf("RDATE: %w", err)
	}
	if opt == nil && len(rdates) == 0 {
		return nil, nil
	}
	exdates, err := dateList(comp.Props[ical.PropExceptionDates], loc)
	if err != nil {
		return nil, fmt.Errorf("EXDATE: %w", err)
	}

	set := &rrule.Set{}
	if opt != nil {
		opt.Dtstart = start
		rule, err := rrule.NewRRule(*opt)
		if err != nil {
			return nil, fmt.Errorf("RRULE: %w", err)
		}
		set.RRule(rule)
	} else {
		set.RDate(start) // with only RDATEs, DTSTART is still the first occurrence
	}
	for _, t := range rdates {
		set.RDate(t)
	}
	for _, t := range exdates {
		set.ExDate(t)
	}
	return set, nil
}

// dateList parses every value of every prop, splitting comma-separated lists.
func dateList(props []ical.Prop, loc *time.Location) ([]time.Time, error) {
	var out []time.Time
	for _, prop := range props {
		for v := range strings.SplitSeq(prop.Value, ",") {
			one := prop
			one.Value = strings.TrimSpace(v)
			t, err := one.DateTime(loc)
			if err != nil {
				return nil, err
			}
			out = append(out, t)
		}
	}
	return out, nil
}

func isDate(prop *ical.Prop) bool {
	return prop.ValueType() == ical.ValueDate ||
		(prop.ValueType() == ical.ValueDefault && len(prop.Value) == len("20060102"))
}

// daysBetween counts calendar days from a to b, ignoring time of day and DST.
func daysBetween(a, b time.Time) int {
	da := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	db := time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
	return int(db.Sub(da).Hours() / 24)
}

// timeProps are the properties whose TZID parameter fixTimezones repairs.
var timeProps = []string{
	ical.PropDateTimeStart, ical.PropDateTimeEnd, ical.PropRecurrenceID,
	ical.PropExceptionDates, ical.PropRecurrenceDates,
}

// fixTimezones rewrites TZID parameters that are not IANA names so go-ical can
// load them. Some generators prefix the name ("/mozilla.org/20070129_1/Europe/Paris");
// those are trimmed. Names that still don't resolve, such as Windows zone names
// from Outlook, are dropped, so the time is read in the configured timezone.
func fixTimezones(comp *ical.Component) {
	for _, name := range timeProps {
		for i := range comp.Props[name] {
			params := comp.Props[name][i].Params
			tzid := params.Get(ical.PropTimezoneID)
			if tzid == "" {
				continue
			}
			if iana, ok := ianaZone(tzid); ok {
				params.Set(ical.PropTimezoneID, iana)
			} else {
				params.Del(ical.PropTimezoneID)
			}
		}
	}
}

func ianaZone(tzid string) (string, bool) {
	name := strings.Trim(tzid, `"`)
	for {
		if _, err := time.LoadLocation(name); err == nil && name != "" && name != "Local" {
			return name, true
		}
		_, rest, ok := strings.Cut(name, "/")
		if !ok {
			return "", false
		}
		name = rest
	}
}
