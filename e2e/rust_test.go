//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRustClient runs the Rust example client (rust/examples/agenda.rs) against
// the Docker image, over NATS on a Docker network. It checks that the
// generated Rust (prost) types decode what the server sends, that error
// replies are recognized, and that the client hears about calendar changes.
func TestRustClient(t *testing.T) {
	requireDocker(t)
	buildImage(t)

	network := "lcars-e2e-" + strconv.Itoa(os.Getpid())
	docker(t, "network", "create", network)
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", network).Run() })

	// A writable copy of the fixtures, so the test can add a file. The mounted
	// directory must be readable by the image's nonroot user.
	dir := filepath.Join(t.TempDir(), "calendars")
	copyDir(t, fixturesDir(t), dir)

	server := strings.TrimSpace(docker(t, "run", "-d",
		"--network", network, "--network-alias", "lcars",
		"-p", "127.0.0.1::8080",
		"-v", dir+":/calendars:ro",
		"-e", "LCARS_CALENDAR_DIR=/calendars",
		"-e", "LCARS_CALENDAR_REFRESH_INTERVAL=1s",
		"-e", "LCARS_TIMEZONE=America/New_York",
		image))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", server).Run() })
	waitReady(t, "http://"+dockerPort(t, server, "8080/tcp")+"/healthz")

	rustDir, err := filepath.Abs("../rust")
	if err != nil {
		t.Fatal(err)
	}
	// The same image and cache volumes as `make rust`, so builds are shared.
	rustArgs := []string{
		"--network", network,
		"-v", rustDir + ":/src:ro",
		"-v", "lcars-cargo-registry:/usr/local/cargo/registry",
		"-v", "lcars-cargo-target:/target",
		"-e", "CARGO_TARGET_DIR=/target",
		"-w", "/src",
		makeVar(t, "RUST"),
	}
	run := func(timeout time.Duration, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(t.Context(), timeout)
		defer cancel()
		var out bytes.Buffer
		cmd := exec.CommandContext(ctx, "docker", append(append([]string{"run", "--rm"}, rustArgs...), args...)...)
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		return out.String(), err
	}

	t.Log("building the Rust example client (the first build may take a few minutes)")
	if out, err := run(15*time.Minute, "cargo", "build", "--locked", "--example", "agenda"); err != nil {
		t.Fatalf("cargo build: %v\n%s", err, out)
	}
	const (
		agenda = "/target/debug/examples/agenda"
		url    = "nats://lcars:4222"
	)

	t.Run("calendar", func(t *testing.T) {
		out, err := run(time.Minute, agenda, url, "2030-03-04", "7")
		if err != nil {
			t.Fatalf("agenda: %v\n%s", err, out)
		}
		if got := strings.TrimSpace(out); got != strings.Join(fixtureWeek, "\n") {
			t.Errorf("Rust client output:\n%s\nwant:\n%s", got, strings.Join(fixtureWeek, "\n"))
		}
	})

	t.Run("error reply", func(t *testing.T) {
		out, err := run(time.Minute, agenda, url, "2030-02-30", "1")
		if err == nil {
			t.Fatalf("agenda with an invalid date succeeded:\n%s", out)
		}
		if !strings.Contains(out, "server returned 400") || !strings.Contains(out, `invalid "from"`) {
			t.Errorf("output = %q, want the server's 400 error", out)
		}
	})

	t.Run("watch", func(t *testing.T) {
		id := strings.TrimSpace(docker(t,
			append(append([]string{"run", "-d"}, rustArgs...), agenda, url, "2030-03-04", "1", "--watch")...))
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
		logs := func() string { return docker(t, "logs", id) }

		waitFor(t, 30*time.Second, "the client to print the calendar", func() bool {
			return strings.Contains(logs(), fixtureWeek[0])
		})
		writeICS(t, filepath.Join(dir, "new.ics"), "Added")
		want := fixtureWeek[0] + ", new/Added 12:00-13:00"
		waitFor(t, 30*time.Second, "the client to print the updated calendar", func() bool {
			l := logs()
			return strings.Contains(l, "-- updated at ") && strings.Contains(l, want)
		})
	})
}

// makeVar returns the value of a "NAME := value" variable in the Makefile.
func makeVar(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\s*:=\s*(\S+)`).FindSubmatch(data)
	if m == nil {
		t.Fatalf("Makefile has no %s variable", name)
	}
	return string(m[1])
}

// copyDir copies the files under src to dst, world-readable.
func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			return os.Chmod(target, 0o755) // regardless of umask
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}
