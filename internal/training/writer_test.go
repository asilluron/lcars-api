package training

import (
	"encoding/binary"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestScreenName(t *testing.T) {
	for _, id := range []string{"kitchen", "dev", "a", "screen_1", "A-1", "abcdefghijklmnopqrstuvwxyz012345"} {
		if got := ScreenName(id); got != id {
			t.Errorf("ScreenName(%q) = %q", id, got)
		}
	}
	for _, id := range []string{"", "../escape", "a/b", "has space", "café", "abcdefghijklmnopqrstuvwxyz0123456", ".."} {
		if got := ScreenName(id); got != "device" {
			t.Errorf("ScreenName(%q) = %q, want device", id, got)
		}
	}
}

func TestWriteAppendAndScreen(t *testing.T) {
	dir := t.TempDir()
	w := openWriter(t, dir, 32<<30)

	hour := time.Date(2030, 3, 4, 15, 0, 0, 0, time.UTC)
	pcm := []byte{0x00, 0x00, 0x01, 0x00} // samples 0, 1
	if n, err := w.Write("kitchen", pcm, hour); err != nil || n != len(pcm) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if n, err := w.Write("kitchen", []byte{0xff, 0xff}, hour); err != nil || n != 2 {
		t.Fatalf("append = %d, %v", n, err)
	}
	// A path-like screen id stays inside the directory, under "device".
	if _, err := w.Write("../escape", pcm, hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "escape.wav")); !os.IsNotExist(err) {
		t.Fatalf("path escaped the training directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.wav")); !os.IsNotExist(err) {
		t.Fatal("wrote outside the temp directory")
	}

	matches := wavs(t, dir)
	if len(matches) != 2 {
		t.Fatalf("files = %v", matches)
	}
	got := readSamples(t, filepath.Join(dir, "20300304T15_kitchen.wav"))
	if !equalSamples(got, []int16{0, 1, -1}) {
		t.Fatalf("samples = %v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "20300304T15_device.wav")); err != nil {
		t.Fatal(err)
	}
}

func TestWriteCapKeepsNewestHour(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	// One chunk is a 48-byte file. The cap fits exactly one of them.
	const chunk = wavHeaderBytes + 4
	w := openWriter(t, dir, chunk)
	hour := time.Date(2030, 3, 4, 15, 0, 0, 0, time.UTC)
	pcm := []byte{0x00, 0x00, 0x01, 0x00}
	if _, err := w.Write("kitchen", pcm, hour); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write("kitchen", pcm, hour.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	left := wavs(t, dir)
	if len(left) != 1 || filepath.Base(left[0]) != "20300304T16_kitchen.wav" {
		t.Fatalf("after cap, files = %v", left)
	}
	note, err := os.ReadFile(filepath.Join(dir, "notes.txt"))
	if err != nil || string(note) != "keep" {
		t.Fatalf("notes.txt = %q, %v", note, err)
	}
}

func TestWriteRejectsBadPCM(t *testing.T) {
	w := openWriter(t, t.TempDir(), 32<<30)
	now := time.Date(2030, 3, 4, 15, 0, 0, 0, time.UTC)
	for _, pcm := range [][]byte{nil, {}, {0x00}} {
		if _, err := w.Write("kitchen", pcm, now); err == nil {
			t.Fatalf("Write(%v) succeeded", pcm)
		}
	}
}

func TestOpenMissingDir(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "missing"), 1, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("Open of a missing directory succeeded")
	}
}

func openWriter(t *testing.T, dir string, maxBytes int64) *Writer {
	t.Helper()
	w, err := Open(dir, maxBytes, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func wavs(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.wav"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func readSamples(t *testing.T, path string) []int16 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < wavHeaderBytes || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		t.Fatalf("%s is not a wav file (%d bytes)", path, len(data))
	}
	dataBytes := binary.LittleEndian.Uint32(data[40:44])
	if int(dataBytes) != len(data)-wavHeaderBytes {
		t.Fatalf("data size %d, file has %d", dataBytes, len(data)-wavHeaderBytes)
	}
	pcm := data[wavHeaderBytes:]
	out := make([]int16, len(pcm)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(pcm[i*2:]))
	}
	return out
}

func equalSamples(got, want []int16) bool {
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
