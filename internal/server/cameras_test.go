package server

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asilluron/lcars-api/internal/cameras"
)

func TestCamerasDisabled(t *testing.T) {
	srv := newTestServer(t)
	for _, path := range []string{"/cameras", "/cameras/CAM1/stream", "/cameras/CAM1/snapshot"} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), cameras.NotConfigured) {
			t.Errorf("GET %s = %d %s", path, resp.StatusCode, body)
		}
	}
}

func TestCamerasProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cameras":
			_, _ = io.WriteString(w, `[{"sn":"CAM1","name":"Front door","camera":true}]`)
		case "/snapshot/CAM1":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte{0xff, 0xd8, 0xff, 0xd9})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	logger := slog.New(slog.DiscardHandler)
	srv := httptest.NewServer(New(logger, Deps{Cameras: cameras.New(upstream.URL)}))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/cameras")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"/cameras/CAM1/stream"`) {
		t.Fatalf("GET /cameras = %d %s", resp.StatusCode, body)
	}

	resp, err = srv.Client().Get(srv.URL + "/cameras/CAM1/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	shot, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/jpeg" || len(shot) != 4 {
		t.Fatalf("snapshot = %d %q %v", resp.StatusCode, resp.Header.Get("Content-Type"), shot)
	}

	resp, err = srv.Client().Get(srv.URL + "/cameras/CAM-1/stream")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad serial = %d, want 400", resp.StatusCode)
	}
}

// TestCameraSnapshotOutlivesWriteTimeout is the reason the snapshot handler
// clears the server's deadlines: the proxy may take a live still that needs
// longer than the 15s write timeout.
func TestCameraSnapshotOutlivesWriteTimeout(t *testing.T) {
	const jpeg = "\xff\xd8\xff\xd9"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/snapshot/CAM1" {
			http.NotFound(w, r)
			return
		}
		time.Sleep(400 * time.Millisecond)
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = io.WriteString(w, jpeg)
	}))
	defer upstream.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler:           New(slog.New(slog.DiscardHandler), Deps{Cameras: cameras.New(upstream.URL)}),
		ReadHeaderTimeout: 200 * time.Millisecond,
		ReadTimeout:       200 * time.Millisecond,
		WriteTimeout:      200 * time.Millisecond,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	resp, err := http.Get("http://" + ln.Addr().String() + "/cameras/CAM1/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != jpeg {
		t.Fatalf("snapshot = %q, want jpeg", got)
	}
}

// TestCameraStreamOutlivesWriteTimeout is the reason the stream handler
// clears the server's read and write deadlines: a live view is longer than
// the 15s timeout on the real server.
func TestCameraStreamOutlivesWriteTimeout(t *testing.T) {
	const frame = "FRAMEBYTES"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stream/CAM1" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "video/octet-stream")
		_, _ = io.WriteString(w, frame)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(400 * time.Millisecond)
		_, _ = io.WriteString(w, frame)
	}))
	defer upstream.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler: New(slog.New(slog.DiscardHandler), Deps{Cameras: cameras.New(upstream.URL)}),
		// Shorter than the gap between frames, so a deadline that is not
		// cleared cuts the stream.
		ReadHeaderTimeout: 200 * time.Millisecond,
		ReadTimeout:       200 * time.Millisecond,
		WriteTimeout:      200 * time.Millisecond,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	resp, err := http.Get("http://" + ln.Addr().String() + "/cameras/CAM1/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != frame+frame {
		t.Fatalf("stream = %q, want both frames", got)
	}
}
