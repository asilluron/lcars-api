//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
	"google.golang.org/protobuf/proto"

	lcarsv1 "github.com/asilluron/lcars-api/gen/go/lcars/v1"
	"github.com/asilluron/lcars-api/internal/reminders"
)

func TestRemindersDisabled(t *testing.T) {
	p := startServer(t)
	base := p.url("")
	resp := mustDo(t, http.MethodGet, base+"/reminders")
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(resp.body), "reminders are not configured") {
		t.Fatalf("GET /reminders = %d %s", resp.StatusCode, resp.body)
	}
	if resp := postReminderRaw(t, base+"/reminders/rem_abcdefghij12", `{"done":true}`); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("POST = %d %s", resp.StatusCode, resp.body)
	}
	nc := connectNATS(t, p.natsURL())
	unset := natsRequest(t, nc, subjectRemindersList, nil)
	if unset.Header.Get(micro.ErrorCodeHeader) != "503" {
		t.Fatalf("NATS list = %s", unset.Header.Get(micro.ErrorCodeHeader))
	}
	unsetSet := natsRequest(t, nc, subjectRemindersSet, &lcarsv1.SetReminderRequest{Id: "rem_abcdefghij12", Done: true})
	if unsetSet.Header.Get(micro.ErrorCodeHeader) != "503" {
		t.Fatalf("NATS set = %s", unsetSet.Header.Get(micro.ErrorCodeHeader))
	}
	for _, op := range []struct{ method, path string }{
		{http.MethodGet, "/admin/api/reminders"},
		{http.MethodPost, "/admin/api/reminders"},
		{http.MethodDelete, "/admin/api/reminders/rem_abcdefghij12"},
	} {
		resp := adminCall(t, base, op.method, op.path, "", map[string]string{"title": "Print setlists"})
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(resp.body), "admin API is disabled") {
			t.Errorf("%s %s = %d %s", op.method, op.path, resp.StatusCode, resp.body)
		}
	}
}

func TestRemindersAdmin(t *testing.T) {
	s := requireSupabase(t)
	email, password := createAdmin(t)
	p := startServer(t, s.env()...)
	base := p.url("")
	token := signIn(t, base, email, password)
	tok := createToken(t, base, token, "Reminder display")
	var created []string
	t.Cleanup(func() { deleteReminders(s, created) })
	track := func(item reminderJSON) reminderJSON {
		created = append(created, item.ID)
		return item
	}

	nc := connectNATS(t, natsURLWith(p.natsURL(), tok))
	sub, err := nc.SubscribeSync(subjectRemindersUpdated)
	if err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	device := basicURL(base, tok)

	// Devices and anonymous callers cannot manage the list.
	if resp := mustDo(t, http.MethodGet, base+"/reminders"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous GET = %d", resp.StatusCode)
	}
	if resp := postReminderRaw(t, base+"/reminders/rem_abcdefghij12", `{"done":true}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous POST = %d", resp.StatusCode)
	}
	for _, bearer := range []string{"", "not-a-session"} {
		for _, op := range []struct{ method, path string }{
			{http.MethodGet, "/admin/api/reminders"},
			{http.MethodPost, "/admin/api/reminders"},
			{http.MethodDelete, "/admin/api/reminders/rem_abcdefghij12"},
		} {
			resp := adminCall(t, base, op.method, op.path, bearer, map[string]string{"title": "x"})
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s bearer %.8q = %d, want 401", op.method, op.path, bearer, resp.StatusCode)
			}
		}
	}
	resp := adminCall(t, base, http.MethodPost, "/admin/api/reminders", tok.Secret, map[string]string{"title": "Bring in recycling bin"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("device secret as bearer = %d", resp.StatusCode)
	}

	for _, id := range []string{"nope", "rem_short"} {
		got := postReminderRaw(t, device+"/reminders/"+id, `{"done":true}`)
		if got.StatusCode != http.StatusBadRequest {
			t.Errorf("POST %s = %d %s, want 400", id, got.StatusCode, got.body)
		}
		msg := natsRequest(t, nc, subjectRemindersSet, &lcarsv1.SetReminderRequest{Id: id, Done: true})
		if code := msg.Header.Get(micro.ErrorCodeHeader); code != "400" {
			t.Errorf("NATS set %s = %s %s, want 400", id, code, msg.Header.Get(micro.ErrorHeader))
		}
	}
	missing := postReminderRaw(t, device+"/reminders/rem_abcdefghij12", `{"done":true}`)
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("missing = %d %s, want 404", missing.StatusCode, missing.body)
	}
	msg := natsRequest(t, nc, subjectRemindersSet, &lcarsv1.SetReminderRequest{Id: "rem_abcdefghij12", Done: true})
	if msg.Header.Get(micro.ErrorCodeHeader) != "404" {
		t.Errorf("NATS missing = %s", msg.Header.Get(micro.ErrorCodeHeader))
	}
	if got := postReminderRaw(t, device+"/reminders/rem_abcdefghij12", `{}`); got.StatusCode != http.StatusBadRequest ||
		!strings.Contains(string(got.body), "is required") {
		t.Errorf("missing done = %d %s", got.StatusCode, got.body)
	}
	if got := postReminderRaw(t, device+"/reminders/rem_abcdefghij12", `{"done":true,"title":"x"}`); got.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown field = %d %s", got.StatusCode, got.body)
	}
	form, err := http.NewRequest(http.MethodPost, device+"/reminders/rem_abcdefghij12", strings.NewReader("done=true"))
	if err != nil {
		t.Fatal(err)
	}
	form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got, err := send(form); err != nil || got.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("form body = %v, %v; want 415", got, err)
	}
	garbage, err := nc.Request(subjectRemindersList, []byte{0xff, 0xff, 0xff}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cover(subjectCoverage, subjectRemindersList)
	if garbage.Header.Get(micro.ErrorCodeHeader) != "400" {
		t.Errorf("garbage list = %s %s", garbage.Header.Get(micro.ErrorCodeHeader), garbage.Header.Get(micro.ErrorHeader))
	}
	raw, err := nc.Request(subjectRemindersSet, []byte{0xff}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cover(subjectCoverage, subjectRemindersSet)
	if raw.Header.Get(micro.ErrorCodeHeader) != "400" {
		t.Errorf("garbage set = %s %s", raw.Header.Get(micro.ErrorCodeHeader), raw.Header.Get(micro.ErrorHeader))
	}

	for _, body := range []any{
		map[string]string{},
		map[string]string{"title": "  "},
		map[string]string{"title": strings.Repeat("a", 81)},
		map[string]string{"title": "Print setlists", "detail": strings.Repeat("b", 121)},
		map[string]string{"title": "Print setlists", "accent": "gold"},
		map[string]any{"title": "Print setlists", "done": true},
	} {
		got := adminCall(t, base, http.MethodPost, "/admin/api/reminders", token, body)
		if got.StatusCode != http.StatusBadRequest {
			t.Errorf("create %v = %d %s, want 400", body, got.StatusCode, got.body)
		}
	}

	existing := adminCall(t, base, http.MethodGet, "/admin/api/reminders", token, nil)
	if existing.StatusCode != http.StatusOK {
		t.Fatalf("admin list = %d %s", existing.StatusCode, existing.body)
	}
	var already reminderList
	if err := json.Unmarshal(existing.body, &already); err != nil {
		t.Fatal(err)
	}
	if reminders.MaxReminders-len(already.Reminders) < 2 {
		t.Fatalf("reminders table has %d rows; this test needs two free slots and only deletes rows it creates", len(already.Reminders))
	}

	first := track(createReminder(t, base, token, "Bring in recycling bin", "Due today · House", ""))
	if first.Accent != "purple" || first.Done || !regexp.MustCompile(`^rem_[a-z0-9]{12}$`).MatchString(first.ID) {
		t.Fatalf("created = %+v", first)
	}
	if strings.Contains(p.stdout.String(), "Bring in recycling bin") {
		t.Fatal("server log contains the reminder title")
	}
	second := track(createReminder(t, base, token, "Send invoices", "Due today · Work", "blue"))
	for range 2 {
		if nextReminderEvent(t, sub).GetUpdatedAtUnix() == 0 {
			t.Fatal("create did not publish an update")
		}
	}
	drainReminderEvents(t, sub)

	listed := getReminders(t, device+"/reminders")
	if len(listed.Reminders) < 2 || listed.Reminders[len(listed.Reminders)-2].Title != first.Title || listed.Reminders[len(listed.Reminders)-1].Accent != "blue" {
		t.Fatalf("device list = %+v", listed.Reminders)
	}
	adminResp := adminCall(t, base, http.MethodGet, "/admin/api/reminders", token, nil)
	if adminResp.StatusCode != http.StatusOK {
		t.Fatalf("admin list = %d %s", adminResp.StatusCode, adminResp.body)
	}
	var adminList reminderList
	if err := json.Unmarshal(adminResp.body, &adminList); err != nil {
		t.Fatal(err)
	}
	if len(adminList.Reminders) != len(listed.Reminders) || adminList.Reminders[len(adminList.Reminders)-1].ID != listed.Reminders[len(listed.Reminders)-1].ID {
		t.Fatalf("admin list = %+v, device list = %+v", adminList.Reminders, listed.Reminders)
	}
	assertSameReminders(t, listed, listRemindersNATS(t, nc, nil))
	assertSameReminders(t, listed, listRemindersNATS(t, nc, &lcarsv1.ListRemindersRequest{}))

	marked := postReminder(t, device+"/reminders/"+first.ID, map[string]bool{"done": true})
	if !marked.Reminders[indexOf(t, marked, first.ID)].Done {
		t.Fatalf("device set = %+v", marked.Reminders)
	}
	if nextReminderEvent(t, sub).GetUpdatedAtUnix() != marked.UpdatedAt.Unix() {
		t.Errorf("event does not match %s", marked.UpdatedAt)
	}
	opened := setReminderNATS(t, nc, &lcarsv1.SetReminderRequest{Id: first.ID, Done: false})
	if opened.GetReminders()[indexOfProto(t, opened, first.ID)].GetDone() {
		t.Fatalf("after NATS set = %+v", opened.GetReminders())
	}
	again := getReminders(t, device+"/reminders")
	assertSameReminders(t, again, opened)
	if nextReminderEvent(t, sub).GetUpdatedAtUnix() != again.UpdatedAt.Unix() {
		t.Errorf("second event does not match %s", again.UpdatedAt)
	}
	drainReminderEvents(t, sub)
	same := postReminder(t, device+"/reminders/"+first.ID, map[string]bool{"done": false})
	if !same.UpdatedAt.Equal(again.UpdatedAt) {
		t.Fatalf("unchanged updated_at = %s, want %s", same.UpdatedAt, again.UpdatedAt)
	}
	if msg, err := sub.NextMsg(200 * time.Millisecond); err == nil {
		t.Fatalf("unexpected event after a no-op set: %s", msg.Data)
	}

	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := p.wait(t); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, p.stderr.String())
	}
	p2 := startServer(t, s.env()...)
	base = p2.url("")
	device = basicURL(base, tok)
	reloaded := getReminders(t, device+"/reminders")
	if reloaded.Reminders[indexOf(t, reloaded, first.ID)].Done || !containsTitle(reloaded, second.Title) {
		t.Fatalf("reloaded = %+v", reloaded.Reminders)
	}
	nc = connectNATS(t, natsURLWith(p2.natsURL(), tok))
	sub, err = nc.SubscribeSync(subjectRemindersUpdated)
	if err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}

	for range reminders.MaxReminders - len(reloaded.Reminders) {
		track(createReminder(t, base, token, "Print setlists", "Due Sat · Band", "orange"))
	}
	full := adminCall(t, base, http.MethodPost, "/admin/api/reminders", token, map[string]string{"title": "Book oil change", "accent": "peach"})
	if full.StatusCode != http.StatusBadRequest || !strings.Contains(string(full.body), "full") {
		t.Fatalf("overfull = %d %s", full.StatusCode, full.body)
	}

	drainReminderEvents(t, sub)
	removed := adminCall(t, base, http.MethodDelete, "/admin/api/reminders/"+second.ID, token, nil)
	if removed.StatusCode != http.StatusNoContent || len(removed.body) != 0 {
		t.Fatalf("delete = %d %s", removed.StatusCode, removed.body)
	}
	if nextReminderEvent(t, sub).GetUpdatedAtUnix() == 0 {
		t.Fatal("delete did not publish an update")
	}
	left := listRemindersNATS(t, nc, nil)
	if len(left.Reminders) != reminders.MaxReminders-1 {
		t.Fatalf("after delete, %d reminders", len(left.Reminders))
	}
	for _, item := range left.Reminders {
		if item.Id == second.ID {
			t.Fatalf("deleted reminder still listed: %+v", item)
		}
	}
	gone := adminCall(t, base, http.MethodDelete, "/admin/api/reminders/"+second.ID, token, nil)
	if gone.StatusCode != http.StatusNotFound {
		t.Fatalf("delete again = %d %s", gone.StatusCode, gone.body)
	}
	badID := adminCall(t, base, http.MethodDelete, "/admin/api/reminders/not-an-id", token, nil)
	if badID.StatusCode != http.StatusBadRequest {
		t.Fatalf("delete bad id = %d %s", badID.StatusCode, badID.body)
	}
}

func indexOf(t *testing.T, list reminderList, id string) int {
	t.Helper()
	for i, item := range list.Reminders {
		if item.ID == id {
			return i
		}
	}
	t.Fatalf("reminder %s not in %+v", id, list.Reminders)
	return 0
}

func indexOfProto(t *testing.T, list *lcarsv1.ListRemindersResponse, id string) int {
	t.Helper()
	for i, item := range list.GetReminders() {
		if item.GetId() == id {
			return i
		}
	}
	t.Fatalf("reminder %s not in list", id)
	return 0
}

func containsTitle(list reminderList, title string) bool {
	for _, item := range list.Reminders {
		if item.Title == title {
			return true
		}
	}
	return false
}

func deleteReminders(s supabaseInfo, ids []string) {
	for _, id := range ids {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.APIURL+"/rest/v1/reminders?id=eq."+url.QueryEscape(id), nil)
		if err != nil {
			cancel()
			continue
		}
		req.Header.Set("Apikey", s.SecretKey)
		req.Header.Set("Authorization", "Bearer "+s.SecretKey)
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil {
			resp.Body.Close()
		}
	}
}

type reminderJSON struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Accent string `json:"accent"`
	Done   bool   `json:"done"`
}

type reminderList struct {
	UpdatedAt time.Time      `json:"updated_at"`
	Reminders []reminderJSON `json:"reminders"`
}

func getReminders(t *testing.T, rawURL string) reminderList {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := send(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d %s", rawURL, resp.StatusCode, resp.body)
	}
	var list reminderList
	if err := json.Unmarshal(resp.body, &list); err != nil {
		t.Fatal(err)
	}
	return list
}

func postReminder(t *testing.T, rawURL string, body any) reminderList {
	t.Helper()
	resp := postReminderRaw(t, rawURL, mustJSON(t, body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s = %d %s", rawURL, resp.StatusCode, resp.body)
	}
	var list reminderList
	if err := json.Unmarshal(resp.body, &list); err != nil {
		t.Fatal(err)
	}
	return list
}

func postReminderRaw(t *testing.T, rawURL, body string) *response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, rawURL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := send(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func createReminder(t *testing.T, base, bearer, title, detail, accent string) reminderJSON {
	t.Helper()
	resp := adminCall(t, base, http.MethodPost, "/admin/api/reminders", bearer, map[string]string{
		"title": title, "detail": detail, "accent": accent,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode, resp.body)
	}
	var item reminderJSON
	if err := json.Unmarshal(resp.body, &item); err != nil {
		t.Fatal(err)
	}
	if item.Title != title || item.Detail != detail {
		t.Fatalf("created = %+v", item)
	}
	return item
}

func listRemindersNATS(t *testing.T, nc *nats.Conn, req proto.Message) *lcarsv1.ListRemindersResponse {
	t.Helper()
	msg := natsRequest(t, nc, subjectRemindersList, req)
	if err := replyError(msg); err != nil {
		t.Fatal(err)
	}
	var resp lcarsv1.ListRemindersResponse
	if err := proto.Unmarshal(msg.Data, &resp); err != nil {
		t.Fatal(err)
	}
	return &resp
}

func setReminderNATS(t *testing.T, nc *nats.Conn, req *lcarsv1.SetReminderRequest) *lcarsv1.ListRemindersResponse {
	t.Helper()
	msg := natsRequest(t, nc, subjectRemindersSet, req)
	if err := replyError(msg); err != nil {
		t.Fatal(err)
	}
	var resp lcarsv1.ListRemindersResponse
	if err := proto.Unmarshal(msg.Data, &resp); err != nil {
		t.Fatal(err)
	}
	return &resp
}

func drainReminderEvents(t *testing.T, sub *nats.Subscription) {
	t.Helper()
	for {
		if _, err := sub.NextMsg(200 * time.Millisecond); err != nil {
			return
		}
	}
}

func nextReminderEvent(t *testing.T, sub *nats.Subscription) *lcarsv1.RemindersUpdated {
	t.Helper()
	cover(subjectCoverage, subjectRemindersUpdated)
	msg, err := sub.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var event lcarsv1.RemindersUpdated
	if err := proto.Unmarshal(msg.Data, &event); err != nil {
		t.Fatal(err)
	}
	return &event
}

func assertSameReminders(t *testing.T, httpList reminderList, natsList *lcarsv1.ListRemindersResponse) {
	t.Helper()
	if httpList.UpdatedAt.Unix() != natsList.GetUpdatedAtUnix() {
		t.Errorf("updated_at %s vs unix %d", httpList.UpdatedAt, natsList.GetUpdatedAtUnix())
	}
	if len(httpList.Reminders) != len(natsList.GetReminders()) {
		t.Fatalf("HTTP %d reminders, NATS %d", len(httpList.Reminders), len(natsList.GetReminders()))
	}
	for i, item := range httpList.Reminders {
		got := natsList.GetReminders()[i]
		if item.ID != got.GetId() || item.Title != got.GetTitle() || item.Detail != got.GetDetail() ||
			item.Accent != got.GetAccent() || item.Done != got.GetDone() {
			t.Errorf("reminder %d HTTP %+v, NATS %+v", i, item, got)
		}
	}
}
