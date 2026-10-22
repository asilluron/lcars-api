//go:build e2e

package e2e

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats.go/micro"
	"google.golang.org/protobuf/proto"

	lcarsv1 "github.com/asilluron/lcars-api/gen/go/lcars/v1"
)

func TestNATSTrainingAudio(t *testing.T) {
	dir := t.TempDir()
	p := startServer(t,
		"LCARS_TRAINING_DIR="+dir,
		"LCARS_TRAINING_MAX_BYTES=34359738368",
	)
	nc := connectNATS(t, p.natsURL())

	pcm := []byte{0x00, 0x00, 0x01, 0x00, 0xff, 0xff} // samples 0, 1, -1
	msg := natsRequest(t, nc, subjectTrainingPut, &lcarsv1.PutTrainingAudioRequest{
		ScreenId: "kitchen",
		Pcm:      pcm,
	})
	if err := replyError(msg); err != nil {
		t.Fatal(err)
	}
	var resp lcarsv1.PutTrainingAudioResponse
	if err := proto.Unmarshal(msg.Data, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.GetAcceptedBytes() != uint32(len(pcm)) {
		t.Fatalf("accepted_bytes = %d, want %d", resp.GetAcceptedBytes(), len(pcm))
	}

	// A second chunk appends, and a path-like screen id cannot leave the directory.
	msg = natsRequest(t, nc, subjectTrainingPut, &lcarsv1.PutTrainingAudioRequest{
		ScreenId: "../escape",
		Pcm:      pcm,
	})
	if err := replyError(msg); err != nil {
		t.Fatal(err)
	}

	kitchen := filepath.Join(dir, wavName(t, dir, "_kitchen.wav"))
	if got := readWavSamples(t, kitchen); !samplesEqual(got, []int16{0, 1, -1}) {
		t.Fatalf("kitchen samples = %v", got)
	}
	device := wavName(t, dir, "_device.wav")
	if _, err := os.Stat(filepath.Join(dir, device)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("training directory = %v, want the kitchen and device files only", entries)
	}
}

func TestNATSTrainingErrors(t *testing.T) {
	p := startServer(t)
	nc := connectNATS(t, p.natsURL())

	msg := natsRequest(t, nc, subjectTrainingPut, &lcarsv1.PutTrainingAudioRequest{})
	if code := msg.Header.Get(micro.ErrorCodeHeader); code != "503" {
		t.Fatalf("unset directory: code = %q, want 503 (%s)", code, msg.Header.Get(micro.ErrorHeader))
	}

	dir := t.TempDir()
	p = startServer(t, "LCARS_TRAINING_DIR="+dir)
	nc = connectNATS(t, p.natsURL())
	for name, body := range map[string][]byte{
		"empty":        mustMarshal(t, &lcarsv1.PutTrainingAudioRequest{ScreenId: "kitchen"}),
		"odd":          mustMarshal(t, &lcarsv1.PutTrainingAudioRequest{Pcm: []byte{0x00}}),
		"too big":      mustMarshal(t, &lcarsv1.PutTrainingAudioRequest{Pcm: make([]byte, 256<<10+2)}),
		"not protobuf": []byte(`{"pcm":"aGk="}`),
	} {
		t.Run(name, func(t *testing.T) {
			cover(subjectCoverage, subjectTrainingPut)
			msg, err := nc.Request(subjectTrainingPut, body, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if code := msg.Header.Get(micro.ErrorCodeHeader); code != "400" {
				t.Errorf("code = %q, want 400 (%s)", code, msg.Header.Get(micro.ErrorHeader))
			}
			if len(msg.Data) != 0 {
				t.Errorf("error reply has a body: %q", msg.Data)
			}
		})
	}
}

func wavName(t *testing.T, dir, suffix string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*"+suffix))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("files matching %s: %v", suffix, matches)
	}
	return filepath.Base(matches[0])
}

func readWavSamples(t *testing.T, path string) []int16 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 44 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		t.Fatalf("%s is not a wav file", path)
	}
	if rate := binary.LittleEndian.Uint32(data[24:28]); rate != 16000 {
		t.Fatalf("sample rate = %d, want 16000", rate)
	}
	n := binary.LittleEndian.Uint32(data[40:44])
	pcm := data[44:]
	if int(n) != len(pcm) {
		t.Fatalf("data size %d, file has %d", n, len(pcm))
	}
	out := make([]int16, len(pcm)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(pcm[i*2:]))
	}
	return out
}

func samplesEqual(got, want []int16) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
