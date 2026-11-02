package reminders

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	return New(&memDB{})
}

func TestCreateListAndRoundTrip(t *testing.T) {
	db := &memDB{}
	s := New(db)
	var notified []time.Time
	s.SetOnChange(func(at time.Time) { notified = append(notified, at) })

	first, err := s.Create(t.Context(), "  Bring in recycling bin  ", " Due today · House ", "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Accent != AccentPurple || first.Done || !strings.HasPrefix(first.ID, "rem_") {
		t.Fatalf("created = %+v", first)
	}
	second, err := s.Create(t.Context(), "Send invoices", "Due today · Work", "BLUE")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetDone(t.Context(), first.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDone(t.Context(), first.ID, true); err != nil {
		t.Fatal(err)
	}

	items, updated, err := s.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ID != first.ID || !items[0].Done || items[0].Title != "Bring in recycling bin" {
		t.Fatalf("list = %+v", items)
	}
	if items[1].Accent != AccentBlue || items[1].Detail != "Due today · Work" {
		t.Fatalf("second = %+v", items[1])
	}
	if updated.IsZero() || len(notified) != 3 { // create, create, one real done change
		t.Fatalf("updated %s, notifications %d", updated, len(notified))
	}

	again, _, err := New(db).List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 2 || again[0].ID != first.ID || !again[0].Done || again[1].ID != second.ID {
		t.Fatalf("reloaded = %+v", again)
	}
}

func TestDelete(t *testing.T) {
	s := openTest(t)
	item, err := s.Create(t.Context(), "Print setlists", "Due Sat · Band", AccentOrange)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(t.Context(), item.ID); err != nil {
		t.Fatal(err)
	}
	items, _, err := s.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("list = %+v, want empty", items)
	}
	if err := s.Delete(t.Context(), item.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete again = %v, want ErrNotFound", err)
	}
}

func TestValidation(t *testing.T) {
	s := openTest(t)
	for _, title := range []string{"", "   ", "line\nbreak", strings.Repeat("a", MaxTitle+1)} {
		if _, err := s.Create(t.Context(), title, "", AccentPurple); !errors.As(err, new(*RequestError)) {
			t.Errorf("title %q error = %v, want RequestError", title, err)
		}
	}
	if _, err := s.Create(t.Context(), "Book oil change", strings.Repeat("a", MaxDetail+1), AccentPeach); !errors.As(err, new(*RequestError)) {
		t.Fatalf("long detail error = %v", err)
	}
	if _, err := s.Create(t.Context(), "Book oil change", "Due today", "gold"); !errors.As(err, new(*RequestError)) {
		t.Fatalf("accent error = %v", err)
	}
	for _, id := range []string{"", "reminder", "rem_SHORT", "rem_" + strings.Repeat("a", 12) + "b"} {
		if err := s.SetDone(t.Context(), id, true); !errors.As(err, new(*RequestError)) {
			t.Errorf("set %q = %v, want RequestError", id, err)
		}
		if err := s.Delete(t.Context(), id); !errors.As(err, new(*RequestError)) {
			t.Errorf("delete %q = %v, want RequestError", id, err)
		}
	}
	missing := "rem_" + strings.Repeat("a", 12)
	if err := s.SetDone(t.Context(), missing, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing set = %v", err)
	}
}

func TestFull(t *testing.T) {
	s := openTest(t)
	for i := range MaxReminders {
		if _, err := s.Create(t.Context(), string(rune('A'+i)), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Create(t.Context(), "One more", "", ""); !errors.As(err, new(*RequestError)) {
		t.Fatalf("overfull error = %v", err)
	}
}

func TestConcurrentCreates(t *testing.T) {
	s := openTest(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Create(context.Background(), "Bring in recycling bin", "Due today · House", AccentPurple); err != nil {
				var re *RequestError
				if !errors.As(err, &re) {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	items, _, err := s.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != MaxReminders {
		t.Fatalf("saved %d reminders, want %d", len(items), MaxReminders)
	}
}

func TestListIsACopy(t *testing.T) {
	s := openTest(t)
	if _, err := s.Create(t.Context(), "Bring in recycling bin", "", AccentPurple); err != nil {
		t.Fatal(err)
	}
	items, at, err := s.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	items[0].Title = "changed"
	again, at2, err := s.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if again[0].Title == "changed" || !at.Equal(at2) {
		t.Fatalf("list leaked a mutation: %+v", again)
	}
	if at.Location() != time.UTC {
		t.Errorf("updated location = %s, want UTC", at.Location())
	}
}

// memDB is an in-memory reminder table for tests.
type memDB struct {
	mu   sync.Mutex
	rows []Reminder
	seq  int
}

func (m *memDB) ListReminders(context.Context) ([]Reminder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.rows), nil
}

func (m *memDB) InsertReminder(_ context.Context, item Reminder) (Reminder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.rows) >= MaxReminders {
		return Reminder{}, errFull
	}
	if slices.ContainsFunc(m.rows, func(r Reminder) bool { return r.ID == item.ID }) {
		return Reminder{}, errDuplicate
	}
	m.seq++
	item.CreatedAt = time.Unix(0, int64(m.seq)).UTC()
	item.UpdatedAt = item.CreatedAt
	m.rows = append(m.rows, item)
	return item, nil
}

func (m *memDB) UpdateReminderDone(_ context.Context, id string, done bool) (Reminder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, row := range m.rows {
		if row.ID != id {
			continue
		}
		m.seq++
		row.Done = done
		row.UpdatedAt = time.Unix(0, int64(m.seq)).UTC()
		m.rows[i] = row
		return row, nil
	}
	return Reminder{}, ErrNotFound
}

func (m *memDB) DeleteReminder(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	before := len(m.rows)
	m.rows = slices.DeleteFunc(m.rows, func(r Reminder) bool { return r.ID == id })
	if len(m.rows) == before {
		return ErrNotFound
	}
	return nil
}
