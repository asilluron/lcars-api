// Package supabase is a minimal client for the parts of Supabase lcars-api
// uses: Auth (to sign admins in and check their sessions) and the Data API
// (PostgREST) for the device_tokens and reminders tables.
//
// It authenticates with the project's secret key (or the legacy service_role
// key), which bypasses row-level security, so it must only ever be used on
// the server.
package supabase

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrInvalidCredentials is returned by SignIn for a wrong email or password,
// and by User for an invalid or expired session.
var ErrInvalidCredentials = errors.New("invalid credentials")

// ErrRateLimited is returned by SignIn when Supabase Auth refuses more
// attempts for now.
var ErrRateLimited = errors.New("too many sign-in attempts")

// ErrNotFound is returned when a row to delete does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when an insert collides with an existing row.
var ErrConflict = errors.New("conflict")

// Client calls one Supabase project.
type Client struct {
	baseURL   string
	secretKey string
	http      *http.Client
}

// New returns a client for the project at baseURL (such as
// "http://127.0.0.1:54321") using secretKey.
func New(baseURL, secretKey string) *Client {
	return &Client{
		baseURL:   strings.TrimSuffix(baseURL, "/"),
		secretKey: secretKey,
		http:      &http.Client{Timeout: 10 * time.Second},
	}
}

// Session is a signed-in admin's access token.
type Session struct {
	AccessToken string `json:"access_token"`
	// ExpiresAt is in seconds since the Unix epoch.
	ExpiresAt int64 `json:"expires_at"`
	User      User  `json:"user"`
}

// User is a Supabase Auth user.
type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// SignIn exchanges an email and password for a session.
func (c *Client) SignIn(ctx context.Context, email, password string) (Session, error) {
	body := map[string]string{"email": email, "password": password}
	var s Session
	err := c.do(ctx, http.MethodPost, "/auth/v1/token?grant_type=password", "", body, &s, nil)
	if apiErr, ok := errors.AsType[*apiError](err); ok {
		switch apiErr.status {
		case http.StatusBadRequest:
			return Session{}, ErrInvalidCredentials
		case http.StatusTooManyRequests:
			return Session{}, ErrRateLimited
		}
	}
	return s, err
}

// User returns the user an access token belongs to, checking that the token
// is valid and unexpired.
func (c *Client) User(ctx context.Context, accessToken string) (User, error) {
	var u User
	err := c.do(ctx, http.MethodGet, "/auth/v1/user", accessToken, nil, &u, nil)
	if apiErr, ok := errors.AsType[*apiError](err); ok &&
		(apiErr.status == http.StatusUnauthorized || apiErr.status == http.StatusForbidden) {
		return User{}, ErrInvalidCredentials
	}
	return u, err
}

// DeviceToken is a row of the device_tokens table.
type DeviceToken struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	SecretHash string    `json:"secret_hash"`
	CreatedAt  time.Time `json:"created_at,omitzero"`
	CreatedBy  *string   `json:"created_by,omitempty"`
}

// tokensPath is the Data API path of the device_tokens table.
const tokensPath = "/rest/v1/device_tokens" //nolint:gosec // a URL path, not a credential

// ListDeviceTokens returns every device token, oldest first.
func (c *Client) ListDeviceTokens(ctx context.Context) ([]DeviceToken, error) {
	var rows []DeviceToken
	err := c.do(ctx, http.MethodGet, tokensPath+"?select=*&order=created_at.asc,id.asc", "", nil, &rows, nil)
	return rows, err
}

// InsertDeviceToken adds t and returns the stored row.
func (c *Client) InsertDeviceToken(ctx context.Context, t DeviceToken) (DeviceToken, error) {
	var rows []DeviceToken
	err := c.do(ctx, http.MethodPost, tokensPath, "", t, &rows, http.Header{"Prefer": {"return=representation"}})
	if apiErr, ok := errors.AsType[*apiError](err); ok && apiErr.status == http.StatusConflict {
		return DeviceToken{}, ErrConflict
	}
	if err != nil {
		return DeviceToken{}, err
	}
	if len(rows) != 1 {
		return DeviceToken{}, fmt.Errorf("insert device token: got %d rows back, want 1", len(rows))
	}
	return rows[0], nil
}

// DeleteDeviceToken deletes the token with the given ID, or returns
// ErrNotFound.
func (c *Client) DeleteDeviceToken(ctx context.Context, id string) error {
	var rows []DeviceToken
	err := c.do(ctx, http.MethodDelete, tokensPath+"?id=eq."+url.QueryEscape(id), "", nil, &rows,
		http.Header{"Prefer": {"return=representation"}})
	if err == nil && len(rows) == 0 {
		return ErrNotFound
	}
	return err
}

// Reminder is a row of the reminders table.
type Reminder struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Detail    string    `json:"detail"`
	Accent    string    `json:"accent"`
	Done      bool      `json:"done"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const remindersPath = "/rest/v1/reminders"

// ListReminders returns every reminder, oldest first.
func (c *Client) ListReminders(ctx context.Context) ([]Reminder, error) {
	var rows []Reminder
	err := c.do(ctx, http.MethodGet, remindersPath+"?select=id,title,detail,accent,done,created_at,updated_at&order=created_at.asc,id.asc", "", nil, &rows, nil)
	return rows, err
}

// InsertReminder adds a reminder and returns the stored row.
func (c *Client) InsertReminder(ctx context.Context, row Reminder) (Reminder, error) {
	in := struct {
		ID     string `json:"id"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
		Accent string `json:"accent"`
		Done   bool   `json:"done"`
	}{row.ID, row.Title, row.Detail, row.Accent, row.Done}
	var rows []Reminder
	err := c.do(ctx, http.MethodPost, remindersPath, "", in, &rows, http.Header{"Prefer": {"return=representation"}})
	if apiErr, ok := errors.AsType[*apiError](err); ok && apiErr.status == http.StatusConflict {
		return Reminder{}, ErrConflict
	}
	if err != nil {
		return Reminder{}, err
	}
	if len(rows) != 1 {
		return Reminder{}, fmt.Errorf("insert reminder: got %d rows back, want 1", len(rows))
	}
	return rows[0], nil
}

// UpdateReminderDone sets done on the reminder with id and returns the row.
func (c *Client) UpdateReminderDone(ctx context.Context, id string, done bool) (Reminder, error) {
	var rows []Reminder
	err := c.do(ctx, http.MethodPatch, remindersPath+"?id=eq."+url.QueryEscape(id), "",
		map[string]bool{"done": done}, &rows, http.Header{"Prefer": {"return=representation"}})
	if err != nil {
		return Reminder{}, err
	}
	if len(rows) == 0 {
		return Reminder{}, ErrNotFound
	}
	return rows[0], nil
}

// DeleteReminder deletes the reminder with id, or returns ErrNotFound.
func (c *Client) DeleteReminder(ctx context.Context, id string) error {
	var rows []Reminder
	err := c.do(ctx, http.MethodDelete, remindersPath+"?id=eq."+url.QueryEscape(id), "", nil, &rows,
		http.Header{"Prefer": {"return=representation"}})
	if err == nil && len(rows) == 0 {
		return ErrNotFound
	}
	return err
}

// ResponseBody returns the body of a failed Supabase response, or "" when
// err is not one. Callers use it to recognize a constraint message without
// showing the body to a client.
func ResponseBody(err error) string {
	apiErr, ok := errors.AsType[*apiError](err)
	if !ok {
		return ""
	}
	return apiErr.body
}

// apiError is a non-2xx response. Its body is kept for the server log; it
// must not be passed on to clients.
type apiError struct {
	method, path string
	status       int
	body         string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("supabase %s %s: %d %s", e.method, e.path, e.status, e.body)
}

// do sends a request with in (if non-nil) as the JSON body, and decodes a
// JSON response into out (if non-nil). bearer is the user's access token, or
// "" to act as the service role.
func (c *Client) do(ctx context.Context, method, path, bearer string, in, out any, header http.Header) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer == "" {
		bearer = c.secretKey
	}
	req.Header.Set("Apikey", c.secretKey)
	req.Header.Set("Authorization", "Bearer "+bearer)

	resp, err := c.http.Do(req)
	if err != nil {
		// The error includes the URL, which has no credentials in it.
		return fmt.Errorf("supabase: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("supabase %s %s: read response: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		p, _, _ := strings.Cut(path, "?")
		return &apiError{method: method, path: p, status: resp.StatusCode, body: string(bytes.TrimSpace(data))}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("supabase %s %s: decode response: %w", method, path, err)
	}
	return nil
}
