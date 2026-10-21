package calendar

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var newYork = mustLoad("America/New_York")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// vcal wraps VEVENT bodies in a VCALENDAR, with the CRLF line endings
// RFC 5545 requires. header lines go into the VCALENDAR itself.
func vcal(header string, events ...string) string {
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//lcars-api//test//EN\n")
	if header != "" {
		b.WriteString(strings.TrimSpace(header) + "\n")
	}
	for _, ev := range events {
		b.WriteString("BEGIN:VEVENT\n" + strings.TrimSpace(ev) + "\nEND:VEVENT\n")
	}
	b.WriteString("END:VCALENDAR\n")
	return strings.ReplaceAll(b.String(), "\n", "\r\n")
}

// writeFiles creates files (path → content) under a new temp dir.
func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func loadStore(t *testing.T, files map[string]string) *Store {
	t.Helper()
	s := NewStore(writeFiles(t, files), newYork, slog.New(slog.DiscardHandler))
	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	return s
}

// day returns midnight on 2030-03-d in New York. 4 March 2030 is a Monday,
// and US daylight saving time starts on Sunday 10 March 2030.
func day(d int) time.Time { return time.Date(2030, 3, d, 0, 0, 0, 0, newYork) }

func at(d, hour, minute int) time.Time { return time.Date(2030, 3, d, hour, minute, 0, 0, newYork) }

// summary renders events compactly for comparison: "title start→end".
func summary(events []Event) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.Title + " " + ev.Start.Format("01-02 15:04") + "→" + ev.End.Format("01-02 15:04")
	}
	return out
}

func assertEvents(t *testing.T, got []Event, want ...string) {
	t.Helper()
	g := summary(got)
	if strings.Join(g, "\n") != strings.Join(want, "\n") {
		t.Errorf("events:\n  got  %q\n  want %q", g, want)
	}
}

func TestSingleEvents(t *testing.T) {
	s := loadStore(t, map[string]string{"cal.ics": vcal("",
		"UID:tz\nSUMMARY:Zoned\nDTSTART;TZID=Europe/London:20300305T140000\nDTEND;TZID=Europe/London:20300305T150000",
		"UID:utc\nSUMMARY:UTC\nDTSTART:20300306T170000Z\nDTEND:20300306T180000Z",
		"UID:float\nSUMMARY:Floating\nDTSTART:20300307T080000\nDURATION:PT45M",
		"UID:allday\nSUMMARY:All day\nDTSTART;VALUE=DATE:20300305\nDTEND;VALUE=DATE:20300306",
		"UID:noend\nSUMMARY:No end date\nDTSTART;VALUE=DATE:20300306",
		"UID:multi\nSUMMARY:Multi day\nDTSTART;VALUE=DATE:20300307\nDTEND;VALUE=DATE:20300310",
		"UID:reminder\nSUMMARY:Instant\nDTSTART:20300308T120000",
		"UID:outside\nSUMMARY:Outside\nDTSTART:20300320T120000",
	)})

	got, _ := s.Events(day(4), day(11))
	assertEvents(t, got,
		"All day 03-05 00:00→03-06 00:00",
		"Zoned 03-05 09:00→03-05 10:00", // 14:00 London = 09:00 New York
		"No end date 03-06 00:00→03-07 00:00",
		"UTC 03-06 12:00→03-06 13:00",
		"Multi day 03-07 00:00→03-10 00:00",
		"Floating 03-07 08:00→03-07 08:45",
		"Instant 03-08 12:00→03-08 12:00",
	)
	for _, ev := range got {
		if ev.Start.Location() != newYork {
			t.Errorf("%s: start location = %v, want %v", ev.Title, ev.Start.Location(), newYork)
		}
	}
	if !got[0].AllDay || got[1].AllDay {
		t.Errorf("AllDay flags wrong: %+v", got[:2])
	}
	if got[1].ID != "tz/20300305T140000Z" {
		t.Errorf("ID = %q, want tz/20300305T140000Z", got[1].ID)
	}
}

func TestRangeOverlap(t *testing.T) {
	s := loadStore(t, map[string]string{"cal.ics": vcal("",
		"UID:a\nSUMMARY:Spans start\nDTSTART:20300304T230000\nDTEND:20300305T010000",
		"UID:b\nSUMMARY:Ends at start\nDTSTART:20300304T220000\nDTEND:20300305T000000",
		"UID:c\nSUMMARY:Starts at end\nDTSTART:20300306T000000\nDTEND:20300306T010000",
		"UID:d\nSUMMARY:Long all-day\nDTSTART;VALUE=DATE:20300301\nDTEND;VALUE=DATE:20300308",
	)})

	got, _ := s.Events(day(5), day(6))
	assertEvents(t, got,
		"Long all-day 03-01 00:00→03-08 00:00",
		"Spans start 03-04 23:00→03-05 01:00",
	)
}

func TestRecurrence(t *testing.T) {
	s := loadStore(t, map[string]string{"cal.ics": vcal("",
		// Daily at 09:00 New York time, keeping 09:00 local across the DST change.
		// Two days are excluded with one comma-separated EXDATE.
		"UID:daily\nSUMMARY:Standup\nDTSTART;TZID=America/New_York:20300301T090000\nDURATION:PT15M\n"+
			"RRULE:FREQ=DAILY\nEXDATE;TZID=America/New_York:20300305T090000,20300307T090000",
		// Weekly all-day, ending with a date-only UNTIL that must be inclusive.
		"UID:bins\nSUMMARY:Bins\nDTSTART;VALUE=DATE:20300226\nRRULE:FREQ=WEEKLY;UNTIL=20300312",
		// Limited by COUNT.
		"UID:count\nSUMMARY:Course\nDTSTART:20300304T180000\nDTEND:20300304T190000\nRRULE:FREQ=DAILY;COUNT=2",
		// RDATE-only recurrence.
		"UID:rdate\nSUMMARY:Visits\nDTSTART:20300304T120000\nDTEND:20300304T130000\nRDATE:20300308T120000",
	)})

	got, _ := s.Events(day(4), day(13))
	var standups, bins, course, visits []string
	for _, ev := range got {
		label := ev.Start.Format("01-02 15:04")
		switch ev.Title {
		case "Standup":
			standups = append(standups, label)
		case "Bins":
			bins = append(bins, label)
		case "Course":
			course = append(course, label)
		case "Visits":
			visits = append(visits, label)
		}
	}
	check := func(name string, got []string, want ...string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	check("standups", standups,
		"03-04 09:00", "03-06 09:00", "03-08 09:00", "03-09 09:00", "03-10 09:00", "03-11 09:00", "03-12 09:00")
	check("bins", bins, "03-05 00:00", "03-12 00:00")
	check("course", course, "03-04 18:00", "03-05 18:00")
	check("visits", visits, "03-04 12:00", "03-08 12:00")

	// After the DST change, 09:00 New York is 13:00 UTC instead of 14:00.
	for _, ev := range got {
		if ev.Title == "Standup" && ev.Start.Equal(at(11, 9, 0)) && ev.ID != "daily/20300311T130000Z" {
			t.Errorf("ID = %q, want daily/20300311T130000Z", ev.ID)
		}
	}
}

func TestRecurrenceOverrides(t *testing.T) {
	s := loadStore(t, map[string]string{
		"cal.ics": vcal("",
			"UID:weekly\nSUMMARY:Practice\nDTSTART:20300304T170000\nDTEND:20300304T180000\nRRULE:FREQ=DAILY;COUNT=4",
			// The second occurrence moved and renamed.
			"UID:weekly\nRECURRENCE-ID:20300305T170000\nSUMMARY:Practice (moved)\nDTSTART:20300305T190000\nDTEND:20300305T200000",
			// The third occurrence cancelled.
			"UID:weekly\nRECURRENCE-ID:20300306T170000\nSTATUS:CANCELLED\nSUMMARY:Practice\nDTSTART:20300306T170000\nDTEND:20300306T180000",
		),
		// Whole event cancelled.
		"cancelled.ics": vcal("", "UID:gone\nSTATUS:CANCELLED\nSUMMARY:Gone\nDTSTART:20300304T100000"),
	})

	got, _ := s.Events(day(4), day(11))
	assertEvents(t, got,
		"Practice 03-04 17:00→03-04 18:00",
		"Practice (moved) 03-05 19:00→03-05 20:00",
		"Practice 03-07 17:00→03-07 18:00",
	)
}

func TestTimezoneFallbacks(t *testing.T) {
	s := loadStore(t, map[string]string{"cal.ics": vcal("",
		// Prefixed IANA name, as some generators write.
		"UID:moz\nSUMMARY:Prefixed\nDTSTART;TZID=/mozilla.org/20070129_1/Europe/Paris:20300305T150000\nDURATION:PT1H",
		// Windows zone name: unknown, so read in the configured timezone.
		"UID:win\nSUMMARY:Windows\nDTSTART;TZID=\"Eastern Standard Time\":20300306T150000\nDURATION:PT1H",
	)})

	got, _ := s.Events(day(4), day(11))
	assertEvents(t, got,
		"Prefixed 03-05 09:00→03-05 10:00", // 15:00 Paris = 09:00 New York
		"Windows 03-06 15:00→03-06 16:00",
	)
}

func TestCalendarNames(t *testing.T) {
	// one is a calendar with a single event whose UID and title are name.
	one := func(name string) string {
		return vcal("", "UID:"+name+"\nSUMMARY:"+name+"\nDTSTART:20300305T100000")
	}
	s := loadStore(t, map[string]string{
		"family.ics":              one("stem"),
		"Work/a.ics":              one("subdir"),
		"Work/deeper/b.ics":       one("deep"),
		"export.ics":              vcal("X-WR-CALNAME:Holidays", "UID:named\nSUMMARY:named\nDTSTART:20300305T100000"),
		"Other/named.ics":         vcal("X-WR-CALNAME:Named wins", "UID:named2\nSUMMARY:named2\nDTSTART:20300305T100000"),
		".hidden.ics":             one("hidden"),
		".git/c.ics":              one("hiddendir"),
		"notes.txt":               "not a calendar",
		"UPPERCASE.ICS":           one("upper"),
		"Work/ignored.ics.backup": one("backup"),
	})

	got, _ := s.Events(day(4), day(11))
	names := map[string]string{}
	for _, ev := range got {
		names[ev.Title] = ev.Calendar
	}
	want := map[string]string{
		"stem":   "family",
		"subdir": "Work",
		"deep":   "Work",
		"named":  "Holidays",
		"named2": "Named wins",
		"upper":  "UPPERCASE",
	}
	if len(names) != len(want) {
		t.Errorf("got events %v, want %v", names, want)
	}
	for title, cal := range want {
		if names[title] != cal {
			t.Errorf("%s: calendar = %q, want %q", title, names[title], cal)
		}
	}
}

func TestBadFilesAreSkipped(t *testing.T) {
	s := loadStore(t, map[string]string{
		"good.ics":     vcal("", "UID:ok\nSUMMARY:Good\nDTSTART:20300305T100000"),
		"garbage.ics":  "this is not iCalendar",
		"nostart.ics":  vcal("", "UID:x\nSUMMARY:No start"),
		"badrrule.ics": vcal("", "UID:y\nSUMMARY:Bad rule\nDTSTART:20300305T100000\nRRULE:FREQ=SOMETIMES"),
	})
	got, _ := s.Events(day(4), day(11))
	assertEvents(t, got, "Good 03-05 10:00→03-05 10:00")
}

func TestLoadMissingDir(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "missing"), newYork, slog.New(slog.DiscardHandler))
	if err := s.Load(); err == nil {
		t.Error("Load: want error for missing directory")
	}
}

func TestReload(t *testing.T) {
	dir := writeFiles(t, map[string]string{"a.ics": vcal("", "UID:a\nSUMMARY:First\nDTSTART:20300305T100000")})
	s := NewStore(dir, newYork, slog.New(slog.DiscardHandler))
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	got, loaded1 := s.Events(day(4), day(11))
	assertEvents(t, got, "First 03-05 10:00→03-05 10:00")

	// New files and removed files are picked up.
	if err := os.WriteFile(filepath.Join(dir, "b.ics"),
		[]byte(vcal("", "UID:b\nSUMMARY:Second\nDTSTART:20300306T100000")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "a.ics")); err != nil {
		t.Fatal(err)
	}
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	got, loaded2 := s.Events(day(4), day(11))
	assertEvents(t, got, "Second 03-06 10:00→03-06 10:00")
	if !loaded2.After(loaded1) {
		t.Errorf("loadedAt did not advance: %v then %v", loaded1, loaded2)
	}

	// If the directory disappears, the previous events are kept.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := s.Load(); err == nil {
		t.Error("Load: want error after directory removed")
	}
	got, loaded3 := s.Events(day(4), day(11))
	assertEvents(t, got, "Second 03-06 10:00→03-06 10:00")
	if !loaded3.Equal(loaded2) {
		t.Errorf("loadedAt changed on failed load: %v → %v", loaded2, loaded3)
	}
}

func TestRun(t *testing.T) {
	dir := writeFiles(t, nil)
	s := NewStore(dir, newYork, slog.New(slog.DiscardHandler))
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	changes := make(chan time.Time, 100)
	go func() {
		s.Run(ctx, 10*time.Millisecond, func(loadedAt time.Time) { changes <- loadedAt })
		close(done)
	}()

	if err := os.WriteFile(filepath.Join(dir, "new.ics"),
		[]byte(vcal("", "UID:n\nSUMMARY:New\nDTSTART:20300305T100000")), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case loadedAt := <-changes:
		got, snapAt := s.Events(day(4), day(11))
		assertEvents(t, got, "New 03-05 10:00→03-05 10:00")
		if !loadedAt.Equal(snapAt) {
			t.Errorf("onChange time = %v, want the load time %v", loadedAt, snapAt)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not pick up the new file")
	}

	// Reloads that find nothing new don't call onChange.
	time.Sleep(100 * time.Millisecond)
	if n := len(changes); n != 0 {
		t.Errorf("onChange called %d more times with no changes", n)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestLoadChanged(t *testing.T) {
	dir := writeFiles(t, map[string]string{"a.ics": vcal("", "UID:a\nSUMMARY:First\nDTSTART:20300305T100000")})
	s := NewStore(dir, newYork, slog.New(slog.DiscardHandler))
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	steps := []struct {
		name   string
		change func()
		want   bool
	}{
		{"first load", func() {}, true},
		{"nothing changed", func() {}, false},
		{"file edited", func() { write("a.ics", vcal("", "UID:a\nSUMMARY:Edited\nDTSTART:20300305T100000")) }, true},
		{"file added", func() { write("b.ics", vcal("", "UID:b\nSUMMARY:Second\nDTSTART:20300306T100000")) }, true},
		{"non-ics file added", func() { write("notes.txt", "not a calendar") }, false},
		{"file renamed", func() {
			if err := os.Rename(filepath.Join(dir, "b.ics"), filepath.Join(dir, "c.ics")); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"file removed", func() {
			if err := os.Remove(filepath.Join(dir, "c.ics")); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"nothing changed again", func() {}, false},
	}
	for _, step := range steps {
		step.change()
		changed, err := s.load()
		if err != nil {
			t.Fatalf("%s: load: %v", step.name, err)
		}
		if changed != step.want {
			t.Errorf("%s: changed = %v, want %v", step.name, changed, step.want)
		}
	}
}

// TestConcurrentEvents exercises the snapshot lock under the race detector.
func TestConcurrentEvents(t *testing.T) {
	s := loadStore(t, map[string]string{"cal.ics": vcal("",
		"UID:r\nSUMMARY:Repeats\nDTSTART:20300301T090000\nRRULE:FREQ=DAILY\nEXDATE:20300305T090000,20300306T090000")})

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if got, _ := s.Events(day(4), day(11)); len(got) != 5 {
				t.Errorf("got %d events, want 5", len(got))
			}
		})
		wg.Go(func() {
			if err := s.Load(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}
