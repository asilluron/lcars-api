// Package locks talks to the Seam API to list smart locks and lock or unlock
// them. The API key and base URL come from the operator's config. Request
// input is never used as a host, and a device id is checked before it is sent.
package locks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// DefaultURL is Seam's production API when seam.url is unset.
const DefaultURL = "https://connect.getseam.com"

// NotConfigured is the message a device gets when seam.api_key is unset.
// HTTP and NATS both use it.
const NotConfigured = "locks are not configured: set seam.api_key in the config file"

const (
	maxBodyBytes   = 1 << 20
	requestTimeout = 15 * time.Second
)

// A Seam device_id is a UUID.
var deviceIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// StatusError is a failure with a status and a message safe to send to a device.
// Err, when set, is for the server log only.
type StatusError struct {
	Code int
	Msg  string
	Err  error
}

func (e *StatusError) Error() string {
	if e.Err != nil {
		return e.Msg + ": " + e.Err.Error()
	}
	return e.Msg
}

func (e *StatusError) Unwrap() error { return e.Err }

// Lock is one smart lock Seam reported.
type Lock struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Locked    bool   `json:"locked"`
	Online    bool   `json:"online"`
	CanLock   bool   `json:"can_lock"`
	CanUnlock bool   `json:"can_unlock"`
}

// Client talks to one Seam workspace.
type Client struct {
	base   string
	apiKey string
	http   *http.Client
}

// New returns a client for the Seam API at base (such as DefaultURL) using
// apiKey. base has no trailing slash.
func New(base, apiKey string) *Client {
	return &Client{
		base:   strings.TrimSuffix(base, "/"),
		apiKey: apiKey,
		http: &http.Client{
			Timeout: requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// List returns the locks Seam currently reports, in Seam's order.
func (c *Client) List(ctx context.Context) ([]Lock, error) {
	var out struct {
		Devices []seamDevice `json:"devices"`
	}
	if err := c.post(ctx, "/locks/list", map[string]any{}, &out); err != nil {
		return nil, err
	}
	locks := make([]Lock, 0, len(out.Devices))
	for _, d := range out.Devices {
		if !deviceIDPattern.MatchString(d.DeviceID) {
			return nil, fmt.Errorf("seam returned an invalid device_id %q", d.DeviceID)
		}
		locks = append(locks, Lock{
			ID:        d.DeviceID,
			Name:      d.displayName(),
			Locked:    d.Properties.Locked,
			Online:    d.Properties.Online,
			CanLock:   d.CanRemotelyLock,
			CanUnlock: d.CanRemotelyUnlock,
		})
	}
	return locks, nil
}

// LockDoor asks Seam to lock the device once. It does not poll or re-issue:
// a jammed bolt will not finish by waiting, and a second command while the
// first is still running only makes the motor grind again.
func (c *Client) LockDoor(ctx context.Context, id string) error {
	return c.act(ctx, id, "/locks/lock_door")
}

// UnlockDoor asks Seam to unlock the device once. Same one-shot rule as
// LockDoor.
func (c *Client) UnlockDoor(ctx context.Context, id string) error {
	return c.act(ctx, id, "/locks/unlock_door")
}

func (c *Client) act(ctx context.Context, id, path string) error {
	if !deviceIDPattern.MatchString(id) {
		return &StatusError{Code: http.StatusBadRequest, Msg: "invalid lock id"}
	}

	var out struct {
		ActionAttempt actionAttempt `json:"action_attempt"`
	}
	if err := c.post(ctx, path, map[string]any{"device_id": id}, &out); err != nil {
		return err
	}
	return result(out.ActionAttempt)
}

// result accepts Seam's immediate reply. "pending" means the command was
// accepted; we leave the outcome to a later list refresh instead of polling.
func result(attempt actionAttempt) error {
	switch attempt.Status {
	case "success", "pending", "":
		return nil
	case "error":
		msg := "lock action failed"
		if attempt.Error != nil && attempt.Error.Message != "" {
			// Keep the device-facing message generic; details stay in Err.
			return &StatusError{
				Code: http.StatusBadGateway,
				Msg:  msg,
				Err:  fmt.Errorf("seam action %s: %s", attempt.Error.Type, attempt.Error.Message),
			}
		}
		return &StatusError{Code: http.StatusBadGateway, Msg: msg}
	default:
		return &StatusError{
			Code: http.StatusBadGateway,
			Msg:  "lock action failed",
			Err:  fmt.Errorf("seam action status %q", attempt.Status),
		}
	}
}

func (c *Client) post(ctx context.Context, path string, body any, dst any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	u, err := url.JoinPath(c.base, path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "lcars-api")

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return &StatusError{Code: http.StatusGatewayTimeout, Msg: "lock action timed out", Err: err}
		}
		return unavailableErr(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return unavailableErr(err)
	}
	if len(data) > maxBodyBytes {
		return unavailableErr(fmt.Errorf("seam response exceeds %d bytes", maxBodyBytes))
	}
	switch resp.StatusCode {
	case http.StatusOK:
		if err := json.Unmarshal(data, dst); err != nil {
			return fmt.Errorf("read seam response: %w", err)
		}
		return nil
	case http.StatusNotFound:
		return &StatusError{Code: http.StatusNotFound, Msg: "lock not found"}
	case http.StatusUnauthorized, http.StatusForbidden:
		return &StatusError{Code: http.StatusServiceUnavailable, Msg: "locks are unavailable", Err: fmt.Errorf("seam status %d", resp.StatusCode)}
	default:
		return &StatusError{Code: http.StatusServiceUnavailable, Msg: "locks are unavailable", Err: fmt.Errorf("seam status %d: %s", resp.StatusCode, truncate(data))}
	}
}

type seamDevice struct {
	DeviceID          string `json:"device_id"`
	DisplayName       string `json:"display_name"`
	Nickname          string `json:"nickname"`
	CanRemotelyLock   bool   `json:"can_remotely_lock"`
	CanRemotelyUnlock bool   `json:"can_remotely_unlock"`
	Properties        struct {
		Name   string `json:"name"`
		Locked bool   `json:"locked"`
		Online bool   `json:"online"`
	} `json:"properties"`
}

func (d seamDevice) displayName() string {
	for _, name := range []string{d.DisplayName, d.Nickname, d.Properties.Name} {
		if name != "" {
			return name
		}
	}
	return ""
}

type actionAttempt struct {
	ActionAttemptID string `json:"action_attempt_id"`
	Status          string `json:"status"`
	Error           *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func unavailableErr(err error) error {
	return &StatusError{Code: http.StatusServiceUnavailable, Msg: "locks are unavailable", Err: err}
}

func truncate(b []byte) string {
	const n = 200
	s := strings.TrimSpace(string(b))
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
