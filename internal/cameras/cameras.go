// Package cameras reads the Eufy camera proxy (the eufy/ process) and turns
// its catalog into paths a device can open on this server.
//
// The proxy URL comes from the operator's config. Request input is never used
// as a host, and a serial is checked before it is added to a URL.
package cameras

import (
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

// A serial is what the proxy accepts: letters and digits, up to 32.
var serialPattern = regexp.MustCompile(`^[A-Za-z0-9]{1,32}$`)

const (
	// NotConfigured is the message a device gets when eufy.url is unset.
	// HTTP and NATS both use it.
	NotConfigured = "cameras are not configured: set eufy.url in the config file"
	// maxListBytes is how much of a catalog response is read.
	maxListBytes = 1 << 20
	// maxSnapshotBytes is how much of a JPEG is read. A larger body is refused.
	maxSnapshotBytes = 10 << 20
	// headerTimeout is how long to wait for the proxy to send response headers.
	// A live stream has no total timeout after that.
	headerTimeout = 30 * time.Second
)

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

// Camera is one device, with the HTTP paths on this server (not on the proxy)
// that serve its video and its latest stored snapshot.
type Camera struct {
	SN           string `json:"sn"`
	Name         string `json:"name"`
	HasCamera    bool   `json:"has_camera"`
	StreamPath   string `json:"stream_path"`
	SnapshotPath string `json:"snapshot_path"`
}

// Client talks to one camera proxy.
type Client struct {
	base string
	http *http.Client
}

// New returns a client for the proxy at base, such as "http://127.0.0.1:3000".
// base has no trailing slash.
func New(base string) *Client {
	return &Client{
		base: strings.TrimSuffix(base, "/"),
		http: &http.Client{
			// No Client.Timeout: a live stream is open for as long as the
			// device is reading. ResponseHeaderTimeout bounds the wait for
			// headers, including the start of a stream.
			Transport: &http.Transport{ResponseHeaderTimeout: headerTimeout},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// List returns the cameras the proxy currently reports. Paths point at this
// server. The proxy's order is kept.
func (c *Client) List(ctx context.Context) ([]Camera, error) {
	ctx, cancel := context.WithTimeout(ctx, headerTimeout)
	defer cancel()
	body, err := c.get(ctx, "cameras")
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var raw []struct {
		SN     string `json:"sn"`
		Name   string `json:"name"`
		Camera bool   `json:"camera"`
	}
	dec := json.NewDecoder(io.LimitReader(body, maxListBytes))
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("read camera list: %w", err)
	}
	out := make([]Camera, 0, len(raw))
	for _, cam := range raw {
		if !serialPattern.MatchString(cam.SN) {
			return nil, fmt.Errorf("camera proxy returned an invalid serial %q", cam.SN)
		}
		out = append(out, Camera{
			SN:           cam.SN,
			Name:         cam.Name,
			HasCamera:    cam.Camera,
			StreamPath:   streamPath(cam.SN),
			SnapshotPath: snapshotPath(cam.SN),
		})
	}
	return out, nil
}

// OpenStream starts the live Annex-B video for sn. The caller must close the
// body; closing it, or cancelling ctx, stops that viewer on the proxy.
// A camera the proxy does not have is a 404.
func (c *Client) OpenStream(ctx context.Context, sn string) (io.ReadCloser, error) {
	if !serialPattern.MatchString(sn) {
		return nil, &StatusError{Code: http.StatusBadRequest, Msg: "invalid serial"}
	}
	resp, err := c.fetch(ctx, "stream", sn)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, &StatusError{Code: http.StatusNotFound, Msg: "camera not found"}
	}
	if resp.StatusCode != http.StatusOK {
		discard(resp.Body)
		return nil, unavailable(resp.StatusCode)
	}
	return resp.Body, nil
}

// Snapshot reads the latest stored JPEG for sn. A miss is a 404.
func (c *Client) Snapshot(ctx context.Context, sn string) ([]byte, error) {
	if !serialPattern.MatchString(sn) {
		return nil, &StatusError{Code: http.StatusBadRequest, Msg: "invalid serial"}
	}
	ctx, cancel := context.WithTimeout(ctx, headerTimeout)
	defer cancel()
	resp, err := c.fetch(ctx, "snapshot", sn)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, &StatusError{Code: http.StatusNotFound, Msg: "camera not found"}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, unavailable(resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "image/jpeg") {
		return nil, fmt.Errorf("camera proxy snapshot content type %q", ct)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSnapshotBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read snapshot: %w", err)
	}
	if len(data) > maxSnapshotBytes {
		return nil, fmt.Errorf("camera proxy snapshot exceeds %d bytes", maxSnapshotBytes)
	}
	if len(data) == 0 {
		return nil, &StatusError{Code: http.StatusNotFound, Msg: "camera not found"}
	}
	return data, nil
}

// get reads a proxy path that does not include a serial, such as "cameras".
func (c *Client) get(ctx context.Context, elem string) (io.ReadCloser, error) {
	u, err := url.JoinPath(c.base, elem)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "lcars-api")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, unavailableErr(err)
	}
	if resp.StatusCode != http.StatusOK {
		discard(resp.Body)
		return nil, unavailable(resp.StatusCode)
	}
	return resp.Body, nil
}

// fetch calls /<kind>/<sn>. sn must already be a valid serial.
func (c *Client) fetch(ctx context.Context, kind, sn string) (*http.Response, error) {
	u, err := url.JoinPath(c.base, kind, sn)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "lcars-api")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, unavailableErr(err)
	}
	return resp, nil
}

func unavailable(status int) error {
	return &StatusError{Code: http.StatusServiceUnavailable, Msg: "cameras are unavailable", Err: fmt.Errorf("proxy status %d", status)}
}

func unavailableErr(err error) error {
	return &StatusError{Code: http.StatusServiceUnavailable, Msg: "cameras are unavailable", Err: err}
}

func streamPath(sn string) string   { return "/cameras/" + sn + "/stream" }
func snapshotPath(sn string) string { return "/cameras/" + sn + "/snapshot" }

func discard(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 4096))
	_ = body.Close()
}
