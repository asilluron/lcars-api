package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/asilluron/lcars-api/internal/devices"
	"github.com/asilluron/lcars-api/internal/supabase"
)

// fakeAdmins accepts one admin, with one access token.
type fakeAdmins struct{ err error }

const (
	testEmail    = "admin@example.com"
	testPassword = "correct horse"
	testToken    = "access-token"
)

func (f fakeAdmins) SignIn(_ context.Context, email, password string) (supabase.Session, error) {
	if f.err != nil {
		return supabase.Session{}, f.err
	}
	if email != testEmail || password != testPassword {
		return supabase.Session{}, supabase.ErrInvalidCredentials
	}
	return supabase.Session{AccessToken: testToken, ExpiresAt: 1900000000, User: supabase.User{ID: "admin-1", Email: email}}, nil
}

func (f fakeAdmins) User(_ context.Context, token string) (supabase.User, error) {
	if f.err != nil {
		return supabase.User{}, f.err
	}
	if token != testToken {
		return supabase.User{}, supabase.ErrInvalidCredentials
	}
	return supabase.User{ID: "admin-1", Email: testEmail}, nil
}

// fakeTokenDB is an in-memory devices.DB.
type fakeTokenDB struct {
	mu   sync.Mutex
	rows []supabase.DeviceToken
}

func (f *fakeTokenDB) ListDeviceTokens(context.Context) ([]supabase.DeviceToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.rows), nil
}

func (f *fakeTokenDB) InsertDeviceToken(_ context.Context, t supabase.DeviceToken) (supabase.DeviceToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t.CreatedAt = time.Date(2030, 3, 4, 9, 0, 0, 0, time.UTC)
	f.rows = append(f.rows, t)
	return t, nil
}

func (f *fakeTokenDB) DeleteDeviceToken(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.rows)
	f.rows = slices.DeleteFunc(f.rows, func(r supabase.DeviceToken) bool { return r.ID == id })
	if len(f.rows) == n {
		return supabase.ErrNotFound
	}
	return nil
}

func newAdminServer(t *testing.T, admins AdminAuth) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	store := devices.NewStore(&fakeTokenDB{}, logger, nil)
	srv := httptest.NewServer(New(logger, Deps{Devices: store, Admins: admins}))
	t.Cleanup(srv.Close)
	return srv
}

// result is the status and headers of a response whose body has been read.
type result struct {
	StatusCode int
	Header     http.Header
}

// call sends a request with an optional JSON body and bearer token, and
// decodes the JSON response into out (if non-nil).
func call(t *testing.T, srv *httptest.Server, method, path, bearer, body string, out any) result {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
	return result{StatusCode: resp.StatusCode, Header: resp.Header}
}

func TestAdminSignIn(t *testing.T) {
	srv := newAdminServer(t, fakeAdmins{})

	var s signInResponse
	resp := call(t, srv, http.MethodPost, "/admin/api/session", "",
		`{"email":"admin@example.com","password":"correct horse"}`, &s)
	if resp.StatusCode != http.StatusOK || s.AccessToken != testToken || s.Email != testEmail ||
		!s.ExpiresAt.Equal(time.Unix(1900000000, 0)) {
		t.Errorf("sign in = %d %+v", resp.StatusCode, s)
	}

	for name, tt := range map[string]struct {
		body string
		want int
	}{
		"wrong password": {`{"email":"admin@example.com","password":"nope"}`, http.StatusUnauthorized},
		"missing field":  {`{"email":"admin@example.com"}`, http.StatusBadRequest},
		"unknown field":  {`{"email":"a","password":"b","admin":true}`, http.StatusBadRequest},
		"not json":       {`email=admin`, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			if resp := call(t, srv, http.MethodPost, "/admin/api/session", "", tt.body, nil); resp.StatusCode != tt.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/admin/api/session", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "text/plain")
	if resp, err := srv.Client().Do(req); err != nil || resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain body = %v, %v; want 415", resp, err)
	} else {
		resp.Body.Close()
	}
}

func TestAdminSupabaseErrors(t *testing.T) {
	for name, tt := range map[string]struct {
		err  error
		want int
	}{
		"rate limited": {supabase.ErrRateLimited, http.StatusTooManyRequests},
		"unreachable":  {errors.New("dial tcp: connection refused"), http.StatusServiceUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newAdminServer(t, fakeAdmins{err: tt.err})
			var body map[string]string
			resp := call(t, srv, http.MethodPost, "/admin/api/session", "", `{"email":"a","password":"b"}`, &body)
			if resp.StatusCode != tt.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.want)
			}
			if strings.Contains(body["error"], "connection refused") {
				t.Errorf("error leaks details: %q", body["error"])
			}
		})
	}
}

func TestAdminTokens(t *testing.T) {
	srv := newAdminServer(t, fakeAdmins{})

	// Every token operation needs a valid session.
	for _, bearer := range []string{"", "expired"} {
		for _, op := range [][2]string{
			{http.MethodGet, "/admin/api/tokens"},
			{http.MethodPost, "/admin/api/tokens"},
			{http.MethodDelete, "/admin/api/tokens/dev_000000000000"},
		} {
			resp := call(t, srv, op[0], op[1], bearer, `{"name":"x"}`, nil)
			if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
				t.Errorf("%s %s with token %q = %d, want 401 with WWW-Authenticate", op[0], op[1], bearer, resp.StatusCode)
			}
		}
	}

	var created deviceToken
	resp := call(t, srv, http.MethodPost, "/admin/api/tokens", testToken, `{"name":"Kitchen display"}`, &created)
	if resp.StatusCode != http.StatusCreated || created.Name != "Kitchen display" || created.Secret == "" ||
		!strings.HasPrefix(created.ID, "dev_") {
		t.Fatalf("create = %d %+v", resp.StatusCode, created)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if resp := call(t, srv, http.MethodPost, "/admin/api/tokens", testToken, `{"name":"  "}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("create with blank name = %d, want 400", resp.StatusCode)
	}

	// The token works as Basic auth on device endpoints.
	basic := func(user, password string) int {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/calendar", nil)
		if user != "" {
			req.SetBasicAuth(user, password)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// 503: authenticated, but no calendar is configured.
	if got := basic(created.ID, created.Secret); got != http.StatusServiceUnavailable {
		t.Errorf("GET /calendar with the token = %d, want 503", got)
	}
	for _, c := range [][2]string{{"", ""}, {created.ID, "wrong"}, {"dev_000000000000", created.Secret}} {
		if got := basic(c[0], c[1]); got != http.StatusUnauthorized {
			t.Errorf("GET /calendar as %q/%q = %d, want 401", c[0], c[1], got)
		}
	}

	var list struct{ Tokens []deviceToken }
	call(t, srv, http.MethodGet, "/admin/api/tokens", testToken, "", &list)
	if len(list.Tokens) != 1 || list.Tokens[0].ID != created.ID || list.Tokens[0].Secret != "" {
		t.Errorf("list = %+v, want the new token without its secret", list.Tokens)
	}

	if resp := call(t, srv, http.MethodDelete, "/admin/api/tokens/"+created.ID, testToken, "", nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("revoke = %d, want 204", resp.StatusCode)
	}
	if resp := call(t, srv, http.MethodDelete, "/admin/api/tokens/"+created.ID, testToken, "", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("revoke again = %d, want 404", resp.StatusCode)
	}
	if got := basic(created.ID, created.Secret); got != http.StatusUnauthorized {
		t.Errorf("GET /calendar with a revoked token = %d, want 401", got)
	}
}

func TestAdminDisabled(t *testing.T) {
	srv := newTestServer(t) // no Supabase
	for _, op := range [][2]string{
		{http.MethodPost, "/admin/api/session"},
		{http.MethodGet, "/admin/api/tokens"},
		{http.MethodPost, "/admin/api/tokens"},
		{http.MethodDelete, "/admin/api/tokens/dev_000000000000"},
	} {
		if resp := call(t, srv, op[0], op[1], testToken, `{}`, nil); resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503", op[0], op[1], resp.StatusCode)
		}
	}
	// Device endpoints are open.
	if resp := call(t, srv, http.MethodGet, "/calendar", "", "", nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET /calendar = %d, want 503 (no calendar), not 401", resp.StatusCode)
	}
}

func TestAdminPage(t *testing.T) {
	files := fstest.MapFS{
		"index.html":         {Data: []byte(`<!doctype html><script src="./assets/app-1a2b.js"></script>`)},
		"assets/app-1a2b.js": {Data: []byte(`console.log("hi")`)},
	}
	mux := http.NewServeMux()
	mux.Handle("GET /admin/", handle(slog.New(slog.DiscardHandler), adminPage(files, true)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for _, tt := range []struct {
		path, wantType, wantCache string
		wantStatus                int
	}{
		{"/admin/", "text/html; charset=utf-8", "no-cache", http.StatusOK},
		{"/admin/assets/app-1a2b.js", "text/javascript; charset=utf-8", "public, max-age=31536000, immutable", http.StatusOK},
		{"/admin/assets/", "", "", http.StatusNotFound},
		{"/admin/assets", "", "", http.StatusNotFound},
		{"/admin/missing.js", "", "", http.StatusNotFound},
		{"/admin/assets/../index.html", "", "", http.StatusOK}, // cleaned by the client and mux
	} {
		resp := call(t, srv, http.MethodGet, tt.path, "", "", nil)
		if resp.StatusCode != tt.wantStatus {
			t.Errorf("GET %s = %d, want %d", tt.path, resp.StatusCode, tt.wantStatus)
			continue
		}
		if tt.wantType != "" && resp.Header.Get("Content-Type") != tt.wantType {
			t.Errorf("GET %s Content-Type = %q, want %q", tt.path, resp.Header.Get("Content-Type"), tt.wantType)
		}
		if tt.wantCache != "" && resp.Header.Get("Cache-Control") != tt.wantCache {
			t.Errorf("GET %s Cache-Control = %q, want %q", tt.path, resp.Header.Get("Cache-Control"), tt.wantCache)
		}
		if resp.Header.Get("Content-Security-Policy") == "" {
			t.Errorf("GET %s has no Content-Security-Policy", tt.path)
		}
	}

	// GET /admin redirects to the page.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(srv.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 3 || resp.Header.Get("Location") != "/admin/" {
		t.Errorf("GET /admin = %d to %q, want a redirect to /admin/", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestAdminPageNotBuilt(t *testing.T) {
	rec := httptest.NewRecorder()
	h := handle(slog.New(slog.DiscardHandler), adminPage(fstest.MapFS{}, false))
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "make admin-ui") {
		t.Errorf("unbuilt admin page = %d %q, want 503 explaining how to build it", rec.Code, rec.Body.String())
	}
}
