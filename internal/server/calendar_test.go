package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/asilluron/lcars-api/internal/calendar"
)

// newCalendarServer serves a calendar made of the given VEVENT bodies,
// in America/New_York.
func newCalendarServer(t *testing.T, events ...string) *httptest.Server {
	t.Helper()
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//lcars-api//test//EN\r\n")
	for _, ev := range events {
		b.WriteString("BEGIN:VEVENT\r\n" + strings.ReplaceAll(ev, "\n", "\r\n") + "\r\nEND:VEVENT\r\n")
	}
	b.WriteString("END:VCALENDAR\r\n")

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "home.ics"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	store := calendar.NewStore(dir, loc, logger)
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(logger, Deps{Calendar: store}))
	t.Cleanup(srv.Close)
	return srv
}

func getCalendar(t *testing.T, srv *httptest.Server, query string) (int, calendarResponse, map[string]string) {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + "/calendar" + query)
	if err != nil {
		t.Fatalf("GET /calendar%s: %v", query, err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body calendarResponse
	var errBody map[string]string
	if resp.StatusCode == http.StatusOK {
		err = json.NewDecoder(resp.Body).Decode(&body)
	} else {
		err = json.NewDecoder(resp.Body).Decode(&errBody)
	}
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.StatusCode, body, errBody
}

func TestCalendarNotConfigured(t *testing.T) {
	srv := newTestServer(t)
	code, _, body := getCalendar(t, srv, "")
	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", code)
	}
	if !strings.Contains(body["error"], "calendar.dir") {
		t.Errorf("error = %q, want a hint about calendar.dir", body["error"])
	}
}

func TestCalendarDays(t *testing.T) {
	srv := newCalendarServer(t,
		"UID:standup\nSUMMARY:Standup\nLOCATION:Video call\nDTSTART:20300305T090000\nDTEND:20300305T091500",
		"UID:bins\nSUMMARY:Bins\nDTSTART;VALUE=DATE:20300305",
		"UID:flight\nSUMMARY:Flight\nDTSTART:20300305T190000\nDTEND:20300307T073000",
		"UID:late\nSUMMARY:Late show\nDTSTART:20300304T230000\nDTEND:20300305T000000",
	)

	code, got, _ := getCalendar(t, srv, "?from=2030-03-04&days=4")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got.Timezone != "America/New_York" || got.From != "2030-03-04" || got.To != "2030-03-07" {
		t.Errorf("header = %q %q..%q, want America/New_York 2030-03-04..2030-03-07", got.Timezone, got.From, got.To)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("updated_at is zero")
	}

	type row struct{ title, startTime, endTime string }
	str := func(p *string) string {
		if p == nil {
			return "-"
		}
		return *p
	}
	want := map[string][]row{
		"2030-03-04": {{"Late show", "23:00", "24:00"}},
		"2030-03-05": {{"Bins", "-", "-"}, {"Standup", "09:00", "09:15"}, {"Flight", "19:00", "-"}},
		"2030-03-06": {{"Flight", "-", "-"}},
		"2030-03-07": {{"Flight", "-", "07:30"}},
	}
	wantWeekdays := []string{"Monday", "Tuesday", "Wednesday", "Thursday"}
	if len(got.Days) != 4 {
		t.Fatalf("got %d days, want 4", len(got.Days))
	}
	for i, d := range got.Days {
		if d.Weekday != wantWeekdays[i] {
			t.Errorf("%s weekday = %q, want %q", d.Date, d.Weekday, wantWeekdays[i])
		}
		if d.Today {
			t.Errorf("%s marked today", d.Date)
		}
		var rows []row
		for _, ev := range d.Events {
			rows = append(rows, row{ev.Title, str(ev.StartTime), str(ev.EndTime)})
		}
		if !slices.Equal(rows, want[d.Date]) {
			t.Errorf("%s events = %v, want %v", d.Date, rows, want[d.Date])
		}
	}

	standup := got.Days[1].Events[1]
	if standup.Calendar != "home" || standup.Location != "Video call" || standup.AllDay ||
		standup.ID != "standup/20300305T140000Z" {
		t.Errorf("standup = %+v", standup)
	}
	if got.Days[1].Events[0].Start.Format(time.RFC3339) != "2030-03-05T00:00:00-05:00" {
		t.Errorf("all-day start = %s, want local midnight", got.Days[1].Events[0].Start.Format(time.RFC3339))
	}
}

func TestCalendarDefaults(t *testing.T) {
	srv := newCalendarServer(t)
	code, got, _ := getCalendar(t, srv, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	today := time.Now().In(mustLoadLocation(t, "America/New_York")).Format(time.DateOnly)
	if len(got.Days) != defaultCalendarDays {
		t.Fatalf("got %d days, want %d", len(got.Days), defaultCalendarDays)
	}
	if got.From != today || got.Days[0].Date != today || !got.Days[0].Today {
		t.Errorf("first day = %+v, want today (%s)", got.Days[0], today)
	}
	for _, d := range got.Days[1:] {
		if d.Today {
			t.Errorf("%s marked today", d.Date)
		}
		if d.Events == nil {
			t.Errorf("%s events is null, want []", d.Date)
		}
	}
}

// TestCalendarJSONShape checks the wire format: empty days have [] and
// all-day events have null times, so clients need no special cases.
func TestCalendarJSONShape(t *testing.T) {
	srv := newCalendarServer(t, "UID:a\nSUMMARY:Holiday\nDTSTART;VALUE=DATE:20300305")
	resp, err := srv.Client().Get(srv.URL + "/calendar?from=2030-03-04&days=2")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw struct {
		Days []struct {
			Events []map[string]any `json:"events"`
		} `json:"days"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if raw.Days[0].Events == nil {
		t.Error("empty day: events is null, want []")
	}
	ev := raw.Days[1].Events[0]
	for _, key := range []string{"id", "title", "calendar", "location", "all_day", "start", "end", "start_time", "end_time"} {
		if _, ok := ev[key]; !ok {
			t.Errorf("event has no %q key", key)
		}
	}
	if ev["start_time"] != nil || ev["end_time"] != nil {
		t.Errorf("all-day times = %v, %v; want null", ev["start_time"], ev["end_time"])
	}
}

func TestCalendarBadRequest(t *testing.T) {
	srv := newCalendarServer(t)
	for _, q := range []string{
		"?from=tomorrow", "?from=2030-13-01", "?from=2030-03-04T00:00:00Z",
		"?days=0", "?days=63", "?days=-1", "?days=week", "?days=1.5",
	} {
		t.Run(q, func(t *testing.T) {
			code, _, body := getCalendar(t, srv, q)
			if code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", code)
			}
			if body["error"] == "" {
				t.Error("no error message")
			}
		})
	}
	if code, got, _ := getCalendar(t, srv, "?days=62"); code != http.StatusOK || len(got.Days) != 62 {
		t.Errorf("days=62: status %d with %d days, want 200 with 62", code, len(got.Days))
	}
}

func mustLoadLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}
