//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRustCameras runs the Rust cameras example against the server image.
// The example lists cameras over NATS with a device token, then opens the
// returned still and live stream on the server's HTTP port. A fake upstream
// stands in for the camera proxy. Closing the example must end that upstream
// stream.
func TestRustCameras(t *testing.T) {
	requireDocker(t)
	sb := requireSupabase(t)
	buildImage(t)
	email, password := createAdmin(t)

	const frame = "FRAMEBYTES"
	closed := make(chan struct{})
	upstream := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	})}
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	go func() { _ = upstream.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = upstream.Shutdown(ctx)
	})

	network := supabaseNetwork
	alias := "lcars-rust-cam-" + strconv.Itoa(os.Getpid())
	config := writeConfigFile(t, "[supabase]\nurl = \""+supabaseKongURL+"\"\nsecret_key = \""+sb.SecretKey+"\"\n")
	server := strings.TrimSpace(docker(t, "run", "-d",
		"--network", network, "--network-alias", alias,
		"--add-host=host.docker.internal:host-gateway",
		"-p", "127.0.0.1::8080",
		"-v", config+":/config/lcars.toml:ro",
		"-e", "LCARS_EUFY_URL=http://host.docker.internal:"+strconv.Itoa(port),
		image))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", server).Run() })
	base := "http://" + dockerPort(t, server, "8080/tcp")
	waitReady(t, base+"/healthz")
	tok := createToken(t, base, signIn(t, base, email, password), "Rust cameras")

	rustDir, err := filepath.Abs("../rust")
	if err != nil {
		t.Fatal(err)
	}
	rustArgs := []string{
		"--network", network,
		"-v", rustDir + ":/src:ro",
		"-v", "lcars-cargo-registry:/usr/local/cargo/registry",
		"-v", "lcars-cargo-target:/target",
		"-e", "CARGO_TARGET_DIR=/target",
		"-w", "/src",
		makeVar(t, "RUST"),
	}
	credentials := []string{"-e", "NATS_USER=" + tok.ID, "-e", "NATS_PASSWORD=" + tok.Secret}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()
	var built bytes.Buffer
	build := exec.CommandContext(ctx, "docker", append(append(append([]string{"run", "--rm"}, credentials...), rustArgs...),
		"cargo", "build", "--locked", "--example", "cameras")...)
	build.Stdout, build.Stderr = &built, &built
	if err := build.Run(); err != nil {
		t.Fatalf("cargo build: %v\n%s", err, built.String())
	}

	ctx, cancel = context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	var out bytes.Buffer
	run := exec.CommandContext(ctx, "docker", append(append(append([]string{"run", "--rm"}, credentials...), rustArgs...),
		"/target/debug/examples/cameras", "nats://"+alias+":4222", "http://"+alias+":8080")...)
	run.Stdout, run.Stderr = &out, &out
	if err := run.Run(); err != nil {
		t.Fatalf("cameras: %v\n%s", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "camera CAM1 Front door") || !strings.Contains(got, "snapshot 4") || !strings.Contains(got, "stream "+frame) {
		t.Fatalf("client output:\n%s", got)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the client did not stop the upstream stream")
	}
	logs := docker(t, "logs", server)
	if strings.Contains(logs, frame) {
		t.Errorf("server log contains the frame bytes:\n%s", logs)
	}
}
