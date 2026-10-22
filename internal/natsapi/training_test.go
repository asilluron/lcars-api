package natsapi

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/micro"
	"google.golang.org/protobuf/proto"

	lcarsv1 "github.com/asilluron/lcars-api/gen/go/lcars/v1"
	"github.com/asilluron/lcars-api/internal/training"
)

func TestTrainingPut(t *testing.T) {
	dir := t.TempDir()
	w, err := training.Open(dir, 32<<30, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })

	nc := startService(t, Deps{Training: w})
	pcm := []byte{0x00, 0x00, 0x02, 0x00}
	msg, err := nc.Request(SubjectTrainingPut, mustMarshal(t, &lcarsv1.PutTrainingAudioRequest{
		ScreenId: "kitchen",
		Pcm:      pcm,
	}), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if code := msg.Header.Get(micro.ErrorCodeHeader); code != "" {
		t.Fatalf("error %s: %s", code, msg.Header.Get(micro.ErrorHeader))
	}
	var resp lcarsv1.PutTrainingAudioResponse
	if err := proto.Unmarshal(msg.Data, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.GetAcceptedBytes() != uint32(len(pcm)) {
		t.Fatalf("accepted_bytes = %d", resp.GetAcceptedBytes())
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*_kitchen.wav"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("files = %v, %v", matches, err)
	}
}

func TestTrainingNotConfigured(t *testing.T) {
	nc := startService(t, Deps{})
	msg, err := nc.Request(SubjectTrainingPut, mustMarshal(t, &lcarsv1.PutTrainingAudioRequest{
		Pcm: []byte{0x00, 0x00},
	}), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if code := msg.Header.Get(micro.ErrorCodeHeader); code != "503" {
		t.Fatalf("code = %q, want 503", code)
	}
	if m := msg.Header.Get(micro.ErrorHeader); !strings.Contains(m, "training.dir") {
		t.Fatalf("message = %q", m)
	}
}
