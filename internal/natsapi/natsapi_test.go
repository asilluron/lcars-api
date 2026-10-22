package natsapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
	"google.golang.org/protobuf/proto"

	lcarsv1 "github.com/asilluron/lcars-api/gen/go/lcars/v1"
	"github.com/asilluron/lcars-api/internal/broker"
	"github.com/asilluron/lcars-api/internal/calendar"
	"github.com/asilluron/lcars-api/internal/config"
)

var discard = slog.New(slog.DiscardHandler)

// connect starts an embedded NATS server and returns a connection to it.
func connect(t *testing.T) *nats.Conn {
	t.Helper()
	b, err := broker.Start(config.NATS{Addr: "127.0.0.1:0"}, nil, discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close(t.Context()) })
	return b.Conn
}

// startService serves the API with deps and returns a connection for clients.
func startService(t *testing.T, deps Deps) *nats.Conn {
	t.Helper()
	nc := connect(t)
	svc, err := Start(t.Context(), nc, discard, "v1.2.3", deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop() })
	return nc
}

// newStore returns a calendar of the given VEVENT bodies, in New York.
func newStore(t *testing.T, events ...string) *calendar.Store {
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
	s := calendar.NewStore(dir, loc, discard)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	return s
}

// request sends req on lcars.calendar.get and returns the reply.
func request(t *testing.T, nc *nats.Conn, req proto.Message) *nats.Msg {
	t.Helper()
	var data []byte
	if req != nil {
		var err error
		if data, err = proto.Marshal(req); err != nil {
			t.Fatal(err)
		}
	}
	msg, err := nc.Request(SubjectCalendarGet, data, 5*time.Second)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	return msg
}

func TestCalendarGet(t *testing.T) {
	nc := startService(t, Deps{Calendar: newStore(t,
		"UID:standup\nSUMMARY:Standup\nLOCATION:Video call\nDTSTART:20300305T090000\nDTEND:20300305T091500",
		"UID:bins\nSUMMARY:Bins\nDTSTART;VALUE=DATE:20300305",
		"UID:flight\nSUMMARY:Flight\nDTSTART:20300305T190000\nDTEND:20300307T073000",
	)})

	msg := request(t, nc, &lcarsv1.GetCalendarRequest{From: "2030-03-04", Days: 4})
	if code := msg.Header.Get(micro.ErrorCodeHeader); code != "" {
		t.Fatalf("error reply %s: %s", code, msg.Header.Get(micro.ErrorHeader))
	}
	var got lcarsv1.GetCalendarResponse
	if err := proto.Unmarshal(msg.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.GetTimezone() != "America/New_York" || got.GetFrom() != "2030-03-04" || got.GetTo() != "2030-03-07" {
		t.Errorf("header = %q %q..%q", got.GetTimezone(), got.GetFrom(), got.GetTo())
	}
	if got.GetUpdatedAtUnix() == 0 {
		t.Error("updated_at_unix is 0")
	}

	var lines []string
	for _, d := range got.GetDays() {
		parts := []string{d.GetDate() + " " + d.GetWeekday() + ":"}
		for _, ev := range d.GetEvents() {
			when := "(all-day)"
			if !ev.GetAllDay() {
				when = "(" + ev.GetStartTime() + "-" + ev.GetEndTime() + ")"
			}
			parts = append(parts, ev.GetTitle()+when)
		}
		lines = append(lines, strings.Join(parts, " "))
	}
	want := []string{
		"2030-03-04 Monday:",
		"2030-03-05 Tuesday: Bins(all-day) Standup(09:00-09:15) Flight(19:00-)",
		"2030-03-06 Wednesday: Flight(-)",
		"2030-03-07 Thursday: Flight(-07:30)",
	}
	if !slices.Equal(lines, want) {
		t.Errorf("days:\n got %q\nwant %q", lines, want)
	}

	standup := got.GetDays()[1].GetEvents()[1]
	wantStart := time.Date(2030, 3, 5, 14, 0, 0, 0, time.UTC).Unix()
	if standup.GetId() != "standup/20300305T140000Z" || standup.GetCalendar() != "home" ||
		standup.GetLocation() != "Video call" || standup.GetStartUnix() != wantStart ||
		standup.GetEndUnix() != wantStart+15*60 {
		t.Errorf("standup = %v", standup)
	}
	// Unset, not "", on days the event doesn't start or end.
	if flight := got.GetDays()[2].GetEvents()[0]; flight.StartTime != nil || flight.EndTime != nil {
		t.Errorf("middle day of flight: start_time=%v end_time=%v, want unset", flight.StartTime, flight.EndTime)
	}
}

// TestCalendarGetDefaults checks that an empty body asks for 7 days from today.
func TestCalendarGetDefaults(t *testing.T) {
	nc := startService(t, Deps{Calendar: newStore(t)})
	msg := request(t, nc, nil)
	var got lcarsv1.GetCalendarResponse
	if err := proto.Unmarshal(msg.Data, &got); err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("America/New_York")
	today := time.Now().In(loc).Format(time.DateOnly)
	if len(got.GetDays()) != calendar.DefaultDays || got.GetFrom() != today || !got.GetDays()[0].GetToday() {
		t.Errorf("got %d days from %s (today=%v), want %d from %s", len(got.GetDays()), got.GetFrom(),
			got.GetDays()[0].GetToday(), calendar.DefaultDays, today)
	}
}

func TestCalendarGetErrors(t *testing.T) {
	nc := startService(t, Deps{Calendar: newStore(t)})
	tests := map[string]struct {
		data     []byte
		wantCode string
		wantMsg  string
	}{
		"bad from":     {mustMarshal(t, &lcarsv1.GetCalendarRequest{From: "tomorrow"}), "400", `invalid "from"`},
		"impossible":   {mustMarshal(t, &lcarsv1.GetCalendarRequest{From: "2030-02-30"}), "400", `invalid "from"`},
		"too many":     {mustMarshal(t, &lcarsv1.GetCalendarRequest{Days: 63}), "400", `invalid "days"`},
		"huge":         {mustMarshal(t, &lcarsv1.GetCalendarRequest{Days: 1 << 31}), "400", `invalid "days"`},
		"not protobuf": {[]byte("{\"days\": 7}"), "400", "lcars.v1.GetCalendarRequest"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			msg, err := nc.Request(SubjectCalendarGet, tt.data, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if code := msg.Header.Get(micro.ErrorCodeHeader); code != tt.wantCode {
				t.Errorf("code = %q, want %q", code, tt.wantCode)
			}
			if m := msg.Header.Get(micro.ErrorHeader); !strings.Contains(m, tt.wantMsg) {
				t.Errorf("message = %q, want it to contain %q", m, tt.wantMsg)
			}
			if len(msg.Data) != 0 {
				t.Errorf("error reply has a body: %q", msg.Data)
			}
		})
	}
	if msg := request(t, nc, &lcarsv1.GetCalendarRequest{Days: calendar.MaxDays}); msg.Header.Get(micro.ErrorCodeHeader) != "" {
		t.Errorf("days=%d: error %s", calendar.MaxDays, msg.Header.Get(micro.ErrorHeader))
	}
}

func TestCalendarNotConfigured(t *testing.T) {
	nc := startService(t, Deps{})
	msg := request(t, nc, &lcarsv1.GetCalendarRequest{})
	if code := msg.Header.Get(micro.ErrorCodeHeader); code != "503" {
		t.Errorf("code = %q, want 503", code)
	}
	if m := msg.Header.Get(micro.ErrorHeader); !strings.Contains(m, "calendar.dir") {
		t.Errorf("message = %q, want a hint about calendar.dir", m)
	}
}

func TestHandleErrors(t *testing.T) {
	nc := connect(t)
	svc, err := micro.AddService(nc, micro.Config{Name: "test", Version: "0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop() })
	handlers := map[string]handlerFunc{
		"api":   func(micro.Request) (proto.Message, error) { return nil, &Error{Code: 404, Msg: "no such thing"} },
		"plain": func(micro.Request) (proto.Message, error) { return nil, errors.New("disk on fire") },
		"panic": func(micro.Request) (proto.Message, error) { panic("boom") },
	}
	for name, h := range handlers {
		if err := svc.AddEndpoint(name, handle(discard, h)); err != nil {
			t.Fatal(err)
		}
	}
	for subject, want := range map[string][2]string{
		"api":   {"404", "no such thing"},
		"plain": {"500", "Internal Server Error"}, // details stay in the log
		"panic": {"500", "Internal Server Error"},
	} {
		msg, err := nc.Request(subject, nil, 5*time.Second)
		if err != nil {
			t.Fatalf("%s: %v", subject, err)
		}
		got := [2]string{msg.Header.Get(micro.ErrorCodeHeader), msg.Header.Get(micro.ErrorHeader)}
		if got != want {
			t.Errorf("%s: got %q, want %q", subject, got, want)
		}
	}
}

func TestServiceInfo(t *testing.T) {
	nc := startService(t, Deps{})
	msg, err := nc.Request("$SRV.INFO."+ServiceName, nil, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var info micro.Info
	if err := json.Unmarshal(msg.Data, &info); err != nil {
		t.Fatal(err)
	}
	if info.Name != ServiceName || info.Version != "1.2.3" || info.Metadata["build"] != "v1.2.3" {
		t.Errorf("info = %+v", info)
	}
	var subjects []string
	for _, ep := range info.Endpoints {
		subjects = append(subjects, ep.Subject)
	}
	for _, s := range subjects {
		if !slices.Contains(Subjects, s) {
			t.Errorf("endpoint %s is not listed in Subjects", s)
		}
	}
	if !slices.Contains(subjects, SubjectCalendarGet) {
		t.Errorf("endpoints %v missing %s", subjects, SubjectCalendarGet)
	}
}

func TestServiceVersion(t *testing.T) {
	for in, want := range map[string]string{
		"v1.2.3":               "1.2.3",
		"v0.0.0-e2e":           "0.0.0-e2e",
		"v1.2.3-4-gabcdef1":    "1.2.3-4-gabcdef1",
		"v1.2.3-dirty":         "1.2.3-dirty",
		"(devel)":              "0.0.0-dev",
		"985ab95":              "0.0.0-dev",
		"985ab95-dirty":        "0.0.0-dev",
		"":                     "0.0.0-dev",
		"v1.2.3+build.5":       "1.2.3+build.5",
		"v01.2.3":              "0.0.0-dev",
		"v1.2.3-4-gabcdef1-x!": "0.0.0-dev",
	} {
		if got := serviceVersion(in); got != want {
			t.Errorf("serviceVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCalendarChanged(t *testing.T) {
	nc := connect(t)
	sub, err := nc.SubscribeSync(SubjectCalendarUpdated)
	if err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	loadedAt := time.Date(2030, 3, 4, 12, 0, 0, 0, time.UTC)
	CalendarChanged(nc, discard)(loadedAt)

	msg, err := sub.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var ev lcarsv1.CalendarUpdated
	if err := proto.Unmarshal(msg.Data, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.GetUpdatedAtUnix() != loadedAt.Unix() {
		t.Errorf("updated_at_unix = %d, want %d", ev.GetUpdatedAtUnix(), loadedAt.Unix())
	}
}

// TestRustSubjects checks that the Rust crate defines exactly the subjects the
// server uses, so clients can't drift from the server.
func TestRustSubjects(t *testing.T) {
	src, err := os.ReadFile("../../rust/src/lib.rs")
	if err != nil {
		t.Fatal(err)
	}
	mod := regexp.MustCompile(`(?s)pub mod subjects \{(.*?)\n\}`).FindSubmatch(src)
	if mod == nil {
		t.Fatal("rust/src/lib.rs has no subjects module")
	}
	var rust []string
	for _, m := range regexp.MustCompile(`pub const \w+: &str = "([^"]+)";`).FindAllSubmatch(mod[1], -1) {
		rust = append(rust, string(m[1]))
	}
	slices.Sort(rust)
	goSubjects := slices.Sorted(slices.Values(Subjects))
	if !slices.Equal(rust, goSubjects) {
		t.Errorf("Rust subjects = %q, Go subjects = %q", rust, goSubjects)
	}
	if !strings.Contains(string(src), `pub const SERVICE_NAME: &str = "`+ServiceName+`";`) {
		t.Errorf("rust/src/lib.rs SERVICE_NAME does not match %q", ServiceName)
	}
	for _, h := range []string{micro.ErrorCodeHeader, micro.ErrorHeader} {
		if !strings.Contains(string(src), `"`+h+`"`) {
			t.Errorf("rust/src/lib.rs does not define header %q", h)
		}
	}
}

// TestSubjectsDocumented checks that the README documents every subject.
func TestSubjectsDocumented(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range Subjects {
		if !strings.Contains(string(readme), "`"+s+"`") {
			t.Errorf("README.md does not document %s", s)
		}
	}
}

func mustMarshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	data, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
