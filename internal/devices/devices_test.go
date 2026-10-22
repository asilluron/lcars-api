package devices

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asilluron/lcars-api/internal/supabase"
)

// fakeDB is an in-memory DB.
type fakeDB struct {
	mu   sync.Mutex
	rows []supabase.DeviceToken
	err  error // returned by every call if set
}

func (f *fakeDB) ListDeviceTokens(context.Context) ([]supabase.DeviceToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.rows), f.err
}

func (f *fakeDB) InsertDeviceToken(_ context.Context, t supabase.DeviceToken) (supabase.DeviceToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return supabase.DeviceToken{}, f.err
	}
	t.CreatedAt = time.Date(2030, 3, 4, 9, 0, 0, 0, time.UTC)
	f.rows = append(f.rows, t)
	return t, nil
}

func (f *fakeDB) DeleteDeviceToken(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	n := len(f.rows)
	f.rows = slices.DeleteFunc(f.rows, func(r supabase.DeviceToken) bool { return r.ID == id })
	if len(f.rows) == n {
		return supabase.ErrNotFound
	}
	return nil
}

func newTestStore(t *testing.T) (*Store, *fakeDB, *[]string) {
	t.Helper()
	db := &fakeDB{}
	var removed []string
	s := NewStore(db, slog.New(slog.DiscardHandler), func(id string) { removed = append(removed, id) })
	return s, db, &removed
}

func TestCreateAndCheck(t *testing.T) {
	s, db, _ := newTestStore(t)
	tok, secret, err := s.Create(t.Context(), "  Kitchen display ", "admin-1")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^dev_[a-z0-9]{12}$`).MatchString(tok.ID) {
		t.Errorf("ID = %q, want dev_ and 12 lowercase letters or digits", tok.ID)
	}
	if tok.Name != "Kitchen display" {
		t.Errorf("Name = %q, want it trimmed", tok.Name)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(secret) {
		t.Errorf("secret = %q, want 43 base64url characters", secret)
	}
	row := db.rows[0]
	if strings.Contains(row.SecretHash, secret) || len(row.SecretHash) != 64 {
		t.Errorf("stored hash = %q, want a hex SHA-256 and not the secret", row.SecretHash)
	}
	if row.CreatedBy == nil || *row.CreatedBy != "admin-1" {
		t.Errorf("CreatedBy = %v, want admin-1", row.CreatedBy)
	}

	if !s.Check(tok.ID, secret) {
		t.Error("Check(new token) = false")
	}
	for _, c := range [][2]string{{tok.ID, "wrong"}, {tok.ID, ""}, {"dev_unknown00000", secret}, {"", ""}} {
		if s.Check(c[0], c[1]) {
			t.Errorf("Check(%q, %q) = true", c[0], c[1])
		}
	}

	// Each token is different.
	tok2, secret2, err := s.Create(t.Context(), "Hallway", "")
	if err != nil {
		t.Fatal(err)
	}
	if tok2.ID == tok.ID || secret2 == secret || db.rows[1].CreatedBy != nil {
		t.Errorf("second token %+v (secret %q) repeats the first, or has a creator", tok2, secret2)
	}
}

func TestCreateInvalidName(t *testing.T) {
	s, db, _ := newTestStore(t)
	for _, name := range []string{"", "   ", strings.Repeat("x", MaxNameLen+1)} {
		if _, _, err := s.Create(t.Context(), name, ""); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Create(%q) = %v, want ErrInvalidName", name, err)
		}
	}
	// Length is counted in characters, not bytes.
	if _, _, err := s.Create(t.Context(), strings.Repeat("é", MaxNameLen), ""); err != nil {
		t.Errorf("Create(%d accented letters) = %v", MaxNameLen, err)
	}
	if len(db.rows) != 1 {
		t.Errorf("stored %d rows, want 1", len(db.rows))
	}
}

func TestRevoke(t *testing.T) {
	s, db, removed := newTestStore(t)
	tok, secret, err := s.Create(t.Context(), "Kitchen display", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(t.Context(), tok.ID); err != nil {
		t.Fatal(err)
	}
	if s.Check(tok.ID, secret) {
		t.Error("revoked token still valid")
	}
	if len(db.rows) != 0 {
		t.Errorf("rows = %v, want none", db.rows)
	}
	if !slices.Equal(*removed, []string{tok.ID}) {
		t.Errorf("onRemove calls = %v, want [%s]", *removed, tok.ID)
	}
	for _, id := range []string{tok.ID, "dev_000000000000", "not-an-id", "dev_x&id=neq.y"} {
		if err := s.Revoke(t.Context(), id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Revoke(%q) = %v, want ErrNotFound", id, err)
		}
	}
}

func TestRevokeDBError(t *testing.T) {
	s, db, removed := newTestStore(t)
	tok, secret, err := s.Create(t.Context(), "Kitchen display", "")
	if err != nil {
		t.Fatal(err)
	}
	db.err = errors.New("database down")
	if err := s.Revoke(t.Context(), tok.ID); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("Revoke = %v, want the database error", err)
	}
	// Still stored, so still valid: memory must match the database.
	if !s.Check(tok.ID, secret) || len(*removed) != 0 {
		t.Error("token invalidated although the delete failed")
	}
}

func TestLoad(t *testing.T) {
	s, db, removed := newTestStore(t)
	if s.Loaded() {
		t.Error("Loaded before Load")
	}
	keep, keepSecret, _ := s.Create(t.Context(), "Kitchen display", "")
	gone, goneSecret, _ := s.Create(t.Context(), "Hallway", "")

	// Rows deleted or broken directly in the database stop working on the
	// next Load, and are reported as removed.
	db.rows = slices.DeleteFunc(db.rows, func(r supabase.DeviceToken) bool { return r.ID == gone.ID })
	db.rows = append(db.rows, supabase.DeviceToken{ID: "dev_malformed000", SecretHash: "nope"})
	if err := s.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !s.Loaded() {
		t.Error("not Loaded after Load")
	}
	if !s.Check(keep.ID, keepSecret) || s.Check(gone.ID, goneSecret) {
		t.Error("Load did not apply the database's tokens")
	}
	if !slices.Equal(*removed, []string{gone.ID}) {
		t.Errorf("onRemove calls = %v, want [%s]", *removed, gone.ID)
	}

	// A failed Load keeps the tokens already loaded.
	db.err = errors.New("database down")
	if err := s.Load(t.Context()); err == nil {
		t.Error("Load = nil, want the database error")
	}
	if !s.Check(keep.ID, keepSecret) {
		t.Error("tokens dropped by a failed Load")
	}
}

func TestList(t *testing.T) {
	s, _, _ := newTestStore(t)
	a, _, _ := s.Create(t.Context(), "Kitchen display", "")
	b, _, _ := s.Create(t.Context(), "Hallway", "")
	got, err := s.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Errorf("List = %+v, want [%+v %+v]", got, a, b)
	}
}
