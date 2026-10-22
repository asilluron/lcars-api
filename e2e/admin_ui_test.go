//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestAdminUI runs the browser tests in admin/e2e (Playwright, in Docker)
// against the admin page of the Docker image, with a real Supabase: signing
// in, creating a device token and using it, and revoking it.
func TestAdminUI(t *testing.T) {
	requireDocker(t)
	sb := requireSupabase(t)

	// The Playwright image must match the @playwright/test version, or the
	// browsers it has won't be the ones the tests expect.
	playwright := makeVar(t, "PLAYWRIGHT")
	data, err := os.ReadFile("../admin/package.json")
	if err != nil {
		t.Fatal(err)
	}
	var pkg struct {
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		t.Fatal(err)
	}
	if v := pkg.DevDependencies["@playwright/test"]; !strings.Contains(playwright, ":v"+v+"-") {
		t.Fatalf("Makefile PLAYWRIGHT = %s, but admin/package.json has @playwright/test %s", playwright, v)
	}

	buildImage(t)
	email, password := createAdmin(t)

	alias := "lcars-ui-" + strconv.Itoa(os.Getpid())
	config := writeConfigFile(t, "timezone = \"America/New_York\"\n\n[calendar]\ndir = \"/calendars\"\n\n"+
		"[supabase]\nurl = \""+supabaseKongURL+"\"\nsecret_key = \""+sb.SecretKey+"\"\n")
	server := strings.TrimSpace(docker(t, "run", "-d",
		"--network", supabaseNetwork, "--network-alias", alias,
		"-p", "127.0.0.1::8080",
		"-v", fixturesDir(t)+":/calendars:ro",
		"-v", config+":/config/lcars.toml:ro",
		image))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", server).Run() })
	waitReady(t, "http://"+dockerPort(t, server, "8080/tcp")+"/healthz")

	adminDir, err := filepath.Abs("../admin")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("running the Playwright tests (the first run downloads the Playwright image)")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm",
		"--network", supabaseNetwork,
		"-v", adminDir+":/src/admin",
		"-v", "lcars-admin-node-modules:/src/admin/node_modules",
		"-v", "lcars-npm-cache:/root/.npm",
		"-w", "/src/admin",
		"-e", "BASE_URL=http://"+alias+":8080/admin/",
		"-e", "ADMIN_EMAIL", "-e", "ADMIN_PASSWORD", // from cmd.Env, so not on the command line
		playwright,
		"sh", "-c", "npm ci --no-audit --no-fund --loglevel=error && npx playwright test")
	cmd.Env = append(os.Environ(), "ADMIN_EMAIL="+email, "ADMIN_PASSWORD="+password)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("playwright: %v\n%s\nserver logs:\n%s", err, out.String(), docker(t, "logs", server))
	}
	t.Logf("playwright:\n%s", out.String())
	if !strings.Contains(out.String(), "4 passed") {
		t.Errorf("want 4 Playwright tests to pass:\n%s", out.String())
	}
}
