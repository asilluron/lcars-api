//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
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

// TestRustLocks runs the Rust locks example against the server image with a
// fake Seam API. The example lists locks over NATS, unlocks, then locks.
func TestRustLocks(t *testing.T) {
	requireDocker(t)
	sb := requireSupabase(t)
	buildImage(t)
	email, password := createAdmin(t)

	upstream := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/locks/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"devices": []map[string]any{{
					"device_id":           testLockID,
					"display_name":        "Front door",
					"can_remotely_lock":   true,
					"can_remotely_unlock": true,
					"properties":          map[string]any{"locked": true, "online": true},
				}},
			})
		case "/locks/lock_door", "/locks/unlock_door":
			if !strings.Contains(string(body), testLockID) {
				t.Errorf("%s body = %s", r.URL.Path, body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"action_attempt": map[string]any{
					"action_attempt_id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
					"status":            "success",
				},
			})
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
	alias := "lcars-rust-lock-" + strconv.Itoa(os.Getpid())
	config := writeConfigFile(t, "[supabase]\nurl = \""+supabaseKongURL+"\"\nsecret_key = \""+sb.SecretKey+"\"\n")
	server := strings.TrimSpace(docker(t, "run", "-d",
		"--network", network, "--network-alias", alias,
		"--add-host=host.docker.internal:host-gateway",
		"-p", "127.0.0.1::8080",
		"-v", config+":/config/lcars.toml:ro",
		"-e", "LCARS_SEAM_API_KEY=seam_test_key",
		"-e", "LCARS_SEAM_URL=http://host.docker.internal:"+strconv.Itoa(port),
		image))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", server).Run() })
	base := "http://" + dockerPort(t, server, "8080/tcp")
	waitReady(t, base+"/healthz")
	tok := createToken(t, base, signIn(t, base, email, password), "Rust locks")

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
		"cargo", "build", "--locked", "--example", "locks")...)
	build.Stdout, build.Stderr = &built, &built
	if err := build.Run(); err != nil {
		t.Fatalf("cargo build: %v\n%s", err, built.String())
	}

	ctx, cancel = context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	var out bytes.Buffer
	run := exec.CommandContext(ctx, "docker", append(append(append([]string{"run", "--rm"}, credentials...), rustArgs...),
		"/target/debug/examples/locks", "nats://"+alias+":4222")...)
	run.Stdout, run.Stderr = &out, &out
	if err := run.Run(); err != nil {
		t.Fatalf("locks: %v\n%s", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "lock "+testLockID+" Front door") ||
		!strings.Contains(got, "unlocked "+testLockID) ||
		!strings.Contains(got, "locked "+testLockID) {
		t.Fatalf("client output:\n%s", got)
	}
	logs := docker(t, "logs", server)
	if strings.Contains(logs, "seam_test_key") {
		t.Errorf("server log contains the Seam API key:\n%s", logs)
	}
}
