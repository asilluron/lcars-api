// Package devices manages the tokens IoT devices use to reach the API.
//
// A token is a username (its ID, such as "dev_7f3k2m9q4x8w") and a secret.
// Devices send them as HTTP Basic auth credentials, or as the user and
// password when connecting to NATS. Tokens are stored in Supabase with only
// a SHA-256 of the secret; the secret is shown once, when the token is
// created.
//
// Checks are answered from an in-memory copy of the tokens, so they don't
// wait on the database (the NATS server checks credentials synchronously),
// and devices keep working if the database is briefly unreachable. The copy
// is updated on every change made through the Store, and re-read
// periodically to pick up changes made directly in the database.
package devices

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/asilluron/lcars-api/internal/supabase"
)

// MaxNameLen is the longest token name allowed, in characters.
const MaxNameLen = 100

// ErrNotFound is returned by Revoke for an unknown token.
var ErrNotFound = errors.New("device token not found")

// ErrInvalidName is returned by Create for an empty or overlong name.
var ErrInvalidName = fmt.Errorf("name must be 1 to %d characters", MaxNameLen)

// idPattern matches token IDs. The database checks the same pattern.
var idPattern = regexp.MustCompile(`^dev_[a-z0-9]{12}$`)

// DB is where tokens are stored; *supabase.Client implements it.
type DB interface {
	ListDeviceTokens(ctx context.Context) ([]supabase.DeviceToken, error)
	InsertDeviceToken(ctx context.Context, t supabase.DeviceToken) (supabase.DeviceToken, error)
	DeleteDeviceToken(ctx context.Context, id string) error
}

// Token describes a device token, without its secret.
type Token struct {
	ID        string
	Name      string
	CreatedAt time.Time
}

// Store checks device credentials and manages tokens.
type Store struct {
	db     DB
	logger *slog.Logger
	// onRemove is called with the ID of each token that stops being valid.
	onRemove func(id string)

	mu     sync.RWMutex
	hashes map[string][sha256.Size]byte // token ID -> SHA-256 of the secret
	loaded bool
}

// NewStore returns a Store backed by db. onRemove, if non-nil, is called
// with the ID of every token that is revoked or disappears from the
// database, so connections using it can be closed.
func NewStore(db DB, logger *slog.Logger, onRemove func(id string)) *Store {
	return &Store{db: db, logger: logger, onRemove: onRemove, hashes: map[string][sha256.Size]byte{}}
}

// Check reports whether id and secret are a valid device token.
func (s *Store) Check(id, secret string) bool {
	s.mu.RLock()
	want, ok := s.hashes[id]
	s.mu.RUnlock()
	got := sha256.Sum256([]byte(secret))
	return ok && subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

// Loaded reports whether the tokens have been read from the database at
// least once. Until then, every Check fails.
func (s *Store) Loaded() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loaded
}

// Load re-reads the tokens from the database.
func (s *Store) Load(ctx context.Context) error {
	_, err := s.list(ctx)
	return err
}

// retryInterval is how soon Run retries while the tokens have never been
// loaded, so devices aren't locked out for long when the database starts
// after lcars-api.
const retryInterval = 5 * time.Second

// Run reloads the tokens every interval until ctx is done. Failures are
// logged, and the last tokens read stay in effect.
func (s *Store) Run(ctx context.Context, interval time.Duration) {
	for {
		wait := interval
		if !s.Loaded() {
			wait = min(interval, retryInterval)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if err := s.Load(ctx); err != nil && ctx.Err() == nil {
			s.logger.Error("reload device tokens", "err", err)
		}
	}
}

// List returns every token, oldest first, read from the database.
func (s *Store) List(ctx context.Context) ([]Token, error) {
	rows, err := s.list(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Token, len(rows))
	for i, r := range rows {
		out[i] = Token{ID: r.ID, Name: r.Name, CreatedAt: r.CreatedAt}
	}
	return out, nil
}

// list reads the tokens and replaces the in-memory copy with them.
func (s *Store) list(ctx context.Context) ([]supabase.DeviceToken, error) {
	rows, err := s.db.ListDeviceTokens(ctx)
	if err != nil {
		return nil, fmt.Errorf("list device tokens: %w", err)
	}
	hashes := make(map[string][sha256.Size]byte, len(rows))
	for _, r := range rows {
		var h [sha256.Size]byte
		if n, err := hex.Decode(h[:], []byte(r.SecretHash)); err != nil || n != sha256.Size {
			s.logger.Warn("ignoring device token with a malformed hash", "id", r.ID)
			continue
		}
		hashes[r.ID] = h
	}

	s.mu.Lock()
	old, wasLoaded := s.hashes, s.loaded
	s.hashes, s.loaded = hashes, true
	s.mu.Unlock()

	if !wasLoaded || !maps.Equal(old, hashes) {
		s.logger.Info("device tokens loaded", "count", len(hashes))
	}
	for id, h := range old {
		if h2, ok := hashes[id]; !ok || h2 != h {
			s.removed(id)
		}
	}
	return rows, nil
}

// Create makes a new token and returns it with its secret, which is not
// stored and cannot be retrieved again. createdBy is the admin's user ID.
func (s *Store) Create(ctx context.Context, name, createdBy string) (Token, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > MaxNameLen {
		return Token{}, "", ErrInvalidName
	}
	secret := newSecret()
	hash := sha256.Sum256([]byte(secret))
	row := supabase.DeviceToken{ID: newID(), Name: name, SecretHash: hex.EncodeToString(hash[:])}
	if createdBy != "" {
		row.CreatedBy = &createdBy
	}
	stored, err := s.db.InsertDeviceToken(ctx, row)
	if err != nil {
		return Token{}, "", fmt.Errorf("create device token: %w", err)
	}

	s.mu.Lock()
	s.hashes[stored.ID] = hash
	s.mu.Unlock()
	s.logger.Info("device token created", "id", stored.ID)
	return Token{ID: stored.ID, Name: stored.Name, CreatedAt: stored.CreatedAt}, secret, nil
}

// Revoke deletes a token. It stops working immediately.
func (s *Store) Revoke(ctx context.Context, id string) error {
	if !idPattern.MatchString(id) {
		return ErrNotFound
	}
	err := s.db.DeleteDeviceToken(ctx, id)
	switch {
	case errors.Is(err, supabase.ErrNotFound):
		// Already gone from the database; make sure it is gone here too.
		err = ErrNotFound
	case err != nil:
		return fmt.Errorf("revoke device token: %w", err)
	default:
		s.logger.Info("device token revoked", "id", id)
	}

	s.mu.Lock()
	_, had := s.hashes[id]
	delete(s.hashes, id)
	s.mu.Unlock()
	if had {
		s.removed(id)
	}
	return err
}

func (s *Store) removed(id string) {
	if s.onRemove != nil {
		s.onRemove(id)
	}
}

// newID returns a token ID: "dev_" and 12 random lowercase letters and
// digits (60 bits; IDs are not secret, only unique).
func newID() string {
	return "dev_" + strings.ToLower(rand.Text()[:12])
}

// newSecret returns a 256-bit random secret, base64url-encoded (43
// characters), which is safe in Basic auth and in NATS URLs.
func newSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails
	return base64.RawURLEncoding.EncodeToString(b)
}
