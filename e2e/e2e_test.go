//go:build e2e

// Package e2e runs black-box tests against the compiled lcars-api binary and
// its Docker image. Run with:
//
//	go test -tags e2e -count=1 ./e2e/...
//
// The Docker tests (the image, and the Rust client against it) are skipped
// when no Docker daemon is reachable, unless E2E_REQUIRE_DOCKER=1 is set (as
// in CI), in which case they fail instead.
//
// Every HTTP path, NATS subject and setting must be exercised by at least one
// test here: a full run fails if any is not (see coverage.go).
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
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
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

	// The admin page is embedded in the binary, so it must be built first.
	if _, err := os.Stat("../internal/adminui/dist/index.html"); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: the admin page is not built: run `make admin-ui` (or `make e2e`)")
		return 1
	}

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
	if code := m.Run(); code != 0 {
		return code
	}
	return checkCoverage()
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
	want := map[string]string{"status": "ok", "version": testVersion, "nats": "connected"}
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
		{http.MethodGet, "/admin", http.StatusOK, ""}, // redirected to /admin/
		{http.MethodGet, "/admin/", http.StatusOK, ""},
		{http.MethodPost, "/admin/", http.StatusMethodNotAllowed, "GET, HEAD"},
		{http.MethodGet, "/admin/assets/", http.StatusNotFound, ""},
		{http.MethodGet, "/admin/no-such-file.js", http.StatusNotFound, ""},
		{http.MethodGet, "/admin/api/session", http.StatusNotFound, ""},
		{http.MethodPut, "/admin/api/tokens", http.StatusMethodNotAllowed, ""},
		{http.MethodGet, "/admin/api/tokens/dev_000000000000", http.StatusNotFound, ""},
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
	// Configure the calendar and a camera proxy so every documented path can
	// answer 200. The proxy is a stand-in; it never talks to Eufy.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cameras":
			_, _ = io.WriteString(w, `[{"sn":"CAM1","name":"Front door","camera":true}]`)
		case "/stream/CAM1":
			_, _ = io.WriteString(w, "x")
		case "/snapshot/CAM1":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte{0xff, 0xd8, 0xff, 0xd9})
		case "/v1/forecast":
			_, _ = io.WriteString(w, `{"current":{"temperature_2m":72.2,"apparent_temperature":70.1,"weather_code":0,"wind_speed_10m":4.2},"daily":{"temperature_2m_max":[78.4],"temperature_2m_min":[55.2]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	p := startServer(t,
		"LCARS_CALENDAR_DIR="+fixturesDir(t),
		"LCARS_EUFY_URL="+upstream.URL,
		"LCARS_WEATHER_LATITUDE=0",
		"LCARS_WEATHER_LONGITUDE=0",
		"LCARS_WEATHER_URL="+upstream.URL,
	)

	resp := mustDo(t, http.MethodGet, p.url("/openapi.json"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /openapi.json status = %d, want 200", resp.StatusCode)
	}
	var spec struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Version string `json:"version"`
		} `json:"info"`
		Paths map[string]map[string]struct {
			Security []map[string]any `json:"security"`
		} `json:"paths"`
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
	// Every documented GET that needs no credentials (with authentication
	// off, as here) must be served.
	for path, ops := range spec.Paths {
		get, ok := ops["get"]
		if !ok || (len(get.Security) > 0 && !slices.ContainsFunc(get.Security, func(s map[string]any) bool { return len(s) == 0 })) {
			continue
		}
		// Path templates are requested with a stand-in serial. The OpenAPI
		// path itself is not a serial the proxy would accept.
		reqPath := strings.ReplaceAll(path, "{sn}", "CAM1")
		if r := mustDo(t, http.MethodGet, p.url(reqPath)); r.StatusCode != http.StatusOK {
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
	p := startServer(t, "LCARS_CALENDAR_DIR="+fixturesDir(t), "LCARS_TIMEZONE=America/New_York")
	// Several NATS clients, like several displays.
	conns := []*nats.Conn{connectNATS(t, p.natsURL()), connectNATS(t, p.natsURL()), connectNATS(t, p.natsURL())}

	const n = 100
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for i := range n {
		wg.Go(func() {
			resp, err := do(http.MethodGet, p.url("/healthz"))
			switch {
			case err != nil:
				errs <- err
			case resp.StatusCode != http.StatusOK:
				errs <- fmt.Errorf("status %d", resp.StatusCode)
			}
		})
		wg.Go(func() {
			cal, err := requestCalendar(conns[i%len(conns)], "2030-03-04", 7)
			if err != nil {
				errs <- err
			} else if got := renderNATS(cal); strings.Join(got, "\n") != strings.Join(fixtureWeek, "\n") {
				errs <- fmt.Errorf("NATS calendar = %q", got)
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
	nc := connectNATS(t, p.natsURL())
	natsRequest(t, nc, subjectCalendarGet, nil) // 503: no calendar configured

	for _, want := range []map[string]any{
		{"msg": "http server listening", "level": "INFO", "version": testVersion, "addr": p.addr},
		{"msg": "nats server listening", "level": "INFO", "addr": p.natsAddr},
		{"msg": "request", "method": "GET", "path": "/healthz", "status": float64(200)},
		{"msg": "request", "method": "POST", "path": "/healthz", "status": float64(405)},
		{"msg": "nats request", "subject": subjectCalendarGet, "status": float64(503)},
	} {
		waitFor(t, 2*time.Second, fmt.Sprintf("log line matching %v", want), func() bool {
			return p.hasLog(want)
		})
	}
}

func TestTextLogFormat(t *testing.T) {
	p := startServer(t, "LCARS_LOG_FORMAT=text")
	waitFor(t, 2*time.Second, "text-format startup log", func() bool {
		return strings.Contains(p.stdout.String(), `msg="http server listening"`)
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
			nc := connectNATS(t, p.natsURL())

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
			// The embedded NATS server is gone too.
			if conn, err := net.DialTimeout("tcp", p.natsAddr, time.Second); err == nil {
				conn.Close()
				t.Error("NATS server still accepting connections after shutdown")
			}
			if nc.IsConnected() {
				t.Error("NATS client still connected after shutdown")
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
		"log level":         {"LCARS_LOG_LEVEL=loud", "LCARS_LOG_LEVEL"},
		"log format":        {"LCARS_LOG_FORMAT=xml", "LCARS_LOG_FORMAT"},
		"shutdown timeout":  {"LCARS_SHUTDOWN_TIMEOUT=soon", "LCARS_SHUTDOWN_TIMEOUT"},
		"timezone":          {"LCARS_TIMEZONE=Mars/Olympus_Mons", "LCARS_TIMEZONE"},
		"refresh interval":  {"LCARS_CALENDAR_REFRESH_INTERVAL=1ms", "LCARS_CALENDAR_REFRESH_INTERVAL"},
		"missing cal dir":   {"LCARS_CALENDAR_DIR=" + filepath.Join(t.TempDir(), "missing"), "load calendar"},
		"missing train dir": {"LCARS_TRAINING_DIR=" + filepath.Join(t.TempDir(), "missing"), "open training directory"},
		"training max":      {"LCARS_TRAINING_MAX_BYTES=0", "LCARS_TRAINING_MAX_BYTES"},
		"nats addr":         {"LCARS_NATS_ADDR=localhost", "LCARS_NATS_ADDR"},
		"nats unreachable":  {"LCARS_NATS_URL=nats://" + freeAddr(t), "connect to NATS"},
		"missing config":    {"LCARS_CONFIG=" + filepath.Join(t.TempDir(), "lcars.toml"), "read config file"},
		"unknown key":       {"LCARS_CONFIG=" + badFile, `unknown setting "calendar.directory"`},
		"supabase url":      {"LCARS_SUPABASE_URL=127.0.0.1:54321", "LCARS_SUPABASE_URL"},
		"supabase no key":   {"LCARS_SUPABASE_URL=http://127.0.0.1:54321", "must be set together"},
		"supabase refresh":  {"LCARS_SUPABASE_REFRESH_INTERVAL=1ms", "LCARS_SUPABASE_REFRESH_INTERVAL"},
		"eufy url":          {"LCARS_EUFY_URL=not-a-url", "LCARS_EUFY_URL"},
		"eufy path":         {"LCARS_EUFY_URL=http://127.0.0.1:3000/cameras", "LCARS_EUFY_URL"},
		"eufy userinfo":     {"LCARS_EUFY_URL=http://user:replace-me@127.0.0.1:3000", "LCARS_EUFY_URL"},
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
	for name, env := range map[string]string{
		"http": "LCARS_ADDR=" + first.addr,
		"nats": "LCARS_NATS_ADDR=" + first.natsAddr,
	} {
		t.Run(name, func(t *testing.T) {
			second := launch(t, env)
			if code := second.wait(t); code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if !strings.Contains(second.stderr.String(), "address already in use") {
				t.Errorf("stderr = %q, want address-in-use error", second.stderr.String())
			}
		})
	}
	// The first server must be unaffected.
	mustDo(t, http.MethodGet, first.url("/healthz"))
	if _, err := requestCalendar(connectNATS(t, first.natsURL()), "", 0); err == nil ||
		!strings.Contains(err.Error(), "503") {
		t.Errorf("NATS request to first server = %v, want a 503 (no calendar)", err)
	}
}

// --- Docker tests ----------------------------------------------------------

// image is the Docker image built by buildImage.
const image = "lcars-api:e2e"

var buildImageOnce = sync.OnceValue(func() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "build",
		"--build-arg", "VERSION="+testVersion, "-t", image, "..").CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker build: %w\n%s", err, out)
	}
	return nil
})

// buildImage builds the image once per test run.
func buildImage(t *testing.T) {
	t.Helper()
	t.Log("building image (first build may take a minute)")
	if err := buildImageOnce(); err != nil {
		t.Fatal(err)
	}
}

func TestDockerImage(t *testing.T) {
	requireDocker(t)
	buildImage(t)

	cfg := strings.TrimSpace(docker(t, "image", "inspect", "-f", "{{.Config.User}}", image))
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
	id := strings.TrimSpace(docker(t, "run", "-d",
		"-p", "127.0.0.1::8080", "-p", "127.0.0.1::4222",
		"-v", fixtures+":/calendars:ro",
		"-v", config+":/config/lcars.toml:ro",
		image))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })

	base := "http://" + dockerPort(t, id, "8080/tcp")
	waitReady(t, base+"/healthz")

	resp := mustDo(t, http.MethodGet, base+"/healthz")
	if resp.StatusCode != http.StatusOK || !bytes.Contains(resp.body, []byte(testVersion)) ||
		!bytes.Contains(resp.body, []byte(`"nats":"connected"`)) {
		t.Errorf("healthz = %d %s, want 200 with version %s and NATS connected", resp.StatusCode, resp.body, testVersion)
	}
	// The calendar must work in the minimal image, over HTTP and over the
	// embedded NATS server on the default port: config read from the working
	// directory, the mounted directory readable by the nonroot user, and
	// timezone data available without system zoneinfo files.
	checkFixtureWeek(t, base)
	checkFixtureWeekNATS(t, "nats://"+dockerPort(t, id, "4222/tcp"))

	// The spec, docs and admin pages are embedded in the binary, so they must
	// be served from the image without any files next to it.
	for _, path := range []string{"/openapi.json", "/docs", "/admin/"} {
		if r := mustDo(t, http.MethodGet, base+path); r.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, r.StatusCode)
		}
	}

	// docker stop sends SIGTERM; the server must drain and exit 0, not be
	// SIGKILLed after the timeout (exit 137).
	docker(t, "stop", "-t", "10", id)
	code := strings.TrimSpace(docker(t, "inspect", "-f", "{{.State.ExitCode}}", id))
	if code != "0" {
		t.Errorf("container exit code = %s, want 0", code)
	}
	logs := docker(t, "logs", id)
	if !strings.Contains(logs, `"msg":"server stopped"`) {
		t.Errorf("container logs missing clean shutdown:\n%s", logs)
	}
}

// dockerPort returns the host address a container's port is published on.
func dockerPort(t *testing.T, id, port string) string {
	t.Helper()
	out := strings.TrimSpace(docker(t, "port", id, port))
	return strings.Split(out, "\n")[0]
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

// docker runs a docker command that should finish quickly, and returns its
// output.
func docker(t *testing.T, args ...string) string {
	const timeout = 30 * time.Second
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
	cmd      *exec.Cmd
	addr     string // HTTP
	natsAddr string // the embedded NATS server
	external string // LCARS_NATS_URL, if set
	stdout   *syncBuffer
	stderr   *syncBuffer

	done     chan struct{}
	exitCode int
}

// launch starts the binary on free ports (HTTP and NATS) without waiting for
// it to be ready. Later env entries override earlier ones, so callers can
// replace LCARS_ADDR and LCARS_NATS_ADDR.
func launch(t *testing.T, env ...string) *server {
	t.Helper()
	return launchIn(t, "", env...)
}

// launchIn is launch with the given working directory ("" for the test's).
func launchIn(t *testing.T, dir string, env ...string) *server {
	t.Helper()
	p := &server{
		addr:     freeAddr(t),
		natsAddr: freeAddr(t),
		stdout:   &syncBuffer{},
		stderr:   &syncBuffer{},
		done:     make(chan struct{}),
	}
	p.cmd = exec.Command(binPath)
	p.cmd.Dir = dir
	p.cmd.Env = append([]string{"LCARS_ADDR=" + p.addr, "LCARS_NATS_ADDR=" + p.natsAddr}, env...)
	for _, kv := range p.cmd.Env {
		key, v, _ := strings.Cut(kv, "=")
		cover(settingsCoverage, key)
		switch key {
		case "LCARS_ADDR":
			p.addr = v
		case "LCARS_NATS_ADDR":
			p.natsAddr = v
		case "LCARS_NATS_URL":
			p.external = v
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

// natsURL is where NATS clients connect: the embedded server, or the external
// one the binary was told to use.
func (p *server) natsURL() string {
	if p.external != "" {
		return p.external
	}
	return "nats://" + p.natsAddr
}

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
	return send(req)
}

// send is do for a prepared request, such as one with a body or headers.
func send(req *http.Request) (*response, error) {
	cover(pathCoverage, req.URL.Path)
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
