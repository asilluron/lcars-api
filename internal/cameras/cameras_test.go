package cameras

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cameras" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("forwarded an Authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"sn":"CAM1","name":"Front door","camera":true},{"sn":"HUB1","name":"","camera":false}]`)
	}))
	defer srv.Close()

	got, err := New(srv.URL).List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []Camera{
		{SN: "CAM1", Name: "Front door", HasCamera: true, StreamPath: "/cameras/CAM1/stream", SnapshotPath: "/cameras/CAM1/snapshot"},
		{SN: "HUB1", Name: "", HasCamera: false, StreamPath: "/cameras/HUB1/stream", SnapshotPath: "/cameras/HUB1/snapshot"},
	}
	if len(got) != len(want) {
		t.Fatalf("cameras = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("camera %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestListRejectsBadSerial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"sn":"../x","name":"nope","camera":true}]`)
	}))
	defer srv.Close()
	if _, err := New(srv.URL).List(t.Context()); err == nil {
		t.Fatal("accepted a serial that is not letters and digits")
	}
}

func TestOpenStreamAndSnapshot(t *testing.T) {
	closed := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/stream/CAM1":
			w.Header().Set("Content-Type", "video/octet-stream")
			_, _ = w.Write([]byte{0, 0, 0, 1})
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
			close(closed)
		case "/snapshot/CAM1":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte{0xff, 0xd8, 0xff, 0xd9})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL)

	body, err := c.OpenStream(t.Context(), "CAM1")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(body, buf); err != nil {
		t.Fatal(err)
	}
	body.Close()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("closing the stream did not cancel the proxy request")
	}

	jpeg, err := c.Snapshot(t.Context(), "CAM1")
	if err != nil || len(jpeg) != 4 {
		t.Fatalf("snapshot = %v, %v", jpeg, err)
	}
	if _, err := c.Snapshot(t.Context(), "MISSING"); !isStatus(err, http.StatusNotFound) {
		t.Fatalf("missing snapshot = %v, want 404", err)
	}
}

func TestInvalidSerialDoesNotCallProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("proxy was called")
	}))
	defer srv.Close()
	c := New(srv.URL)
	for _, sn := range []string{"", "CAM-1", "../etc", strings.Repeat("A", 33)} {
		if _, err := c.OpenStream(t.Context(), sn); !isStatus(err, http.StatusBadRequest) {
			t.Errorf("OpenStream %q = %v, want 400", sn, err)
		}
		if _, err := c.Snapshot(t.Context(), sn); !isStatus(err, http.StatusBadRequest) {
			t.Errorf("Snapshot %q = %v, want 400", sn, err)
		}
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	secret := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "secret-frame")
	}))
	defer secret.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, secret.URL+"/stream/CAM1", http.StatusFound)
	}))
	defer srv.Close()

	_, err := New(srv.URL).OpenStream(t.Context(), "CAM1")
	if !isStatus(err, http.StatusServiceUnavailable) {
		t.Fatalf("err = %v, want 503", err)
	}
	if err != nil && strings.Contains(err.Error(), "secret-frame") {
		t.Fatal("followed a redirect")
	}
}

func TestProxyDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()
	_, err := New(srv.URL).List(context.Background())
	if !isStatus(err, http.StatusServiceUnavailable) {
		t.Fatalf("err = %v, want 503", err)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Err == nil {
		t.Fatal("transport failure was not kept for the log")
	}
	if strings.Contains(se.Msg, "127.0.0.1") {
		t.Fatal("client message includes the proxy address")
	}
}

func TestStreamCancel(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		body, err := New(srv.URL).OpenStream(ctx, "CAM1")
		if err == nil {
			_, err = io.Copy(io.Discard, body)
			body.Close()
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !isStatus(err, http.StatusServiceUnavailable) && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling did not stop the stream")
	}
}

func isStatus(err error, code int) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == code
}
