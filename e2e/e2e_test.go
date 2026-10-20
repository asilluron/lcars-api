//go:build e2e

// Package e2e runs black-box tests against the compiled lcars-api binary and
// its Docker image. Run with:
//
//	go test -tags e2e -count=1 ./e2e/...
//
// The Docker tests are skipped when no Docker daemon is reachable, unless
// E2E_REQUIRE_DOCKER=1 is set (as in CI), in which case they fail instead.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const testVersion = "v0.0.0-e2e"

// binPath is the lcars-api binary built once in TestMain.
var binPath string

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	dir, err := os.MkdirTemp("", "lcars-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: temp dir:", err)
		return 1
	}
	defer os.RemoveAll(dir)

	binPath = filepath.Join(dir, "lcars-api")
	build := exec.Command("go", "build", "-trimpath",
		"-ldflags", "-X github.com/asilluron/lcars-api/internal/server.version="+testVersion,
		"-o", binPath, "./cmd/lcars-api")
	build.Dir = ".."
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: build binary:", err)
		return 1
	}
	return m.Run()
}

// --- binary tests ----------------------------------------------------------

func TestHealthz(t *testing.T) {
	p := startServer(t)

	resp := mustDo(t, http.MethodGet, p.url("/healthz"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(resp.body, &body); err != nil {
		t.Fatalf("decode %q: %v", resp.body, err)
	}
	want := map[string]string{"status": "ok", "version": testVersion}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("body[%q] = %q, want %q (body: %s)", k, body[k], v, resp.body)
		}
	}
}

func TestRouting(t *testing.T) {
	p := startServer(t)

	tests := []struct {
		method, path string
		wantStatus   int
		wantAllow    string
	}{
		{http.MethodGet, "/healthz", http.StatusOK, ""},
		{http.MethodHead, "/healthz", http.StatusOK, ""},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed, "GET, HEAD"},
		{http.MethodDelete, "/healthz", http.StatusMethodNotAllowed, "GET, HEAD"},
		{http.MethodGet, "/healthz/extra", http.StatusNotFound, ""},
		{http.MethodGet, "/", http.StatusNotFound, ""},
		{http.MethodPost, "/calendar", http.StatusMethodNotAllowed, "GET, HEAD"},
		{http.MethodGet, "/openapi.json", http.StatusOK, ""},
		{http.MethodPost, "/openapi.json", http.StatusMethodNotAllowed, "GET, HEAD"},
		{http.MethodGet, "/docs", http.StatusOK, ""},
		{http.MethodHead, "/docs", http.StatusOK, ""},
		{http.MethodPut, "/docs", http.StatusMethodNotAllowed, "GET, HEAD"},
		{http.MethodGet, "/docs/", http.StatusNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			resp := mustDo(t, tt.method, p.url(tt.path))
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if tt.wantAllow != "" && resp.Header.Get("Allow") != tt.wantAllow {
				t.Errorf("Allow = %q, want %q", resp.Header.Get("Allow"), tt.wantAllow)
			}
		})
	}
}

func TestAPIDocs(t *testing.T) {
	// Configure the calendar so every documented path can answer 200.
	p := startServer(t, "LCARS_CALENDAR_DIR="+fixturesDir(t))

	resp := mustDo(t, http.MethodGet, p.url("/openapi.json"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /openapi.json status = %d, want 200", resp.StatusCode)
	}
	var spec struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Version string `json:"version"`
		} `json:"info"`
		Paths map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(resp.body, &spec); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	if spec.OpenAPI != "3.1.0" {
		t.Errorf("openapi = %q, want 3.1.0", spec.OpenAPI)
	}
	if spec.Info.Version != testVersion {
		t.Errorf("info.version = %q, want %q", spec.Info.Version, testVersion)
	}
	// Every documented path must be served.
	for path := range spec.Paths {
		if r := mustDo(t, http.MethodGet, p.url(path)); r.StatusCode != http.StatusOK {
			t.Errorf("GET %s (documented) status = %d, want 200", path, r.StatusCode)
		}
	}

	resp = mustDo(t, http.MethodGet, p.url("/docs"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /docs status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET /docs Content-Type = %q, want text/html", ct)
	}
	if !bytes.Contains(resp.body, []byte(`Redoc.init("openapi.json"`)) {
		t.Errorf("GET /docs body does not load openapi.json:\n%s", resp.body)
	}
}

func TestConcurrentRequests(t *testing.T) {
	p := startServer(t)

	const n = 100
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Go(func() {
			resp, err := do(http.MethodGet, p.url("/healthz"))
			switch {
			case err != nil:
				errs <- err
			case resp.StatusCode != http.StatusOK:
				errs <- fmt.Errorf("status %d", resp.StatusCode)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestRequestLogging(t *testing.T) {
	p := startServer(t)
	mustDo(t, http.MethodGet, p.url("/healthz"))
	mustDo(t, http.MethodPost, p.url("/healthz"))

	for _, want := range []map[string]any{
		{"msg": "server listening", "level": "INFO", "version": testVersion},
		{"msg": "request", "method": "GET", "path": "/healthz", "status": float64(200)},
		{"msg": "request", "method": "POST", "path": "/healthz", "status": float64(405)},
	} {
		waitFor(t, 2*time.Second, fmt.Sprintf("log line matching %v", want), func() bool {
			return p.hasLog(want)
		})
	}
}

func TestTextLogFormat(t *testing.T) {
	p := startServer(t, "LCARS_LOG_FORMAT=text")
	waitFor(t, 2*time.Second, "text-format startup log", func() bool {
		return strings.Contains(p.stdout.String(), `msg="server listening"`)
	})
}

func TestDebugLogLevelAccepted(t *testing.T) {
	p := startServer(t, "LCARS_LOG_LEVEL=debug")
	mustDo(t, http.MethodGet, p.url("/healthz"))
}

func TestGracefulShutdown(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			p := startServer(t)
			mustDo(t, http.MethodGet, p.url("/healthz"))

			if err := p.cmd.Process.Signal(sig); err != nil {
				t.Fatalf("signal: %v", err)
			}
			if code := p.wait(t); code != 0 {
				t.Errorf("exit code = %d, want 0\nstderr: %s", code, p.stderr.String())
			}
			for _, msg := range []string{"shutting down", "server stopped"} {
				if !p.hasLog(map[string]any{"msg": msg}) {
					t.Errorf("missing %q log line\nstdout: %s", msg, p.stdout.String())
				}
			}
			if _, err := do(http.MethodGet, p.url("/healthz")); err == nil {
				t.Error("server still accepting connections after shutdown")
			}
		})
	}
}

func TestInvalidConfigExits(t *testing.T) {
	badFile := writeConfigFile(t, "[calendar]\ndirectory = \"/srv/calendars\"\n")
	tests := map[string]struct {
		env        string
		wantStderr string
	}{
		"log level":        {"LCARS_LOG_LEVEL=loud", "LCARS_LOG_LEVEL"},
		"log format":       {"LCARS_LOG_FORMAT=xml", "LCARS_LOG_FORMAT"},
		"shutdown timeout": {"LCARS_SHUTDOWN_TIMEOUT=soon", "LCARS_SHUTDOWN_TIMEOUT"},
		"timezone":         {"LCARS_TIMEZONE=Mars/Olympus_Mons", "LCARS_TIMEZONE"},
		"refresh interval": {"LCARS_CALENDAR_REFRESH_INTERVAL=1ms", "LCARS_CALENDAR_REFRESH_INTERVAL"},
		"missing cal dir":  {"LCARS_CALENDAR_DIR=" + filepath.Join(t.TempDir(), "missing"), "load calendar"},
		"missing config":   {"LCARS_CONFIG=" + filepath.Join(t.TempDir(), "lcars.toml"), "read config file"},
		"unknown key":      {"LCARS_CONFIG=" + badFile, `unknown setting "calendar.directory"`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			p := launch(t, tt.env)
			if code := p.wait(t); code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if !strings.Contains(p.stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", p.stderr.String(), tt.wantStderr)
			}
		})
	}
}

func TestPortInUseExits(t *testing.T) {
	first := startServer(t)

	second := launch(t, "LCARS_ADDR="+first.addr)
	if code := second.wait(t); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(second.stderr.String(), "address already in use") {
		t.Errorf("stderr = %q, want address-in-use error", second.stderr.String())
	}
	// The first server must be unaffected.
	mustDo(t, http.MethodGet, first.url("/healthz"))
}

// --- Docker tests ----------------------------------------------------------

func TestDockerImage(t *testing.T) {
	requireDocker(t)

	const image = "lcars-api:e2e"
	t.Log("building image (first build may take a minute)")
	docker(t, 5*time.Minute, "build", "--build-arg", "VERSION="+testVersion, "-t", image, "..")

	cfg := strings.TrimSpace(docker(t, 30*time.Second, "image", "inspect", "-f", "{{.Config.User}}", image))
	if cfg != "nonroot:nonroot" {
		t.Errorf("image user = %q, want nonroot:nonroot", cfg)
	}

	// Mount the fixture calendars and a config file at the image's default
	// config path. The config sets no addr, so the default :8080 is used.
	fixtures, err := filepath.Abs(calendarFixtures)
	if err != nil {
		t.Fatal(err)
	}
	config := writeConfigFile(t, "timezone = \"America/New_York\"\n\n[calendar]\ndir = \"/calendars\"\n")
	id := strings.TrimSpace(docker(t, 30*time.Second, "run", "-d", "-p", "127.0.0.1::8080",
		"-v", fixtures+":/calendars:ro",
		"-v", config+":/config/lcars.toml:ro",
		image))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })

	hostPort := strings.TrimSpace(docker(t, 30*time.Second, "port", id, "8080/tcp"))
	base := "http://" + strings.Split(hostPort, "\n")[0]
	waitReady(t, base+"/healthz")

	resp := mustDo(t, http.MethodGet, base+"/healthz")
	if resp.StatusCode != http.StatusOK || !bytes.Contains(resp.body, []byte(testVersion)) {
		t.Errorf("healthz = %d %s, want 200 with version %s", resp.StatusCode, resp.body, testVersion)
	}
	// The calendar must work in the minimal image: config read from the
	// working directory, the mounted directory readable by the nonroot user,
	// and timezone data available without system zoneinfo files.
	checkFixtureWeek(t, base)

	// The spec and docs page are embedded in the binary, so they must be
	// served from the image without any files next to it.
	for _, path := range []string{"/openapi.json", "/docs"} {
		if r := mustDo(t, http.MethodGet, base+path); r.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, r.StatusCode)
		}
	}

	// docker stop sends SIGTERM; the server must drain and exit 0, not be
	// SIGKILLed after the timeout (exit 137).
	docker(t, 30*time.Second, "stop", "-t", "10", id)
	code := strings.TrimSpace(docker(t, 30*time.Second, "inspect", "-f", "{{.State.ExitCode}}", id))
	if code != "0" {
		t.Errorf("container exit code = %s, want 0", code)
	}
	logs := docker(t, 30*time.Second, "logs", id)
	if !strings.Contains(logs, `"msg":"server stopped"`) {
		t.Errorf("container logs missing clean shutdown:\n%s", logs)
	}
}

func requireDocker(t *testing.T) {
	t.Helper()
	err := exec.Command("docker", "info").Run()
	if err == nil {
		return
	}
	if os.Getenv("E2E_REQUIRE_DOCKER") == "1" {
		t.Fatalf("docker required but unavailable: %v", err)
	}
	t.Skipf("docker unavailable (%v); set E2E_REQUIRE_DOCKER=1 to fail instead", err)
}

func docker(t *testing.T, timeout time.Duration, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("docker %s: %v\n%s%s", strings.Join(args, " "), err, out.String(), errOut.String())
	}
	return out.String() + errOut.String()
}

// --- helpers ---------------------------------------------------------------

// server is a running lcars-api subprocess.
type server struct {
	cmd    *exec.Cmd
	addr   string
	stdout *syncBuffer
	stderr *syncBuffer

	done     chan struct{}
	exitCode int
}

// launch starts the binary on a free port without waiting for it to be ready.
// Later env entries override earlier ones, so callers can replace LCARS_ADDR.
func launch(t *testing.T, env ...string) *server {
	t.Helper()
	return launchIn(t, "", env...)
}

// launchIn is launch with the given working directory ("" for the test's).
func launchIn(t *testing.T, dir string, env ...string) *server {
	t.Helper()
	p := &server{
		addr:   freeAddr(t),
		stdout: &syncBuffer{},
		stderr: &syncBuffer{},
		done:   make(chan struct{}),
	}
	p.cmd = exec.Command(binPath)
	p.cmd.Dir = dir
	p.cmd.Env = append([]string{"LCARS_ADDR=" + p.addr}, env...)
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "LCARS_ADDR="); ok {
			p.addr = v
		}
	}
	p.cmd.Stdout, p.cmd.Stderr = p.stdout, p.stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start binary: %v", err)
	}
	go func() {
		err := p.cmd.Wait()
		p.exitCode = 0
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			p.exitCode = exitErr.ExitCode()
		} else if err != nil {
			p.exitCode = -1
		}
		close(p.done)
	}()
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			_ = p.cmd.Process.Kill()
			<-p.done
		}
		if t.Failed() {
			t.Logf("server stdout:\n%s\nserver stderr:\n%s", p.stdout.String(), p.stderr.String())
		}
	})
	return p
}

// startServer launches the binary and waits until /healthz answers.
func startServer(t *testing.T, env ...string) *server {
	t.Helper()
	return startServerIn(t, "", env...)
}

// startServerIn is startServer with the given working directory.
func startServerIn(t *testing.T, dir string, env ...string) *server {
	t.Helper()
	p := launchIn(t, dir, env...)
	waitReady(t, p.url("/healthz"))
	return p
}

func (p *server) url(path string) string { return "http://" + p.addr + path }

// wait blocks until the process exits and returns its exit code, failing the
// test if that takes more than 5 seconds.
func (p *server) wait(t *testing.T) int {
	const timeout = 5 * time.Second
	t.Helper()
	select {
	case <-p.done:
		return p.exitCode
	case <-time.After(timeout):
		t.Fatalf("process did not exit within %s", timeout)
		return -1
	}
}

// hasLog reports whether any JSON log line contains all the given fields.
func (p *server) hasLog(want map[string]any) bool {
	for line := range strings.Lines(p.stdout.String()) {
		var got map[string]any
		if json.Unmarshal([]byte(line), &got) != nil {
			continue
		}
		match := true
		for k, v := range want {
			if got[k] != v {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

type response struct {
	*http.Response
	body []byte
}

var client = &http.Client{Timeout: 5 * time.Second}

func do(method, url string) (*response, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &response{Response: resp, body: body}, nil
}

func mustDo(t *testing.T, method, url string) *response {
	t.Helper()
	resp, err := do(method, url)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func waitReady(t *testing.T, url string) {
	t.Helper()
	waitFor(t, 10*time.Second, url+" to return 200", func() bool {
		resp, err := do(http.MethodGet, url)
		return err == nil && resp.StatusCode == http.StatusOK
	})
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().String()
}

// syncBuffer is a bytes.Buffer safe for concurrent writes from a subprocess
// and reads from the test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
