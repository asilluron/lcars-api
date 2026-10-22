// Package training stores microphone audio for wake-word training.
//
// lcars.training.put appends 16 kHz mono PCM to an hourly WAV file inside the
// directory the operator configured. The file name is chosen here. The
// screen id from the request is only a label, and only after it has been
// reduced to a short token.
package training

import (
	"cmp"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"sync"
	"time"
)

const (
	// SampleRate is the PCM rate every stored file uses.
	SampleRate = 16_000
	// MaxPCM is the most audio one request may carry.
	MaxPCM = 256 << 10
	// wavHeaderBytes is the fixed 44-byte PCM header this package writes.
	wavHeaderBytes = 44
)

// hourlyName matches the files this package writes, and no others. The size
// cap deletes only these, so a note left in the directory is left alone.
var hourlyName = regexp.MustCompile(`^[0-9]{8}T[0-9]{2}_[A-Za-z0-9_-]{1,32}\.wav$`)

// screenToken matches a screen id that is safe to put in a file name.
var screenToken = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// Writer appends PCM to hourly WAV files under a single directory. It is
// safe for concurrent use.
type Writer struct {
	root     *os.Root
	maxBytes int64
	logger   *slog.Logger
	mu       sync.Mutex
}

// Open opens dir with os.Root and returns a Writer that keeps the tree at or
// under maxBytes. dir must already exist.
func Open(dir string, maxBytes int64, logger *slog.Logger) (*Writer, error) {
	if maxBytes < 1 {
		return nil, fmt.Errorf("training max_bytes must be at least 1, got %d", maxBytes)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Writer{root: root, maxBytes: maxBytes, logger: logger}, nil
}

// Close releases the directory.
func (w *Writer) Close() error {
	return w.root.Close()
}

// Write appends pcm, which must be even-length 16-bit little-endian mono, to
// the hourly file for now. The file name uses the server clock and a
// sanitized screen id. Write reports how many bytes were appended.
func (w *Writer) Write(screen string, pcm []byte, now time.Time) (int, error) {
	if len(pcm) == 0 || len(pcm)%2 != 0 {
		return 0, fmt.Errorf("pcm must be a non-empty even number of bytes, got %d", len(pcm))
	}
	name := now.Format("20060102T15") + "_" + ScreenName(screen) + ".wav"

	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.append(name, pcm); err != nil {
		return 0, err
	}
	w.logger.Info("training audio stored", "file", name, "bytes", len(pcm))
	if err := w.enforce(); err != nil {
		return 0, err
	}
	return len(pcm), nil
}

// ScreenName returns id when it is a short token, and "device" otherwise.
// The result is always safe to use as a single path segment.
func ScreenName(id string) string {
	if screenToken.MatchString(id) {
		return id
	}
	return "device"
}

func (w *Writer) append(name string, pcm []byte) error {
	f, err := w.root.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open training file: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat training file: %w", err)
	}
	size := info.Size()
	if size == 0 {
		if _, err := f.Write(wavHeader(len(pcm))); err != nil {
			return fmt.Errorf("write training header: %w", err)
		}
		if _, err := f.Write(pcm); err != nil {
			return fmt.Errorf("write training audio: %w", err)
		}
		return nil
	}
	header := make([]byte, wavHeaderBytes)
	if _, err := io.ReadFull(f, header); err != nil {
		return fmt.Errorf("read training header: %w", err)
	}
	if !validHeader(header) {
		return fmt.Errorf("training file %s is not 16 kHz mono PCM", name)
	}
	if _, err := f.Seek(size, io.SeekStart); err != nil {
		return fmt.Errorf("seek training file: %w", err)
	}
	if _, err := f.Write(pcm); err != nil {
		return fmt.Errorf("write training audio: %w", err)
	}
	dataBytes := size + int64(len(pcm)) - wavHeaderBytes
	if dataBytes < 0 || dataBytes > int64(^uint32(0)) {
		return fmt.Errorf("training file %s is too large", name)
	}
	binary.LittleEndian.PutUint32(header[4:8], uint32(36+dataBytes))
	binary.LittleEndian.PutUint32(header[40:44], uint32(dataBytes))
	if _, err := f.WriteAt(header, 0); err != nil {
		return fmt.Errorf("update training header: %w", err)
	}
	return nil
}

// enforce deletes the oldest hourly files until the directory is within the cap.
func (w *Writer) enforce() error {
	entries, err := fs.ReadDir(w.root.FS(), ".")
	if err != nil {
		return fmt.Errorf("read training directory: %w", err)
	}
	type file struct {
		name string
		size int64
	}
	var files []file
	var total int64
	for _, entry := range entries {
		if entry.IsDir() || !hourlyName.MatchString(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat training file: %w", err)
		}
		files = append(files, file{name: entry.Name(), size: info.Size()})
		total += info.Size()
	}
	slices.SortFunc(files, func(a, b file) int { return cmp.Compare(a.name, b.name) })
	for total > w.maxBytes && len(files) > 0 {
		oldest := files[0]
		files = files[1:]
		if err := w.root.Remove(oldest.name); err != nil {
			return fmt.Errorf("remove training file: %w", err)
		}
		total -= oldest.size
		w.logger.Info("training audio removed", "file", oldest.name, "bytes", oldest.size)
	}
	return nil
}

func wavHeader(dataBytes int) []byte {
	header := make([]byte, wavHeaderBytes)
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], uint32(36+dataBytes))
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(header[22:24], 1) // mono
	binary.LittleEndian.PutUint32(header[24:28], SampleRate)
	binary.LittleEndian.PutUint32(header[28:32], SampleRate*2)
	binary.LittleEndian.PutUint16(header[32:34], 2) // block align
	binary.LittleEndian.PutUint16(header[34:36], 16)
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], uint32(dataBytes))
	return header
}

func validHeader(header []byte) bool {
	if len(header) < wavHeaderBytes {
		return false
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		return false
	}
	if string(header[12:16]) != "fmt " || string(header[36:40]) != "data" {
		return false
	}
	if binary.LittleEndian.Uint16(header[20:22]) != 1 || binary.LittleEndian.Uint16(header[22:24]) != 1 {
		return false
	}
	if binary.LittleEndian.Uint32(header[24:28]) != SampleRate || binary.LittleEndian.Uint16(header[34:36]) != 16 {
		return false
	}
	return true
}
