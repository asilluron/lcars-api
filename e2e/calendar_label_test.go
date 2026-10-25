//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// clearFixtureCalendarLabel deletes the admin label for the fixture Google
// calendar, so a name saved by a test doesn't change the fixture week for
// later servers. The row is removed before a server starts, because a server
// reads labels at startup.
func clearFixtureCalendarLabel(t *testing.T) {
	t.Helper()
	s := requireSupabase(t)
	s.call(t, http.MethodDelete, "/rest/v1/calendar_labels?id=eq."+url.QueryEscape(fixtureGoogleID), nil)
}

func TestCalendarLabels(t *testing.T) {
	p, admin := authServer(t)
	t.Cleanup(func() { clearFixtureCalendarLabel(t) })
	base := p.url("")
	tok := createToken(t, base, admin, "Label display")

	list := adminCall(t, base, http.MethodGet, "/admin/api/calendars", admin, nil)
	if list.StatusCode != http.StatusOK {
		t.Fatalf("list calendars = %d %s", list.StatusCode, list.body)
	}
	var listed struct {
		Calendars []struct {
			ID, Source, Title, Path, Name, Category string
		} `json:"calendars"`
	}
	if err := json.Unmarshal(list.body, &listed); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, cal := range listed.Calendars {
		if cal.ID != fixtureGoogleID {
			continue
		}
		found = true
		if cal.Source != "ada" || cal.Title != "Ada Google Calendar" || cal.Path != "google.ics" ||
			cal.Name != "" || cal.Category != "" {
			t.Errorf("google calendar = %+v", cal)
		}
	}
	if !found {
		t.Fatalf("calendars = %+v, want %s", listed.Calendars, fixtureGoogleID)
	}

	for _, bad := range []any{
		map[string]string{"id": fixtureGoogleID, "name": "Family", "category": "school"},
		map[string]string{"id": fixtureGoogleID, "name": strings.Repeat("x", 41)},
		map[string]string{"name": "Family"},
		map[string]string{"id": "file:missing.ics", "name": "Family"},
	} {
		want := http.StatusBadRequest
		if bad.(map[string]string)["id"] == "file:missing.ics" {
			want = http.StatusNotFound
		}
		if resp := adminCall(t, base, http.MethodPut, "/admin/api/calendars", admin, bad); resp.StatusCode != want {
			t.Errorf("save %v = %d %s, want %d", bad, resp.StatusCode, resp.body, want)
		}
	}

	saved := adminCall(t, base, http.MethodPut, "/admin/api/calendars", admin, map[string]string{
		"id": fixtureGoogleID, "name": "Family", "category": "personal",
	})
	if saved.StatusCode != http.StatusOK || !strings.Contains(string(saved.body), `"name":"Family"`) ||
		!strings.Contains(string(saved.body), `"category":"personal"`) {
		t.Fatalf("save = %d %s", saved.StatusCode, saved.body)
	}

	checkLabeled(t, basicURL(base, tok), "Family", "personal")
	nc, err := nats.Connect(natsURLWith(p.natsURL(), tok), nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	via := mustRequestCalendar(t, nc, "2030-03-05", 1)
	var natsOK bool
	for _, day := range via.GetDays() {
		for _, ev := range day.GetEvents() {
			if ev.GetTitle() != "Pay invoices" {
				continue
			}
			natsOK = ev.GetCalendar() == "Family" && ev.GetCategory() == "personal"
		}
	}
	if !natsOK {
		t.Errorf("NATS calendar = %v, want Family/personal on Pay invoices", via)
	}

	cleared := adminCall(t, base, http.MethodPut, "/admin/api/calendars", admin, map[string]string{
		"id": fixtureGoogleID, "name": "", "category": "",
	})
	if cleared.StatusCode != http.StatusOK {
		t.Fatalf("clear = %d %s", cleared.StatusCode, cleared.body)
	}
	checkLabeled(t, basicURL(base, tok), "ada", "")
}

func checkLabeled(t *testing.T, base, name, category string) {
	t.Helper()
	cal := getCalendar(t, base+"/calendar?from=2030-03-05&days=1")
	for _, day := range cal.Days {
		for _, ev := range day.Events {
			if ev.Title != "Pay invoices" {
				continue
			}
			if ev.Calendar != name || ev.Category != category {
				t.Errorf("Pay invoices = calendar %q category %q, want %q %q", ev.Calendar, ev.Category, name, category)
			}
			return
		}
	}
	t.Error("Pay invoices was not in the calendar")
}
