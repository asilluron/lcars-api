package natsapi

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
	"google.golang.org/protobuf/proto"

	lcarsv1 "github.com/asilluron/lcars-api/gen/go/lcars/v1"
	"github.com/asilluron/lcars-api/internal/reminders"
)

func TestRemindersNATS(t *testing.T) {
	store := reminders.New(&reminderMem{})
	nc := startService(t, Deps{Reminders: store})
	sub, err := nc.SubscribeSync(SubjectRemindersUpdated)
	if err != nil {
		t.Fatal(err)
	}

	msg := natsReminder(t, nc, SubjectRemindersList, &lcarsv1.ListRemindersRequest{})
	var list lcarsv1.ListRemindersResponse
	if err := proto.Unmarshal(msg.Data, &list); err != nil || len(list.GetReminders()) != 0 {
		t.Fatalf("empty list: %v %+v", err, &list)
	}

	// The NATS service does not create reminders. Seed one through the store
	// the handler reads, which publishes because SetOnChange is not set here.
	item, err := store.Create(t.Context(), "Bring in recycling bin", "Due today · House", reminders.AccentPurple)
	if err != nil {
		t.Fatal(err)
	}
	store.SetOnChange(RemindersChanged(nc, discard))

	msg = natsReminder(t, nc, SubjectRemindersSet, &lcarsv1.SetReminderRequest{Id: item.ID, Done: true})
	if err := proto.Unmarshal(msg.Data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.GetReminders()) != 1 || !list.GetReminders()[0].GetDone() || list.GetReminders()[0].GetTitle() != item.Title {
		t.Fatalf("set = %+v", list.GetReminders())
	}
	event, err := sub.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var updated lcarsv1.RemindersUpdated
	if err := proto.Unmarshal(event.Data, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.GetUpdatedAtUnix() != list.GetUpdatedAtUnix() {
		t.Errorf("event %d, list %d", updated.GetUpdatedAtUnix(), list.GetUpdatedAtUnix())
	}

	missing := natsReminder(t, nc, SubjectRemindersSet, &lcarsv1.SetReminderRequest{Id: "rem_abcdefghij12", Done: true})
	if missing.Header.Get(micro.ErrorCodeHeader) != "404" {
		t.Fatalf("missing = %s", missing.Header.Get(micro.ErrorHeader))
	}
}

func TestRemindersUnset(t *testing.T) {
	nc := startService(t, Deps{})
	msg := natsReminder(t, nc, SubjectRemindersList, nil)
	if msg.Header.Get(micro.ErrorCodeHeader) != "503" {
		t.Fatalf("list = %s %s", msg.Header.Get(micro.ErrorCodeHeader), msg.Header.Get(micro.ErrorHeader))
	}
	msg = natsReminder(t, nc, SubjectRemindersSet, &lcarsv1.SetReminderRequest{Id: "rem_abcdefghij12"})
	if msg.Header.Get(micro.ErrorCodeHeader) != "503" {
		t.Fatalf("set = %s", msg.Header.Get(micro.ErrorCodeHeader))
	}
}

// reminderMem is the reminder table for this package's tests.
type reminderMem struct {
	mu   sync.Mutex
	rows []reminders.Reminder
}

func (m *reminderMem) ListReminders(context.Context) ([]reminders.Reminder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.rows), nil
}

func (m *reminderMem) InsertReminder(_ context.Context, item reminders.Reminder) (reminders.Reminder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	item.CreatedAt, item.UpdatedAt = now, now
	m.rows = append(m.rows, item)
	return item, nil
}

func (m *reminderMem) UpdateReminderDone(_ context.Context, id string, done bool) (reminders.Reminder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, row := range m.rows {
		if row.ID != id {
			continue
		}
		row.Done = done
		row.UpdatedAt = time.Now().UTC()
		m.rows[i] = row
		return row, nil
	}
	return reminders.Reminder{}, reminders.ErrNotFound
}

func (m *reminderMem) DeleteReminder(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	before := len(m.rows)
	m.rows = slices.DeleteFunc(m.rows, func(row reminders.Reminder) bool { return row.ID == id })
	if len(m.rows) == before {
		return reminders.ErrNotFound
	}
	return nil
}

func natsReminder(t *testing.T, nc *nats.Conn, subject string, req proto.Message) *nats.Msg {
	t.Helper()
	var data []byte
	if req != nil {
		var err error
		if data, err = proto.Marshal(req); err != nil {
			t.Fatal(err)
		}
	}
	msg, err := nc.Request(subject, data, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if subject == SubjectRemindersList && msg.Header.Get(micro.ErrorCodeHeader) == "" {
		return msg
	}
	return msg
}
