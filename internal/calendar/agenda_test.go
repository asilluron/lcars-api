package calendar

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestAgenda(t *testing.T) {
	s := loadStore(t, map[string]string{"home.ics": vcal("",
		"UID:standup\nSUMMARY:Standup\nDTSTART:20300305T090000\nDTEND:20300305T091500",
		"UID:bins\nSUMMARY:Bins\nDTSTART;VALUE=DATE:20300305",
		"UID:flight\nSUMMARY:Flight\nDTSTART:20300305T190000\nDTEND:20300307T073000",
		"UID:late\nSUMMARY:Late show\nDTSTART:20300304T230000\nDTEND:20300305T000000",
		"UID:alarm\nSUMMARY:Alarm\nDTSTART:20300307T060000",
	)})
	a, err := s.Agenda("2030-03-04", 4, at(5, 12, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !a.From().Equal(day(4)) || !a.To().Equal(day(7)) || a.Location != newYork {
		t.Errorf("agenda %v..%v in %v", a.From(), a.To(), a.Location)
	}
	var got []string
	for _, d := range a.Days {
		parts := []string{d.Date.Format("01-02")}
		if d.Today {
			parts = append(parts, "(today)")
		}
		for _, ev := range d.Events {
			parts = append(parts, ev.Title+"["+ev.StartTime+"-"+ev.EndTime+"]")
		}
		got = append(got, strings.Join(parts, " "))
	}
	want := []string{
		"03-04 Late show[23:00-24:00]",
		"03-05 (today) Bins[-] Standup[09:00-09:15] Flight[19:00-]",
		"03-06 Flight[-]",
		// Ongoing events come first: they started earlier.
		"03-07 Flight[-07:30] Alarm[06:00-06:00]",
	}
	if !slices.Equal(got, want) {
		t.Errorf("agenda:\n got %q\nwant %q", got, want)
	}
}

func TestAgendaDefaults(t *testing.T) {
	s := loadStore(t, nil)
	// Late evening in New York is already the next day in UTC; "today" must
	// follow the store's location.
	now := at(5, 23, 30)
	a, err := s.Agenda("", 0, now.UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Days) != DefaultDays || !a.From().Equal(day(5)) || !a.Days[0].Today {
		t.Errorf("got %d days from %v (today=%v), want %d from 03-05 (today)",
			len(a.Days), a.From(), a.Days[0].Today, DefaultDays)
	}
	for _, d := range a.Days {
		if d.Events == nil {
			t.Errorf("%v: Events is nil, want empty", d.Date)
		}
	}
}

func TestAgendaInvalid(t *testing.T) {
	s := loadStore(t, nil)
	for _, tt := range []struct {
		from string
		days int
		want error
	}{
		{"tomorrow", 1, ErrInvalidFrom},
		{"2030-02-30", 1, ErrInvalidFrom},
		{"2030-03-04T00:00:00Z", 1, ErrInvalidFrom},
		{"", -1, ErrInvalidDays},
		{"", MaxDays + 1, ErrInvalidDays},
	} {
		_, err := s.Agenda(tt.from, tt.days, at(5, 12, 0))
		if !errors.Is(err, tt.want) {
			t.Errorf("Agenda(%q, %d) = %v, want %v", tt.from, tt.days, err, tt.want)
		}
	}
	if a, err := s.Agenda("", MaxDays, at(5, 12, 0)); err != nil || len(a.Days) != MaxDays {
		t.Errorf("Agenda(MaxDays) = %d days, %v", len(a.Days), err)
	}
}
