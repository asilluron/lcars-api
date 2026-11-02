package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asilluron/lcars-api/internal/calendar"
	"github.com/asilluron/lcars-api/internal/devices"
	"github.com/asilluron/lcars-api/internal/supabase"
)

// fakeLabelDB is an in-memory CalendarLabelDB.
type fakeLabelDB struct {
	mu   sync.Mutex
	rows []supabase.CalendarLabel
}

func (f *fakeLabelDB) ListCalendarLabels(context.Context) ([]supabase.CalendarLabel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.rows), nil
}

func (f *fakeLabelDB) UpsertCalendarLabel(_ context.Context, row supabase.CalendarLabel) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rows {
		if f.rows[i].ID == row.ID {
			f.rows[i] = row
			return nil
		}
	}
	f.rows = append(f.rows, row)
	return nil
}

func (f *fakeLabelDB) DeleteCalendarLabel(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.rows)
	f.rows = slices.DeleteFunc(f.rows, func(row supabase.CalendarLabel) bool { return row.ID == id })
	if len(f.rows) == n {
		return supabase.ErrNotFound
	}
	return nil
}

func TestCalendarAdmin(t *testing.T) {
	dir := t.TempDir()
	eid := base64.StdEncoding.EncodeToString([]byte("evt ada@example.com"))
	body := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nX-WR-CALNAME:Ada Google Calendar\r\n" +
		"BEGIN:VEVENT\r\nUID:evt@google.com\r\nSUMMARY:Invoices\r\n" +
		"DTSTART:20300305T150000\r\nDTEND:20300305T160000\r\n" +
		"URL:https://www.google.com/calendar/event?eid=" + eid + "\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	if err := os.WriteFile(filepath.Join(dir, "ada.ics"), []byte(body), 0o600); err != nil {
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
	labels := &fakeLabelDB{}
	var notifies atomic.Int32
	dev := devices.NewStore(&fakeTokenDB{}, logger, nil)
	srv := httptest.NewServer(New(logger, Deps{
		Calendar:        store,
		Devices:         dev,
		Admins:          fakeAdmins{},
		Labels:          labels,
		CalendarChanged: func(time.Time) { notifies.Add(1) },
	}))
	t.Cleanup(srv.Close)

	if resp := call(t, srv, http.MethodGet, "/admin/api/calendars", "", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET without a session = %d, want 401", resp.StatusCode)
	}

	var list struct {
		Calendars []calendarSource `json:"calendars"`
	}
	if resp := call(t, srv, http.MethodGet, "/admin/api/calendars", testToken, "", &list); resp.StatusCode != http.StatusOK ||
		len(list.Calendars) != 1 || list.Calendars[0].ID != "mail:ada@example.com" || list.Calendars[0].Source != "ada" {
		t.Fatalf("list = %d %+v", resp.StatusCode, list.Calendars)
	}

	var saved calendarSource
	resp := call(t, srv, http.MethodPut, "/admin/api/calendars", testToken,
		`{"id":"mail:ada@example.com","name":" Family ","category":"Personal"}`, &saved)
	if resp.StatusCode != http.StatusOK || saved.Name != "Family" || saved.Category != "personal" {
		t.Fatalf("save = %d %+v", resp.StatusCode, saved)
	}
	if notifies.Load() != 1 {
		t.Errorf("calendar updates = %d, want 1", notifies.Load())
	}
	token, secret, err := dev.Create(t.Context(), "Display", "admin-1")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/calendar?from=2030-03-05&days=1", nil)
	req.SetBasicAuth(token.ID, secret)
	httpResp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer httpResp.Body.Close()
	var agenda calendarResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&agenda); err != nil {
		t.Fatal(err)
	}
	if httpResp.StatusCode != http.StatusOK || len(agenda.Days) != 1 || len(agenda.Days[0].Events) != 1 ||
		agenda.Days[0].Events[0].Calendar != "Family" || agenda.Days[0].Events[0].Category != "personal" {
		t.Fatalf("calendar = %d %+v", httpResp.StatusCode, agenda.Days)
	}

	if resp := call(t, srv, http.MethodPut, "/admin/api/calendars", testToken,
		`{"id":"mail:ada@example.com","name":"Family","category":"personal"}`, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("save again = %d", resp.StatusCode)
	}
	if notifies.Load() != 1 {
		t.Errorf("unchanged save published an update (%d)", notifies.Load())
	}
	if resp := call(t, srv, http.MethodPut, "/admin/api/calendars", testToken,
		`{"id":"mail:ada@example.com","name":"","category":""}`, nil); resp.StatusCode != http.StatusOK || notifies.Load() != 2 {
		t.Fatalf("clear = %d, updates %d; want 200 and 2", resp.StatusCode, notifies.Load())
	}
	if resp := call(t, srv, http.MethodPut, "/admin/api/calendars", testToken,
		`{"id":"mail:ada@example.com","category":"school"}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad category = %d, want 400", resp.StatusCode)
	}
	if resp := call(t, srv, http.MethodPut, "/admin/api/calendars", testToken,
		`{"id":"file:nope.ics","name":"X"}`, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown calendar = %d, want 404", resp.StatusCode)
	}

	// Signed in, but no calendar directory.
	plain := httptest.NewServer(New(logger, Deps{Devices: dev, Admins: fakeAdmins{}, Labels: labels}))
	t.Cleanup(plain.Close)
	if resp := call(t, plain, http.MethodGet, "/admin/api/calendars", testToken, "", nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET without a calendar = %d, want 503", resp.StatusCode)
	}
}
