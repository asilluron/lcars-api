//go:build e2e

package e2e

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// The local Supabase stack, started with `supabase start` (`make e2e` does).
// Its Docker network and API gateway are named by the Supabase CLI after
// project_id in supabase/config.toml; containers on that network reach the
// API at supabaseKongURL.
const (
	supabaseNetwork = "supabase_network_lcars-api"
	supabaseKongURL = "http://supabase_kong_lcars-api:8000"
)

// supabaseInfo is what `supabase status` reports.
type supabaseInfo struct {
	APIURL    string `json:"API_URL"`
	SecretKey string `json:"SECRET_KEY"`
}

var supabaseStatus = sync.OnceValues(func() (supabaseInfo, error) {
	cmd := exec.Command("supabase", "status", "-o", "json")
	cmd.Dir = ".."
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return supabaseInfo{}, fmt.Errorf("supabase status: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var info supabaseInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return supabaseInfo{}, fmt.Errorf("parse supabase status: %w", err)
	}
	if info.APIURL == "" || info.SecretKey == "" {
		return supabaseInfo{}, errors.New("supabase status reported no API URL or secret key")
	}
	return info, nil
})

// requireSupabase returns the local Supabase stack, or skips the test if it
// isn't running (fails instead with E2E_REQUIRE_DOCKER=1, as `make e2e` and
// CI set).
func requireSupabase(t *testing.T) supabaseInfo {
	t.Helper()
	info, err := supabaseStatus()
	if err == nil {
		return info
	}
	if os.Getenv("E2E_REQUIRE_DOCKER") == "1" {
		t.Fatalf("local Supabase required but not running (start it with `supabase start`): %v", err)
	}
	t.Skipf("local Supabase not running (%v); start it with `supabase start`, or run `make e2e`", err)
	return supabaseInfo{}
}

// env returns the settings that point lcars-api at this Supabase.
func (s supabaseInfo) env() []string {
	return []string{"LCARS_SUPABASE_URL=" + s.APIURL, "LCARS_SUPABASE_SECRET_KEY=" + s.SecretKey}
}

// call sends a request straight to Supabase with the secret key, bypassing
// lcars-api (so it isn't counted as API coverage), and returns the body.
func (s supabaseInfo) call(t *testing.T, method, path string, body any) []byte {
	t.Helper()
	var r io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, s.APIURL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Apikey", s.SecretKey)
	req.Header.Set("Authorization", "Bearer "+s.SecretKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("supabase %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("supabase %s %s = %d %s", method, path, resp.StatusCode, data)
	}
	return data
}

// createAdmin adds a Supabase Auth user for the test, deleted afterwards,
// and returns its email and password.
func createAdmin(t *testing.T) (email, password string) {
	t.Helper()
	s := requireSupabase(t)
	email = "e2e-" + strings.ToLower(rand.Text()[:12]) + "@example.com"
	password = rand.Text()
	var user struct{ ID string }
	data := s.call(t, http.MethodPost, "/auth/v1/admin/users",
		map[string]any{"email": email, "password": password, "email_confirm": true})
	if err := json.Unmarshal(data, &user); err != nil || user.ID == "" {
		t.Fatalf("create admin: %v %s", err, data)
	}
	t.Cleanup(func() {
		// Tokens it created are kept, with created_by set to null.
		req, _ := http.NewRequest(http.MethodDelete, s.APIURL+"/auth/v1/admin/users/"+user.ID, nil)
		req.Header.Set("Apikey", s.SecretKey)
		req.Header.Set("Authorization", "Bearer "+s.SecretKey)
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
		}
	})
	return email, password
}

// adminCall sends a request to the admin API of the server at base, with an
// optional JSON body and access token.
func adminCall(t *testing.T, base, method, path, bearer string, body any) *response {
	t.Helper()
	var r io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, base+path, r)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := send(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// signIn signs in to the admin API and returns the access token.
func signIn(t *testing.T, base, email, password string) string {
	t.Helper()
	resp := adminCall(t, base, http.MethodPost, "/admin/api/session", "",
		map[string]string{"email": email, "password": password})
	var s struct {
		AccessToken string    `json:"access_token"`
		ExpiresAt   time.Time `json:"expires_at"`
		Email       string    `json:"email"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(resp.body, &s) != nil || s.AccessToken == "" {
		t.Fatalf("sign in = %d %s", resp.StatusCode, resp.body)
	}
	if s.Email != email || time.Until(s.ExpiresAt) < 30*time.Minute {
		t.Errorf("session email %q expires %s; want %s, an hour from now", s.Email, s.ExpiresAt, email)
	}
	return s.AccessToken
}

type deviceToken struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Secret    string    `json:"secret"`
}

// createToken makes a device token through the admin API. It is deleted
// from the database after the test, if the test hasn't revoked it.
func createToken(t *testing.T, base, bearer, name string) deviceToken {
	t.Helper()
	resp := adminCall(t, base, http.MethodPost, "/admin/api/tokens", bearer, map[string]string{"name": name})
	var tok deviceToken
	if resp.StatusCode != http.StatusCreated || json.Unmarshal(resp.body, &tok) != nil {
		t.Fatalf("create token = %d %s", resp.StatusCode, resp.body)
	}
	s := requireSupabase(t)
	t.Cleanup(func() {
		req, _ := http.NewRequest(http.MethodDelete, s.APIURL+"/rest/v1/device_tokens?id=eq."+tok.ID, nil)
		req.Header.Set("Apikey", s.SecretKey)
		req.Header.Set("Authorization", "Bearer "+s.SecretKey)
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
		}
	})
	return tok
}

// basicURL returns base with tok as its Basic auth credentials, which the
// HTTP client sends as an Authorization header.
func basicURL(base string, tok deviceToken) string {
	u, _ := url.Parse(base)
	u.User = url.UserPassword(tok.ID, tok.Secret)
	return u.String()
}

// natsURL returns the NATS URL with tok as its credentials.
func natsURLWith(natsURL string, tok deviceToken) string { return basicURL(natsURL, tok) }

// authServer starts the binary with authentication on, the fixture calendar,
// and a signed-in admin, and returns it with the admin's access token.
func authServer(t *testing.T, env ...string) (*server, string) {
	t.Helper()
	s := requireSupabase(t)
	email, password := createAdmin(t)
	env = append(append(s.env(), "LCARS_CALENDAR_DIR="+fixturesDir(t), "LCARS_TIMEZONE=America/New_York"), env...)
	p := startServer(t, env...)
	return p, signIn(t, p.url(""), email, password)
}

func TestAdminAPI(t *testing.T) {
	s := requireSupabase(t)
	email, password := createAdmin(t)
	p := startServer(t, s.env()...)
	base := p.url("")

	// Sign-in failures.
	for name, tt := range map[string]struct {
		body any
		ct   string
		want int
	}{
		"wrong password": {map[string]string{"email": email, "password": "wrong"}, "", http.StatusUnauthorized},
		"unknown admin":  {map[string]string{"email": "nobody@example.com", "password": password}, "", http.StatusUnauthorized},
		"missing field":  {map[string]string{"email": email}, "", http.StatusBadRequest},
		"unknown field":  {map[string]any{"email": email, "password": password, "role": "admin"}, "", http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			if resp := adminCall(t, base, http.MethodPost, "/admin/api/session", "", tt.body); resp.StatusCode != tt.want {
				t.Errorf("sign in = %d %s, want %d", resp.StatusCode, resp.body, tt.want)
			}
		})
	}
	req, _ := http.NewRequest(http.MethodPost, base+"/admin/api/session", strings.NewReader("email=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if resp, err := send(req); err != nil || resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("form-encoded sign in = %v, %v; want 415", resp, err)
	}

	token := signIn(t, base, email, password)

	// Token operations need a valid session.
	for _, bearer := range []string{"", "not-a-session", token + "x"} {
		for _, op := range [][2]string{
			{http.MethodGet, "/admin/api/tokens"},
			{http.MethodPost, "/admin/api/tokens"},
			{http.MethodDelete, "/admin/api/tokens/dev_000000000000"},
		} {
			resp := adminCall(t, base, op[0], op[1], bearer, map[string]string{"name": "x"})
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s with session %.10q = %d, want 401", op[0], op[1], bearer, resp.StatusCode)
			}
		}
	}

	// Create.
	name := "Kitchen display " + rand.Text()[:6]
	tok := createToken(t, base, token, name)
	if !regexp.MustCompile(`^dev_[a-z0-9]{12}$`).MatchString(tok.ID) ||
		!regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(tok.Secret) ||
		tok.Name != name || time.Since(tok.CreatedAt).Abs() > time.Minute {
		t.Errorf("created token = %+v", tok)
	}
	for _, bad := range []any{map[string]string{"name": " "}, map[string]string{"name": strings.Repeat("x", 101)}, map[string]int{"name": 1}} {
		if resp := adminCall(t, base, http.MethodPost, "/admin/api/tokens", token, bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("create %v = %d, want 400", bad, resp.StatusCode)
		}
	}

	// The secret is stored only as a hash.
	row := s.call(t, http.MethodGet, "/rest/v1/device_tokens?id=eq."+tok.ID, nil)
	if !regexp.MustCompile(`"secret_hash":"[0-9a-f]{64}"`).Match(row) || bytes.Contains(row, []byte(tok.Secret)) {
		t.Errorf("stored row %s contains the secret, or no SHA-256", row)
	}

	// List: the token is there, without its secret.
	resp := adminCall(t, base, http.MethodGet, "/admin/api/tokens", token, nil)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(resp.body, []byte(tok.ID)) || bytes.Contains(resp.body, []byte(tok.Secret)) {
		t.Errorf("list = %d %s, want the new token without its secret", resp.StatusCode, resp.body)
	}

	// Revoke.
	if resp := adminCall(t, base, http.MethodDelete, "/admin/api/tokens/"+tok.ID, token, nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("revoke = %d %s, want 204", resp.StatusCode, resp.body)
	}
	for _, id := range []string{tok.ID, "dev_000000000000", "nonsense"} {
		if resp := adminCall(t, base, http.MethodDelete, "/admin/api/tokens/"+id, token, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("revoke %s = %d, want 404", id, resp.StatusCode)
		}
	}
	if resp := adminCall(t, base, http.MethodGet, "/admin/api/tokens", token, nil); bytes.Contains(resp.body, []byte(tok.ID)) {
		t.Errorf("revoked token still listed: %s", resp.body)
	}

	// The admin page loads, with the scripts and styles it references.
	page := mustDo(t, http.MethodGet, base+"/admin/")
	if page.StatusCode != http.StatusOK || !bytes.Contains(page.body, []byte(`<div id="root">`)) ||
		page.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("GET /admin/ = %d %s", page.StatusCode, page.body)
	}
	assets := regexp.MustCompile(`(?:src|href)="\./(assets/[^"]+)"`).FindAllSubmatch(page.body, -1)
	if len(assets) < 2 {
		t.Errorf("admin page references %d assets, want a script and a stylesheet", len(assets))
	}
	for _, m := range assets {
		r := mustDo(t, http.MethodGet, base+"/admin/"+string(m[1]))
		if r.StatusCode != http.StatusOK || !strings.Contains(r.Header.Get("Cache-Control"), "immutable") {
			t.Errorf("GET /admin/%s = %d (Cache-Control %q)", m[1], r.StatusCode, r.Header.Get("Cache-Control"))
		}
	}

	// Admin actions are logged by user ID, never by email.
	for _, want := range []map[string]any{
		{"msg": "device authentication enabled", "level": "INFO"},
		{"msg": "device tokens loaded"},
		{"msg": "admin signed in", "level": "INFO"},
		{"msg": "admin sign-in failed", "level": "WARN"},
		{"msg": "device token created", "id": tok.ID},
		{"msg": "device token revoked", "id": tok.ID},
	} {
		if !p.hasLog(want) {
			t.Errorf("no log line matching %v", want)
		}
	}
	for _, secret := range []string{email, password, tok.Secret, token, s.SecretKey} {
		if strings.Contains(p.stdout.String()+p.stderr.String(), secret) {
			t.Errorf("logs contain a credential or email (%.6s...)", secret)
		}
	}
}

func TestDeviceAuthHTTP(t *testing.T) {
	p, admin := authServer(t)
	base := p.url("")

	resp := mustDo(t, http.MethodGet, base+"/calendar")
	if resp.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic ") {
		t.Errorf("GET /calendar without a token = %d (WWW-Authenticate %q), want 401 asking for Basic auth",
			resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}

	tok := createToken(t, base, admin, "Hallway display")
	checkFixtureWeek(t, basicURL(base, tok))

	wrong := tok
	wrong.Secret = strings.Repeat("A", 43)
	for name, u := range map[string]string{
		"wrong secret":  basicURL(base, wrong),
		"admin session": base, // a Bearer admin token is not a device token
	} {
		req, _ := http.NewRequest(http.MethodGet, u+"/calendar", nil)
		if name == "admin session" {
			req.Header.Set("Authorization", "Bearer "+admin)
		}
		if resp, err := send(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: GET /calendar = %v, %v; want 401", name, resp, err)
		}
	}

	// Health, docs and the admin page stay open.
	for _, path := range []string{"/healthz", "/openapi.json", "/docs", "/admin/"} {
		if r := mustDo(t, http.MethodGet, base+path); r.StatusCode != http.StatusOK {
			t.Errorf("GET %s without a token = %d, want 200", path, r.StatusCode)
		}
	}

	adminCall(t, base, http.MethodDelete, "/admin/api/tokens/"+tok.ID, admin, nil)
	if r := mustDo(t, http.MethodGet, basicURL(base, tok)+"/calendar"); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /calendar with a revoked token = %d, want 401", r.StatusCode)
	}
}

func TestDeviceAuthNATS(t *testing.T) {
	p, admin := authServer(t)
	tok := createToken(t, p.url(""), admin, "Bedroom display")

	for name, u := range map[string]string{
		"no credentials": p.natsURL(),
		"wrong secret":   natsURLWith(p.natsURL(), deviceToken{ID: tok.ID, Secret: "wrong"}),
		"service user":   "nats://lcars-api:guess@" + p.natsAddr,
	} {
		if nc, err := nats.Connect(u, nats.Timeout(5*time.Second)); err == nil {
			nc.Close()
			t.Errorf("%s: connected, want an authorization error", name)
		} else if !errors.Is(err, nats.ErrAuthorization) {
			t.Errorf("%s: err = %v, want an authorization error", name, err)
		}
	}

	// With the token: requests and events work, nothing else is allowed.
	checkFixtureWeekNATS(t, natsURLWith(p.natsURL(), tok))
	permErrs := make(chan error, 4)
	closed := make(chan struct{})
	nc, err := nats.Connect(natsURLWith(p.natsURL(), tok), nats.NoReconnect(),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) { permErrs <- err }),
		nats.ClosedHandler(func(*nats.Conn) { close(closed) }))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	if _, err := nc.SubscribeSync(subjectCalendarUpdated); err != nil {
		t.Fatal(err)
	}
	mustRequestCalendar(t, nc, "2030-03-04", 1)
	_ = nc.Publish(subjectCalendarUpdated, nil) // devices can't fake events
	_, _ = nc.SubscribeSync("$SRV.>")           // or watch other traffic
	_ = nc.Flush()
	for range 2 {
		select {
		case err := <-permErrs:
			if !errors.Is(err, nats.ErrPermissionViolation) {
				t.Errorf("err = %v, want a permissions violation", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no permissions violation reported")
		}
	}

	// Revoking closes the connection at once, and the token can't reconnect.
	adminCall(t, p.url(""), http.MethodDelete, "/admin/api/tokens/"+tok.ID, admin, nil)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("NATS connection still open after its token was revoked")
	}
	if nc2, err := nats.Connect(natsURLWith(p.natsURL(), tok)); err == nil {
		nc2.Close()
		t.Error("revoked token can still connect")
	}
}

// TestTokenRefresh checks that tokens deleted directly in the database (not
// through the admin API) stop working after the refresh interval.
func TestTokenRefresh(t *testing.T) {
	s := requireSupabase(t)
	p, admin := authServer(t, "LCARS_SUPABASE_REFRESH_INTERVAL=1s")
	tok := createToken(t, p.url(""), admin, "Garage display")

	closed := make(chan struct{})
	nc, err := nats.Connect(natsURLWith(p.natsURL(), tok), nats.NoReconnect(),
		nats.ClosedHandler(func(*nats.Conn) { close(closed) }))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	s.call(t, http.MethodDelete, "/rest/v1/device_tokens?id=eq."+tok.ID, nil)
	waitFor(t, 10*time.Second, "the deleted token to be refused", func() bool {
		r, err := do(http.MethodGet, basicURL(p.url(""), tok)+"/calendar")
		return err == nil && r.StatusCode == http.StatusUnauthorized
	})
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Error("NATS connection still open after its token was deleted")
	}
}

// TestSupabaseUnavailable checks that the server still starts when Supabase
// is down, refuses devices until it can load the tokens, and reports the
// admin API as unavailable.
func TestSupabaseUnavailable(t *testing.T) {
	p := startServer(t,
		"LCARS_SUPABASE_URL=http://"+freeAddr(t), "LCARS_SUPABASE_SECRET_KEY=sb_secret_example",
		"LCARS_CALENDAR_DIR="+fixturesDir(t))

	if r := mustDo(t, http.MethodGet, p.url("/healthz")); r.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", r.StatusCode)
	}
	if r := mustDo(t, http.MethodGet, p.url("/calendar")); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /calendar = %d, want 401", r.StatusCode)
	}
	if _, err := nats.Connect(p.natsURL()); !errors.Is(err, nats.ErrAuthorization) {
		t.Errorf("NATS connect = %v, want an authorization error", err)
	}
	resp := adminCall(t, p.url(""), http.MethodPost, "/admin/api/session", "",
		map[string]string{"email": "admin@example.com", "password": "x"})
	if resp.StatusCode != http.StatusServiceUnavailable || bytes.Contains(resp.body, []byte("connection refused")) {
		t.Errorf("sign in = %d %s, want a generic 503", resp.StatusCode, resp.body)
	}
	for _, want := range []map[string]any{
		{"msg": "device tokens unavailable; devices are refused until they load", "level": "ERROR"},
		{"msg": "supabase request failed", "level": "ERROR"},
	} {
		if !p.hasLog(want) {
			t.Errorf("no log line matching %v", want)
		}
	}
	if strings.Contains(p.stdout.String(), "sb_secret_example") {
		t.Error("logs contain the secret key")
	}
}

// TestAdminDisabled checks the server without Supabase: no authentication,
// and an admin API that says how to enable it.
func TestAdminDisabled(t *testing.T) {
	p := startServer(t, "LCARS_CALENDAR_DIR="+fixturesDir(t), "LCARS_TIMEZONE=America/New_York")

	checkFixtureWeek(t, p.url(""))
	checkFixtureWeekNATS(t, p.natsURL())
	for _, op := range [][2]string{
		{http.MethodPost, "/admin/api/session"},
		{http.MethodGet, "/admin/api/tokens"},
		{http.MethodPost, "/admin/api/tokens"},
		{http.MethodDelete, "/admin/api/tokens/dev_000000000000"},
	} {
		resp := adminCall(t, p.url(""), op[0], op[1], "x", map[string]string{"name": "x"})
		if resp.StatusCode != http.StatusServiceUnavailable || !bytes.Contains(resp.body, []byte("supabase.url")) {
			t.Errorf("%s %s = %d %s, want 503 naming the setting", op[0], op[1], resp.StatusCode, resp.body)
		}
	}
	if !p.hasLog(map[string]any{
		"level": "WARN",
		"msg":   "device authentication disabled: supabase.url is not set, so anyone on the network can use the API",
	}) {
		t.Error("no warning that authentication is off")
	}
}
