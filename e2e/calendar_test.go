//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nats-io/nats.go/micro"
	"google.golang.org/protobuf/proto"

	lcarsv1 "github.com/asilluron/lcars-api/gen/go/lcars/v1"
)

// calendarFixtures holds fictional .ics files for the week of 4–10 March
// 2030, which includes the start of US daylight saving time (10 March). It
// also holds one invalid .ics file and one non-.ics file, which must be
// skipped.
const calendarFixtures = "testdata/calendars"

// fixtureWeek is what GET /calendar?from=2030-03-04&days=7 returns for the
// fixtures, in America/New_York, one line per day.
var fixtureWeek = []string{
	"2030-03-04 Monday: work/Standup 09:00-09:15",
	"2030-03-05 Tuesday: work/Standup 09:00-09:15, Family/Soccer practice (field 2) 18:00-19:00",
	"2030-03-06 Wednesday: work/Standup 09:00-09:15, work/Design review 10:00-11:00",
	"2030-03-07 Thursday: work/Standup 09:00-09:15",
	"2030-03-08 Friday: Family/Grandparents visit all-day, work/Standup 09:00-09:15, Family/Pizza night 19:00-20:30",
	"2030-03-09 Saturday: Family/Grandparents visit all-day, Family/Morning run 07:00-07:30",
	"2030-03-10 Sunday: Family/Grandparents visit all-day, Family/Morning run 07:00-07:30",
}

type calendarResponse struct {
	Timezone  string    `json:"timezone"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	UpdatedAt time.Time `json:"updated_at"`
	Days      []struct {
		Date    string `json:"date"`
		Weekday string `json:"weekday"`
		Today   bool   `json:"today"`
		Events  []struct {
			ID        string  `json:"id"`
			Title     string  `json:"title"`
			Calendar  string  `json:"calendar"`
			Location  string  `json:"location"`
			AllDay    bool    `json:"all_day"`
			Start     string  `json:"start"`
			End       string  `json:"end"`
			StartTime *string `json:"start_time"`
			EndTime   *string `json:"end_time"`
		} `json:"events"`
	} `json:"days"`
}

func getCalendar(t *testing.T, url string) calendarResponse {
	t.Helper()
	resp := mustDo(t, http.MethodGet, url)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d %s, want 200", url, resp.StatusCode, resp.body)
	}
	var cal calendarResponse
	if err := json.Unmarshal(resp.body, &cal); err != nil {
		t.Fatalf("decode %s: %v", resp.body, err)
	}
	return cal
}

// render formats a calendar response like fixtureWeek.
func render(cal calendarResponse) []string {
	var out []string
	for _, d := range cal.Days {
		var evs []string
		for _, ev := range d.Events {
			when := "all-day"
			if !ev.AllDay {
				when = deref(ev.StartTime) + "-" + deref(ev.EndTime)
			}
			evs = append(evs, ev.Calendar+"/"+ev.Title+" "+when)
		}
		out = append(out, fmt.Sprintf("%s %s: %s", d.Date, d.Weekday, strings.Join(evs, ", ")))
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return "?"
	}
	return *s
}

// checkFixtureWeek checks that the server at base serves the fixture week.
func checkFixtureWeek(t *testing.T, base string) {
	t.Helper()
	cal := getCalendar(t, base+"/calendar?from=2030-03-04&days=7")
	if cal.Timezone != "America/New_York" || cal.From != "2030-03-04" || cal.To != "2030-03-10" {
		t.Errorf("timezone/from/to = %q %q %q", cal.Timezone, cal.From, cal.To)
	}
	if got := render(cal); strings.Join(got, "\n") != strings.Join(fixtureWeek, "\n") {
		t.Errorf("calendar week:\ngot:\n  %s\nwant:\n  %s",
			strings.Join(got, "\n  "), strings.Join(fixtureWeek, "\n  "))
	}
	// The morning run keeps 07:00 local time across the DST change.
	if len(cal.Days) == 7 && len(cal.Days[5].Events) == 2 && len(cal.Days[6].Events) == 2 {
		if s := cal.Days[5].Events[1].Start; s != "2030-03-09T07:00:00-05:00" {
			t.Errorf("Saturday run start = %s, want 2030-03-09T07:00:00-05:00", s)
		}
		if s := cal.Days[6].Events[1].Start; s != "2030-03-10T07:00:00-04:00" {
			t.Errorf("Sunday run start = %s, want 2030-03-10T07:00:00-04:00", s)
		}
	}
}

// writeConfigFile writes content to lcars.toml in a new temp dir and returns
// its path. The file is world-readable so a container's nonroot user can read it.
func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lcars.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func fixturesDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(calendarFixtures)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCalendar(t *testing.T) {
	config := writeConfigFile(t, fmt.Sprintf(
		"timezone = \"America/New_York\"\n\n[calendar]\ndir = %q\n", fixturesDir(t)))
	p := startServer(t, "LCARS_CONFIG="+config)

	checkFixtureWeek(t, p.url(""))

	for _, want := range []map[string]any{
		{"msg": "config loaded", "file": config},
		{"msg": "calendar loaded", "files": float64(4), "skipped": float64(1)},
		{"msg": "calendar: skipping file", "level": "WARN", "path": "broken.ics"},
	} {
		if !p.hasLog(want) {
			t.Errorf("missing log line matching %v\nstdout: %s", want, p.stdout.String())
		}
	}
}

// TestCalendarConfigInWorkingDir checks that ./lcars.toml is read without
// LCARS_CONFIG, and that a relative calendar.dir is resolved against it.
func TestCalendarConfigInWorkingDir(t *testing.T) {
	// The server sees its working directory with symlinks resolved (on macOS,
	// /var is a symlink to /private/var), so compute the relative path from that.
	wd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, fixturesDir(t))
	if err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("timezone = \"America/New_York\"\n\n[calendar]\ndir = %q\n", rel)
	if err := os.WriteFile(filepath.Join(wd, "lcars.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	p := startServerIn(t, wd)
	checkFixtureWeek(t, p.url(""))
	if !p.hasLog(map[string]any{"msg": "config loaded", "file": "lcars.toml"}) {
		t.Errorf("missing config loaded log line\nstdout: %s", p.stdout.String())
	}
}

func TestCalendarEnvOverridesFile(t *testing.T) {
	config := writeConfigFile(t,
		"timezone = \"Asia/Tokyo\"\n\n[calendar]\ndir = \"/nonexistent/calendars\"\n")
	p := startServer(t,
		"LCARS_CONFIG="+config,
		"LCARS_CALENDAR_DIR="+fixturesDir(t),
		"LCARS_TIMEZONE=America/New_York",
	)
	checkFixtureWeek(t, p.url(""))
}

func TestCalendarRefresh(t *testing.T) {
	dir := t.TempDir()
	p := startServer(t,
		"LCARS_CALENDAR_DIR="+dir,
		"LCARS_CALENDAR_REFRESH_INTERVAL=1s",
		"LCARS_TIMEZONE=America/New_York",
	)
	url := p.url("/calendar?from=2030-03-04&days=1")
	titles := func() string {
		var out []string
		for _, ev := range getCalendar(t, url).Days[0].Events {
			out = append(out, ev.Title)
		}
		return strings.Join(out, ",")
	}
	first := getCalendar(t, url)
	if len(first.Days[0].Events) != 0 {
		t.Fatalf("empty directory served events: %+v", first.Days)
	}

	ics := func(title string) []byte {
		return []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//lcars-api//e2e//EN\r\n" +
			"BEGIN:VEVENT\r\nUID:refresh@example.com\r\nSUMMARY:" + title +
			"\r\nDTSTART:20300304T120000\r\nDURATION:PT1H\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	}
	path := filepath.Join(dir, "new.ics")

	if err := os.WriteFile(path, ics("Added"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "new event to appear", func() bool { return titles() == "Added" })

	if err := os.WriteFile(path, ics("Renamed"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "event to be renamed", func() bool { return titles() == "Renamed" })

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "event to disappear", func() bool { return titles() == "" })

	if last := getCalendar(t, url); !last.UpdatedAt.After(first.UpdatedAt) {
		t.Errorf("updated_at did not advance: %v then %v", first.UpdatedAt, last.UpdatedAt)
	}

	// The refresh loop must not hold up shutdown.
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := p.wait(t); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

// TestCalendarToday checks the defaults: from today, for 7 days.
func TestCalendarToday(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().In(ny)
	dir := t.TempDir()
	ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//lcars-api//e2e//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:today@example.com\r\nSUMMARY:Today's event\r\n" +
		"DTSTART;VALUE=DATE:" + today.Format("20060102") + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	if err := os.WriteFile(filepath.Join(dir, "today.ics"), []byte(ics), 0o600); err != nil {
		t.Fatal(err)
	}
	p := startServer(t, "LCARS_CALENDAR_DIR="+dir, "LCARS_TIMEZONE=America/New_York")

	cal := getCalendar(t, p.url("/calendar"))
	if len(cal.Days) != 7 {
		t.Fatalf("got %d days, want 7", len(cal.Days))
	}
	d := cal.Days[0]
	if d.Date != today.Format(time.DateOnly) || !d.Today {
		t.Errorf("first day = %s (today=%v), want %s (today=true)", d.Date, d.Today, today.Format(time.DateOnly))
	}
	if len(d.Events) != 1 || d.Events[0].Title != "Today's event" || !d.Events[0].AllDay {
		t.Errorf("today's events = %+v", d.Events)
	}
	for _, d := range cal.Days[1:] {
		if d.Today || len(d.Events) != 0 {
			t.Errorf("%s: today=%v events=%d, want false and 0", d.Date, d.Today, len(d.Events))
		}
	}

	// Over NATS, an empty request means the same.
	viaNATS := natsRequest(t, connectNATS(t, p.natsURL()), subjectCalendarGet, nil)
	if err := replyError(viaNATS); err != nil {
		t.Fatal(err)
	}
	var pcal lcarsv1.GetCalendarResponse
	if err := proto.Unmarshal(viaNATS.Data, &pcal); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(renderNATS(&pcal), "\n"), strings.Join(render(cal), "\n"); got != want {
		t.Errorf("NATS default calendar:\n%s\nHTTP:\n%s", got, want)
	}
	if !pcal.GetDays()[0].GetToday() {
		t.Error("NATS: first day not marked today")
	}
}

func TestCalendarDisabled(t *testing.T) {
	p := startServer(t)
	resp := mustDo(t, http.MethodGet, p.url("/calendar"))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if !strings.Contains(string(resp.body), "calendar.dir") {
		t.Errorf("body = %s, want a hint about calendar.dir", resp.body)
	}
	if !p.hasLog(map[string]any{"msg": "calendar disabled: calendar.dir is not set"}) {
		t.Errorf("missing calendar disabled log line\nstdout: %s", p.stdout.String())
	}

	msg := natsRequest(t, connectNATS(t, p.natsURL()), subjectCalendarGet, &lcarsv1.GetCalendarRequest{})
	if code := msg.Header.Get(micro.ErrorCodeHeader); code != "503" {
		t.Errorf("NATS %s = %q, want 503", micro.ErrorCodeHeader, code)
	}
	if m := msg.Header.Get(micro.ErrorHeader); !strings.Contains(m, "calendar.dir") {
		t.Errorf("NATS error = %q, want a hint about calendar.dir", m)
	}
}

func TestCalendarBadRequest(t *testing.T) {
	p := startServer(t, "LCARS_CALENDAR_DIR="+fixturesDir(t))
	for _, q := range []string{"?from=next-week", "?from=2030-02-30", "?days=0", "?days=100", "?days=seven"} {
		t.Run(q, func(t *testing.T) {
			resp := mustDo(t, http.MethodGet, p.url("/calendar"+q))
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
			var body map[string]string
			if err := json.Unmarshal(resp.body, &body); err != nil || body["error"] == "" {
				t.Errorf("body = %s, want JSON with an error message", resp.body)
			}
		})
	}
}
