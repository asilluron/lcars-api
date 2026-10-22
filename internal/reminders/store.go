// Package reminders stores the to-do list the display shows.
//
// The list lives in Supabase. The server is the only writer: admins add and
// remove reminders, and a device may only mark an existing reminder done or
// open. The id in a request is a lookup key, never part of a filesystem path.
package reminders

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/asilluron/lcars-api/internal/supabase"
)

const (
	// MaxReminders is how many reminders the list may hold. The display
	// shows six rows.
	MaxReminders = 6
	// MaxTitle is the longest title, in characters.
	MaxTitle = 80
	// MaxDetail is the longest detail line, in characters.
	MaxDetail = 120

	// AccentPurple is the default reminder color.
	AccentPurple = "purple"
	// AccentBlue is the blue reminder color.
	AccentBlue = "blue"
	// AccentOrange is the orange reminder color.
	AccentOrange = "orange"
	// AccentPeach is the peach reminder color.
	AccentPeach = "peach"

	// NotConfigured is the message clients get when Supabase is not configured.
	NotConfigured = "reminders are not configured: set supabase.url and supabase.secret_key in the config file"
)

// Accents lists the accent values Create accepts, in the order the admin
// page shows them.
var Accents = []string{AccentPurple, AccentBlue, AccentOrange, AccentPeach}

// RequestError is a rejected create or update. Handlers send Msg to the
// client with status 400.
type RequestError struct{ Msg string }

func (e *RequestError) Error() string { return e.Msg }

// ErrNotFound is returned when an id matches the pattern but no reminder
// has it.
var ErrNotFound = errors.New("no such reminder")

var (
	errFull      = &RequestError{Msg: fmt.Sprintf("the reminder list is full (%d)", MaxReminders)}
	errID        = &RequestError{Msg: `invalid "id"`}
	errDuplicate = errors.New("duplicate reminder id")
)

// idPattern matches the ids this package generates.
var idPattern = regexp.MustCompile(`^rem_[a-z0-9]{12}$`)

// Reminder is one to-do row. CreatedAt orders the list and is not sent to
// clients. UpdatedAt is the last change to this row.
type Reminder struct {
	ID        string
	Title     string
	Detail    string
	Accent    string
	Done      bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// DB is the reminder table.
type DB interface {
	ListReminders(ctx context.Context) ([]Reminder, error)
	InsertReminder(ctx context.Context, item Reminder) (Reminder, error)
	UpdateReminderDone(ctx context.Context, id string, done bool) (Reminder, error)
	DeleteReminder(ctx context.Context, id string) error
}

// Store is the reminder list. A change is committed before it is visible
// to List.
type Store struct {
	db DB

	mu       sync.Mutex
	onChange func(time.Time)
}

// Supabase adapts a Supabase client to DB.
func Supabase(client *supabase.Client) DB { return clientDB{client} }

// New returns a list backed by db.
func New(db DB) *Store { return &Store{db: db} }

// SetOnChange registers fn, called after a change has been saved, with the
// time of that change. It replaces any previous function.
func (s *Store) SetOnChange(fn func(time.Time)) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

// List returns the reminders, oldest first, and when the list last changed.
// An empty list has the zero time.
func (s *Store) List(ctx context.Context) ([]Reminder, time.Time, error) {
	items, err := s.db.ListReminders(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	items = slices.Clone(items)
	slices.SortFunc(items, compareReminders)
	return items, latest(items), nil
}

// Create appends a reminder. An empty accent is purple. The new reminder
// is not done.
func (s *Store) Create(ctx context.Context, title, detail, accent string) (Reminder, error) {
	title, err := cleanTitle(title)
	if err != nil {
		return Reminder{}, err
	}
	detail, err = cleanDetail(detail)
	if err != nil {
		return Reminder{}, err
	}
	accent, err = cleanAccent(accent)
	if err != nil {
		return Reminder{}, err
	}

	s.mu.Lock()
	item, at, notify, err := s.create(ctx, title, detail, accent)
	fn := s.onChange
	s.mu.Unlock()
	if err != nil {
		return Reminder{}, err
	}
	if notify && fn != nil {
		fn(at)
	}
	return item, nil
}

// SetDone stores whether the reminder is done. Setting the state it already
// has changes nothing and does not notify. An id of the wrong shape is a
// RequestError; a well-formed id that is not in the list is ErrNotFound.
func (s *Store) SetDone(ctx context.Context, id string, done bool) error {
	if !idPattern.MatchString(id) {
		return errID
	}
	s.mu.Lock()
	at, notify, err := s.setDone(ctx, id, done)
	fn := s.onChange
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if notify && fn != nil {
		fn(at)
	}
	return nil
}

// Delete removes the reminder. The id rules match SetDone.
func (s *Store) Delete(ctx context.Context, id string) error {
	if !idPattern.MatchString(id) {
		return errID
	}
	s.mu.Lock()
	at, err := s.delete(ctx, id)
	fn := s.onChange
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if fn != nil {
		fn(at)
	}
	return nil
}

func (s *Store) create(ctx context.Context, title, detail, accent string) (Reminder, time.Time, bool, error) {
	items, err := s.db.ListReminders(ctx)
	if err != nil {
		return Reminder{}, time.Time{}, false, err
	}
	if len(items) >= MaxReminders {
		return Reminder{}, time.Time{}, false, errFull
	}
	item := Reminder{Title: title, Detail: detail, Accent: accent}
	var saved Reminder
	for range 5 {
		item.ID = "rem_" + strings.ToLower(rand.Text()[:12])
		saved, err = s.db.InsertReminder(ctx, item)
		if !errors.Is(err, errDuplicate) {
			break
		}
	}
	if err != nil {
		return Reminder{}, time.Time{}, false, err
	}
	return saved, saved.UpdatedAt.UTC(), true, nil
}

func (s *Store) setDone(ctx context.Context, id string, done bool) (time.Time, bool, error) {
	items, err := s.db.ListReminders(ctx)
	if err != nil {
		return time.Time{}, false, err
	}
	i := slices.IndexFunc(items, func(r Reminder) bool { return r.ID == id })
	if i < 0 {
		return time.Time{}, false, ErrNotFound
	}
	if items[i].Done == done {
		return time.Time{}, false, nil
	}
	saved, err := s.db.UpdateReminderDone(ctx, id, done)
	if err != nil {
		return time.Time{}, false, err
	}
	return saved.UpdatedAt.UTC(), true, nil
}

func (s *Store) delete(ctx context.Context, id string) (time.Time, error) {
	items, err := s.db.ListReminders(ctx)
	if err != nil {
		return time.Time{}, err
	}
	if !slices.ContainsFunc(items, func(r Reminder) bool { return r.ID == id }) {
		return time.Time{}, ErrNotFound
	}
	if err := s.db.DeleteReminder(ctx, id); err != nil {
		return time.Time{}, err
	}
	left := slices.DeleteFunc(items, func(r Reminder) bool { return r.ID == id })
	if at := latest(left); !at.IsZero() {
		return at, nil
	}
	return time.Now().UTC(), nil
}

func compareReminders(a, b Reminder) int {
	if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
		return c
	}
	return strings.Compare(a.ID, b.ID)
}

func latest(items []Reminder) time.Time {
	var at time.Time
	for _, item := range items {
		if item.UpdatedAt.After(at) {
			at = item.UpdatedAt
		}
	}
	return at.UTC()
}

func cleanTitle(title string) (string, error) {
	title = strings.TrimSpace(title)
	if title == "" || len([]rune(title)) > MaxTitle || hasControl(title) {
		return "", &RequestError{Msg: fmt.Sprintf(`invalid "title": want 1 to %d characters`, MaxTitle)}
	}
	return title, nil
}

func cleanDetail(detail string) (string, error) {
	detail = strings.TrimSpace(detail)
	if len([]rune(detail)) > MaxDetail || hasControl(detail) {
		return "", &RequestError{Msg: fmt.Sprintf(`invalid "detail": want at most %d characters`, MaxDetail)}
	}
	return detail, nil
}

func cleanAccent(accent string) (string, error) {
	accent = strings.ToLower(strings.TrimSpace(accent))
	if accent == "" {
		return AccentPurple, nil
	}
	if !slices.Contains(Accents, accent) {
		return "", &RequestError{Msg: `invalid "accent": want purple, blue, orange, or peach`}
	}
	return accent, nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// clientDB stores reminders in Supabase.
type clientDB struct{ client *supabase.Client }

func (db clientDB) ListReminders(ctx context.Context) ([]Reminder, error) {
	rows, err := db.client.ListReminders(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]Reminder, len(rows))
	for i, row := range rows {
		items[i] = Reminder{
			ID: row.ID, Title: row.Title, Detail: row.Detail, Accent: row.Accent, Done: row.Done,
			CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(),
		}
	}
	return items, nil
}

func (db clientDB) InsertReminder(ctx context.Context, item Reminder) (Reminder, error) {
	row, err := db.client.InsertReminder(ctx, supabase.Reminder{
		ID: item.ID, Title: item.Title, Detail: item.Detail, Accent: item.Accent, Done: item.Done,
	})
	if errors.Is(err, supabase.ErrConflict) {
		return Reminder{}, errDuplicate
	}
	if strings.Contains(supabase.ResponseBody(err), "the reminder list is full") {
		return Reminder{}, errFull
	}
	if err != nil {
		return Reminder{}, err
	}
	item.CreatedAt = row.CreatedAt.UTC()
	item.UpdatedAt = row.UpdatedAt.UTC()
	return item, nil
}

func (db clientDB) UpdateReminderDone(ctx context.Context, id string, done bool) (Reminder, error) {
	row, err := db.client.UpdateReminderDone(ctx, id, done)
	if errors.Is(err, supabase.ErrNotFound) {
		return Reminder{}, ErrNotFound
	}
	if err != nil {
		return Reminder{}, err
	}
	return Reminder{
		ID: row.ID, Title: row.Title, Detail: row.Detail, Accent: row.Accent, Done: row.Done,
		CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(),
	}, nil
}

func (db clientDB) DeleteReminder(ctx context.Context, id string) error {
	err := db.client.DeleteReminder(ctx, id)
	if errors.Is(err, supabase.ErrNotFound) {
		return ErrNotFound
	}
	return err
}
