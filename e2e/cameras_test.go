//go:build e2e

package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	lcarsv1 "github.com/asilluron/lcars-api/gen/go/lcars/v1"
)

const subjectCamerasList = "lcars.cameras.list"

func TestCameras(t *testing.T) {
	const frame = "FRAMEBYTES"
	closed := make(chan struct{})
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/cameras":
			_, _ = io.WriteString(w, `[{"sn":"CAM1","name":"Front door","camera":true}]`)
		case "/stream/CAM1":
			w.Header().Set("Content-Type", "video/octet-stream")
			_, _ = io.WriteString(w, frame)
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
	defer upstream.Close()

	p := startServer(t, "LCARS_EUFY_URL="+upstream.URL)
	if !p.hasLog(map[string]any{"msg": "cameras enabled"}) {
		t.Errorf("missing cameras enabled log\nstdout: %s", p.stdout.String())
	}

	resp := mustDo(t, http.MethodGet, p.url("/cameras"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /cameras = %d %s", resp.StatusCode, resp.body)
	}
	var list struct {
		Cameras []struct {
			SN           string `json:"sn"`
			Name         string `json:"name"`
			HasCamera    bool   `json:"has_camera"`
			StreamPath   string `json:"stream_path"`
			SnapshotPath string `json:"snapshot_path"`
		} `json:"cameras"`
	}
	if err := json.Unmarshal(resp.body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Cameras) != 1 || list.Cameras[0].SN != "CAM1" || list.Cameras[0].Name != "Front door" ||
		!list.Cameras[0].HasCamera || list.Cameras[0].StreamPath != "/cameras/CAM1/stream" ||
		list.Cameras[0].SnapshotPath != "/cameras/CAM1/snapshot" {
		t.Fatalf("catalog = %+v", list.Cameras)
	}

	nc := connectNATS(t, p.natsURL())
	msg := natsRequest(t, nc, subjectCamerasList, &lcarsv1.ListCamerasRequest{})
	if err := replyError(msg); err != nil {
		t.Fatal(err)
	}
	var got lcarsv1.ListCamerasResponse
	if err := proto.Unmarshal(msg.Data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.GetCameras()) != 1 || got.GetCameras()[0].GetSn() != list.Cameras[0].SN ||
		got.GetCameras()[0].GetStreamPath() != list.Cameras[0].StreamPath ||
		got.GetCameras()[0].GetHasCamera() != list.Cameras[0].HasCamera {
		t.Fatalf("NATS catalog = %+v", got.GetCameras())
	}
	cover(subjectCoverage, subjectCamerasList)
	raw, err := nc.Request(subjectCamerasList, []byte{0xff, 0xff, 0xff, 0xff, 0xff}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Header.Get("Nats-Service-Error-Code") != "400" {
		t.Errorf("bad protobuf = %s, want 400", raw.Header.Get("Nats-Service-Error-Code"))
	}

	before := hits.Load()
	badSerial := mustDo(t, http.MethodGet, p.url("/cameras/CAM-1/stream"))
	if badSerial.StatusCode != http.StatusBadRequest || hits.Load() != before {
		t.Errorf("bad serial = %d (proxy hits %d -> %d), want 400 and no proxy call", badSerial.StatusCode, before, hits)
	}
	missing := mustDo(t, http.MethodGet, p.url("/cameras/OTHER/snapshot"))
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("missing snapshot = %d, want 404", missing.StatusCode)
	}

	shot := mustDo(t, http.MethodGet, p.url("/cameras/CAM1/snapshot"))
	if shot.StatusCode != http.StatusOK || shot.Header.Get("Content-Type") != "image/jpeg" || len(shot.body) != 4 {
		t.Fatalf("snapshot = %d %q (%d bytes)", shot.StatusCode, shot.Header.Get("Content-Type"), len(shot.body))
	}

	stream := readCameraStream(t, p.url("/cameras/CAM1/stream"))
	if string(stream) != frame {
		t.Fatalf("stream = %q", stream)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the device stream did not stop the proxy request")
	}
	logs := p.stdout.String() + p.stderr.String()
	if strings.Contains(logs, frame) {
		t.Error("logs contain camera frame bytes")
	}
}

func TestCamerasDisabled(t *testing.T) {
	p := startServer(t)
	if !p.hasLog(map[string]any{"msg": "cameras disabled: eufy.url is not set"}) {
		t.Errorf("missing cameras disabled log\nstdout: %s", p.stdout.String())
	}
	resp := mustDo(t, http.MethodGet, p.url("/cameras"))
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(resp.body), "eufy.url") {
		t.Fatalf("GET /cameras = %d %s", resp.StatusCode, resp.body)
	}
	nc := connectNATS(t, p.natsURL())
	msg := natsRequest(t, nc, subjectCamerasList, &lcarsv1.ListCamerasRequest{})
	if err := replyError(msg); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("NATS = %v, want 503", err)
	}
}

func TestCamerasNeedToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cameras":
			_, _ = io.WriteString(w, `[{"sn":"CAM1","name":"Front door","camera":true}]`)
		case "/stream/CAM1":
			_, _ = io.WriteString(w, "FRAMEBYTES")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	p, admin := authServer(t, "LCARS_EUFY_URL="+upstream.URL)
	base := p.url("")
	if resp := mustDo(t, http.MethodGet, base+"/cameras"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /cameras without a token = %d, want 401", resp.StatusCode)
	}
	if resp := mustDo(t, http.MethodGet, base+"/cameras/CAM1/stream"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET stream without a token = %d, want 401", resp.StatusCode)
	}

	tok := createToken(t, base, admin, "Kitchen display")
	authed := basicURL(base, tok)
	resp := mustDo(t, http.MethodGet, authed+"/cameras")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(resp.body), "CAM1") {
		t.Fatalf("GET /cameras with a token = %d %s", resp.StatusCode, resp.body)
	}
	stream := mustDo(t, http.MethodGet, authed+"/cameras/CAM1/stream")
	if stream.StatusCode != http.StatusOK || string(stream.body) != "FRAMEBYTES" {
		t.Fatalf("GET stream with a token = %d %q", stream.StatusCode, stream.body)
	}
	if strings.Contains(p.stdout.String()+p.stderr.String(), "FRAMEBYTES") {
		t.Error("logs contain camera frame bytes")
	}

	nc, err := nats.Connect(natsURLWith(p.natsURL(), tok), nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	msg := natsRequest(t, nc, subjectCamerasList, &lcarsv1.ListCamerasRequest{})
	if err := replyError(msg); err != nil {
		t.Fatal(err)
	}
	var got lcarsv1.ListCamerasResponse
	if err := proto.Unmarshal(msg.Data, &got); err != nil || len(got.GetCameras()) != 1 {
		t.Fatalf("NATS cameras = %v %v", got.GetCameras(), err)
	}
}

func readCameraStream(t *testing.T, url string) []byte {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	cover(pathCoverage, req.URL.Path)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET stream = %d %s", resp.StatusCode, body)
	}
	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf)
	if n == 0 {
		t.Fatal(err)
	}
	return buf[:n]
}
