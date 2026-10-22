//go:build e2e

package e2e

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
	"google.golang.org/protobuf/proto"

	lcarsv1 "github.com/asilluron/lcars-api/gen/go/lcars/v1"
)

// The subjects, spelled out rather than imported, so a change to them on the
// server is caught here as a breaking change for clients.
const (
	subjectCalendarGet      = "lcars.calendar.get"
	subjectCalendarUpdated  = "lcars.calendar.updated"
	subjectRemindersList    = "lcars.reminders.list"
	subjectRemindersSet     = "lcars.reminders.set"
	subjectRemindersUpdated = "lcars.reminders.updated"
)

func connectNATS(t *testing.T, url string) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(url, nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatalf("connect to %s: %v", url, err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// natsRequest sends req (nil for an empty body) and returns the reply.
func natsRequest(t *testing.T, nc *nats.Conn, subject string, req proto.Message) *nats.Msg {
	t.Helper()
	msg, err := rawRequest(nc, subject, req)
	if err != nil {
		t.Fatalf("request %s: %v", subject, err)
	}
	return msg
}

func rawRequest(nc *nats.Conn, subject string, req proto.Message) (*nats.Msg, error) {
	var data []byte
	if req != nil {
		var err error
		if data, err = proto.Marshal(req); err != nil {
			return nil, err
		}
	}
	cover(subjectCoverage, subject)
	return nc.Request(subject, data, 5*time.Second)
}

// replyError returns the error in an error reply, or nil.
func replyError(msg *nats.Msg) error {
	if code := msg.Header.Get(micro.ErrorCodeHeader); code != "" {
		return fmt.Errorf("error reply %s: %s", code, msg.Header.Get(micro.ErrorHeader))
	}
	return nil
}

// requestCalendar asks for the calendar over NATS. It is safe to call from
// several goroutines.
func requestCalendar(nc *nats.Conn, from string, days uint32) (*lcarsv1.GetCalendarResponse, error) {
	msg, err := rawRequest(nc, subjectCalendarGet, &lcarsv1.GetCalendarRequest{From: from, Days: days})
	if err != nil {
		return nil, err
	}
	if err := replyError(msg); err != nil {
		return nil, err
	}
	var cal lcarsv1.GetCalendarResponse
	if err := proto.Unmarshal(msg.Data, &cal); err != nil {
		return nil, fmt.Errorf("decode reply: %w", err)
	}
	return &cal, nil
}

func mustRequestCalendar(t *testing.T, nc *nats.Conn, from string, days uint32) *lcarsv1.GetCalendarResponse {
	t.Helper()
	cal, err := requestCalendar(nc, from, days)
	if err != nil {
		t.Fatal(err)
	}
	return cal
}

// renderNATS formats a NATS calendar reply like fixtureWeek.
func renderNATS(cal *lcarsv1.GetCalendarResponse) []string {
	var out []string
	for _, d := range cal.GetDays() {
		var evs []string
		for _, ev := range d.GetEvents() {
			when := "all-day"
			if !ev.GetAllDay() {
				when = deref(ev.StartTime) + "-" + deref(ev.EndTime)
			}
			evs = append(evs, ev.GetCalendar()+"/"+ev.GetTitle()+" "+when)
		}
		out = append(out, fmt.Sprintf("%s %s: %s", d.GetDate(), d.GetWeekday(), strings.Join(evs, ", ")))
	}
	return out
}

// checkFixtureWeekNATS checks that the NATS server at url serves the fixture
// week.
func checkFixtureWeekNATS(t *testing.T, url string) {
	t.Helper()
	cal := mustRequestCalendar(t, connectNATS(t, url), "2030-03-04", 7)
	if cal.GetTimezone() != "America/New_York" || cal.GetFrom() != "2030-03-04" || cal.GetTo() != "2030-03-10" {
		t.Errorf("timezone/from/to = %q %q %q", cal.GetTimezone(), cal.GetFrom(), cal.GetTo())
	}
	if got := renderNATS(cal); strings.Join(got, "\n") != strings.Join(fixtureWeek, "\n") {
		t.Errorf("NATS calendar week:\ngot:\n  %s\nwant:\n  %s",
			strings.Join(got, "\n  "), strings.Join(fixtureWeek, "\n  "))
	}
	// The morning run keeps 07:00 local time across the DST change.
	days := cal.GetDays()
	if len(days) == 7 && len(days[5].GetEvents()) == 2 && len(days[6].GetEvents()) == 2 {
		ny, err := time.LoadLocation("America/New_York")
		if err != nil {
			t.Fatal(err)
		}
		for i, want := range map[int]string{5: "2030-03-09T07:00:00-05:00", 6: "2030-03-10T07:00:00-04:00"} {
			if got := time.Unix(days[i].GetEvents()[1].GetStartUnix(), 0).In(ny).Format(time.RFC3339); got != want {
				t.Errorf("%s run start = %s, want %s", days[i].GetDate(), got, want)
			}
		}
	}
}

func TestNATSCalendar(t *testing.T) {
	config := writeConfigFile(t, fmt.Sprintf(
		"timezone = \"America/New_York\"\n\n[calendar]\ndir = %q\n", fixturesDir(t)))
	p := startServer(t, "LCARS_CONFIG="+config)
	checkFixtureWeekNATS(t, p.natsURL())

	// NATS and HTTP serve the same calendar, field for field.
	nc := connectNATS(t, p.natsURL())
	for _, q := range []struct {
		from string
		days uint32
	}{{"2030-03-04", 7}, {"2030-03-08", 1}, {"2030-02-25", 62}} {
		viaNATS := mustRequestCalendar(t, nc, q.from, q.days)
		viaHTTP := getCalendar(t, p.url(fmt.Sprintf("/calendar?from=%s&days=%d", q.from, q.days)))
		if a, b := strings.Join(renderNATS(viaNATS), "\n"), strings.Join(render(viaHTTP), "\n"); a != b {
			t.Errorf("from=%s days=%d: NATS and HTTP differ:\nNATS:\n%s\nHTTP:\n%s", q.from, q.days, a, b)
			continue
		}
		if viaNATS.GetUpdatedAtUnix() != viaHTTP.UpdatedAt.Unix() {
			t.Errorf("updated_at: NATS %d, HTTP %v", viaNATS.GetUpdatedAtUnix(), viaHTTP.UpdatedAt)
		}
		for i, d := range viaHTTP.Days {
			for j, ev := range d.Events {
				pev := viaNATS.GetDays()[i].GetEvents()[j]
				start, _ := time.Parse(time.RFC3339, ev.Start)
				end, _ := time.Parse(time.RFC3339, ev.End)
				if pev.GetId() != ev.ID || pev.GetLocation() != ev.Location ||
					pev.GetStartUnix() != start.Unix() || pev.GetEndUnix() != end.Unix() {
					t.Errorf("%s event %d: NATS %v, HTTP %+v", d.Date, j, pev, ev)
				}
			}
		}
	}
}

// TestNATSCalendarErrors checks error replies: an empty body with the
// standard NATS service error headers.
func TestNATSCalendarErrors(t *testing.T) {
	p := startServer(t, "LCARS_CALENDAR_DIR="+fixturesDir(t))
	nc := connectNATS(t, p.natsURL())

	for name, body := range map[string][]byte{
		"bad from":     mustMarshal(t, &lcarsv1.GetCalendarRequest{From: "next-week"}),
		"bad date":     mustMarshal(t, &lcarsv1.GetCalendarRequest{From: "2030-02-30"}),
		"too many":     mustMarshal(t, &lcarsv1.GetCalendarRequest{Days: 100}),
		"not protobuf": []byte(`{"from":"2030-03-04"}`),
	} {
		t.Run(name, func(t *testing.T) {
			cover(subjectCoverage, subjectCalendarGet)
			msg, err := nc.Request(subjectCalendarGet, body, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if code := msg.Header.Get(micro.ErrorCodeHeader); code != "400" {
				t.Errorf("%s = %q, want 400", micro.ErrorCodeHeader, code)
			}
			if msg.Header.Get(micro.ErrorHeader) == "" {
				t.Errorf("no %s header", micro.ErrorHeader)
			}
			if len(msg.Data) != 0 {
				t.Errorf("error reply has a body: %q", msg.Data)
			}
		})
	}
}

// TestNATSCalendarUpdated checks that a change to the .ics files is
// published, and that reloads that change nothing are not.
func TestNATSCalendarUpdated(t *testing.T) {
	dir := t.TempDir()
	p := startServer(t,
		"LCARS_CALENDAR_DIR="+dir,
		"LCARS_CALENDAR_REFRESH_INTERVAL=1s",
		"LCARS_TIMEZONE=America/New_York",
	)
	nc := connectNATS(t, p.natsURL())
	sub, err := nc.SubscribeSync(subjectCalendarUpdated)
	if err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	nextUpdate := func(timeout time.Duration) (*lcarsv1.CalendarUpdated, error) {
		msg, err := sub.NextMsg(timeout)
		if err != nil {
			return nil, err
		}
		cover(subjectCoverage, subjectCalendarUpdated)
		var ev lcarsv1.CalendarUpdated
		return &ev, proto.Unmarshal(msg.Data, &ev)
	}

	// Nothing changes: no events.
	if ev, err := nextUpdate(2500 * time.Millisecond); !errors.Is(err, nats.ErrTimeout) {
		t.Fatalf("got update %v (err %v) with no changes", ev, err)
	}

	writeICS(t, filepath.Join(dir, "new.ics"), "Added")
	before := time.Now().Add(-time.Second)
	ev, err := nextUpdate(5 * time.Second)
	if err != nil {
		t.Fatalf("no update after adding a file: %v", err)
	}
	if got := time.Unix(ev.GetUpdatedAtUnix(), 0); got.Before(before.Truncate(time.Second)) || got.After(time.Now()) {
		t.Errorf("updated_at_unix = %v, want about now", got)
	}
	cal := mustRequestCalendar(t, nc, "2030-03-04", 1)
	if evs := cal.GetDays()[0].GetEvents(); len(evs) != 1 || evs[0].GetTitle() != "Added" {
		t.Errorf("after update, events = %v", evs)
	}
	if cal.GetUpdatedAtUnix() != ev.GetUpdatedAtUnix() {
		t.Errorf("calendar updated_at_unix = %d, event says %d", cal.GetUpdatedAtUnix(), ev.GetUpdatedAtUnix())
	}

	// Several reloads with no change: still no further events.
	if ev, err := nextUpdate(2500 * time.Millisecond); !errors.Is(err, nats.ErrTimeout) {
		t.Fatalf("got update %v (err %v) with no further changes", ev, err)
	}

	if err := os.Remove(filepath.Join(dir, "new.ics")); err != nil {
		t.Fatal(err)
	}
	if _, err := nextUpdate(5 * time.Second); err != nil {
		t.Fatalf("no update after removing a file: %v", err)
	}
	if evs := mustRequestCalendar(t, nc, "2030-03-04", 1).GetDays()[0].GetEvents(); len(evs) != 0 {
		t.Errorf("after removal, events = %v", evs)
	}
	if !p.hasLog(map[string]any{"msg": "calendar update published", "subject": subjectCalendarUpdated}) {
		t.Errorf("missing publish log line\nstdout: %s", p.stdout.String())
	}
}

// TestNATSService checks the standard NATS service discovery requests.
func TestNATSService(t *testing.T) {
	p := startServer(t)
	nc := connectNATS(t, p.natsURL())

	msg, err := nc.Request("$SRV.PING.lcars", nil, 5*time.Second)
	if err != nil {
		t.Fatalf("$SRV.PING.lcars: %v", err)
	}
	var ping micro.Ping
	if err := json.Unmarshal(msg.Data, &ping); err != nil || ping.Name != "lcars" {
		t.Errorf("ping = %s (%v)", msg.Data, err)
	}

	msg, err = nc.Request("$SRV.INFO.lcars", nil, 5*time.Second)
	if err != nil {
		t.Fatalf("$SRV.INFO.lcars: %v", err)
	}
	var info micro.Info
	if err := json.Unmarshal(msg.Data, &info); err != nil {
		t.Fatal(err)
	}
	if info.Version != strings.TrimPrefix(testVersion, "v") || info.Metadata["build"] != testVersion {
		t.Errorf("version = %q, build = %q; want %s", info.Version, info.Metadata["build"], testVersion)
	}
	var endpoints []string
	for _, ep := range info.Endpoints {
		endpoints = append(endpoints, ep.Subject)
	}
	want := []string{subjectCalendarGet, subjectCamerasList, subjectRemindersList, subjectRemindersSet}
	slices.Sort(endpoints)
	slices.Sort(want)
	if !slices.Equal(endpoints, want) {
		t.Errorf("endpoints = %v, want %v", endpoints, want)
	}
}

// TestNATSExternalServer runs lcars-api against a separate NATS server, which
// then restarts: lcars-api must report it in /healthz and reconnect.
func TestNATSExternalServer(t *testing.T) {
	natsAddr := freeAddr(t)
	ext := startNATSServer(t, natsAddr)
	unused := freeAddr(t)
	p := startServer(t,
		"LCARS_NATS_URL=nats://"+natsAddr,
		"LCARS_NATS_ADDR="+unused,
		"LCARS_CALENDAR_DIR="+fixturesDir(t),
		"LCARS_TIMEZONE=America/New_York",
	)
	if conn, err := net.DialTimeout("tcp", unused, time.Second); err == nil {
		conn.Close()
		t.Errorf("embedded NATS server started on %s despite LCARS_NATS_URL", unused)
	}
	if !p.hasLog(map[string]any{"msg": "nats connected", "server": "nats://" + natsAddr}) {
		t.Errorf("missing nats connected log line\nstdout: %s", p.stdout.String())
	}
	checkFixtureWeekNATS(t, p.natsURL())

	health := func() (int, string) {
		resp := mustDo(t, http.MethodGet, p.url("/healthz"))
		var body map[string]string
		_ = json.Unmarshal(resp.body, &body)
		return resp.StatusCode, body["nats"]
	}
	if code, state := health(); code != http.StatusOK || state != "connected" {
		t.Errorf("healthz = %d nats=%q, want 200 connected", code, state)
	}

	ext.Shutdown()
	ext.WaitForShutdown()
	waitFor(t, 5*time.Second, "healthz to report NATS down", func() bool {
		code, state := health()
		return code == http.StatusServiceUnavailable && state == "disconnected"
	})

	startNATSServer(t, natsAddr)
	waitFor(t, 10*time.Second, "lcars-api to reconnect", func() bool {
		code, _ := health()
		return code == http.StatusOK
	})
	// The service is registered again on the new server.
	waitFor(t, 5*time.Second, "calendar requests to work again", func() bool {
		_, err := requestCalendar(connectNATS(t, p.natsURL()), "2030-03-04", 1)
		return err == nil
	})
	if !p.hasLog(map[string]any{"msg": "nats reconnected"}) {
		t.Errorf("missing nats reconnected log line\nstdout: %s", p.stdout.String())
	}
}

// TestNATSTLS checks that the embedded server speaks TLS, that the
// certificate lists the configured IP, and that the calendar API answers
// on that connection. A second launch uses the generated files via the
// cert and key settings.
func TestNATSTLS(t *testing.T) {
	dir := t.TempDir()
	p := startServer(t,
		"LCARS_NATS_TLS_DIR="+dir,
		"LCARS_NATS_TLS_HOSTS=127.0.0.1",
	)
	if !p.hasLog(map[string]any{"msg": "nats tls enabled", "hosts": "127.0.0.1"}) {
		t.Fatalf("missing tls log\nstdout: %s", p.stdout.String())
	}
	cert := parsePEMCert(t, filepath.Join(dir, "server.pem"))
	if !certHasIP(cert, net.ParseIP("127.0.0.1")) {
		t.Fatalf("certificate IPs = %v, want 127.0.0.1", cert.IPAddresses)
	}

	plain, err := nats.Connect("nats://"+p.natsAddr, nats.Timeout(2*time.Second), nats.MaxReconnects(0))
	if err == nil {
		plain.Close()
		t.Fatal("plain nats connected; tls should be required")
	}
	nc := connectTLS(t, "tls://"+p.natsAddr, filepath.Join(dir, "ca.pem"))
	if _, err := requestCalendar(nc, "", 0); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("calendar over tls = %v, want a 503 (no calendar)", err)
	}

	again := startServer(t,
		"LCARS_NATS_TLS_CERT="+filepath.Join(dir, "server.pem"),
		"LCARS_NATS_TLS_KEY="+filepath.Join(dir, "server.key"),
		"LCARS_NATS_TLS_HOSTS=127.0.0.1",
	)
	nc = connectTLS(t, "tls://"+again.natsAddr, filepath.Join(dir, "ca.pem"))
	if _, err := requestCalendar(nc, "", 0); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("calendar over supplied cert = %v, want a 503", err)
	}
}

func connectTLS(t *testing.T, url, ca string) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(url, nats.RootCAs(ca), nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatalf("connect to %s: %v", url, err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func parsePEMCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatalf("%s is not a PEM certificate", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func certHasIP(cert *x509.Certificate, ip net.IP) bool {
	for _, candidate := range cert.IPAddresses {
		if candidate.Equal(ip) {
			return true
		}
	}
	return false
}

// startNATSServer runs a NATS server on addr for the rest of the test.
func startNATSServer(t *testing.T, addr string) *natsserver.Server {
	t.Helper()
	host, port, _ := net.SplitHostPort(addr)
	n, _ := strconv.Atoi(port)
	ns, err := natsserver.NewServer(&natsserver.Options{Host: host, Port: n, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	ns.Start()
	t.Cleanup(ns.Shutdown)
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatalf("NATS server on %s not ready", addr)
	}
	return ns
}

func writeICS(t *testing.T, path, title string) {
	t.Helper()
	ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//lcars-api//e2e//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:" + strings.ToLower(title) + "@example.com\r\nSUMMARY:" + title +
		"\r\nDTSTART:20300304T120000\r\nDURATION:PT1H\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	if err := os.WriteFile(path, []byte(ics), 0o644); err != nil {
		t.Fatal(err)
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
