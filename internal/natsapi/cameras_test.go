package natsapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nats-io/nats.go/micro"
	"google.golang.org/protobuf/proto"

	lcarsv1 "github.com/asilluron/lcars-api/gen/go/lcars/v1"
	"github.com/asilluron/lcars-api/internal/cameras"
)

func TestCamerasList(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cameras" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `[{"sn":"CAM1","name":"Front door","camera":true}]`)
	}))
	defer upstream.Close()

	nc := startService(t, Deps{Cameras: cameras.New(upstream.URL)})
	msg, err := nc.Request(SubjectCamerasList, mustMarshal(t, &lcarsv1.ListCamerasRequest{}), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if code := msg.Header.Get(micro.ErrorCodeHeader); code != "" {
		t.Fatalf("error reply %s: %s", code, msg.Header.Get(micro.ErrorHeader))
	}
	var got lcarsv1.ListCamerasResponse
	if err := proto.Unmarshal(msg.Data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.GetCameras()) != 1 {
		t.Fatalf("cameras = %d", len(got.GetCameras()))
	}
	cam := got.GetCameras()[0]
	if cam.GetSn() != "CAM1" || cam.GetName() != "Front door" || !cam.GetHasCamera() ||
		cam.GetStreamPath() != "/cameras/CAM1/stream" || cam.GetSnapshotPath() != "/cameras/CAM1/snapshot" {
		t.Errorf("camera = %+v", cam)
	}
}

func TestCamerasListDisabled(t *testing.T) {
	nc := startService(t, Deps{})
	msg, err := nc.Request(SubjectCamerasList, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Header.Get(micro.ErrorCodeHeader) != "503" || msg.Header.Get(micro.ErrorHeader) != cameras.NotConfigured {
		t.Fatalf("reply = %s %s", msg.Header.Get(micro.ErrorCodeHeader), msg.Header.Get(micro.ErrorHeader))
	}
}

func TestCamerasListBadBody(t *testing.T) {
	nc := startService(t, Deps{})
	msg, err := nc.Request(SubjectCamerasList, []byte{0xff, 0xff, 0xff, 0xff, 0xff}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Header.Get(micro.ErrorCodeHeader) != "400" {
		t.Fatalf("code = %s, want 400", msg.Header.Get(micro.ErrorCodeHeader))
	}
}
