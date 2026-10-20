package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// routeRecorder collects the patterns passed to Handle.
type routeRecorder []string

func (r *routeRecorder) Handle(pattern string, _ http.Handler) { *r = append(*r, pattern) }

// TestRoutesMatchOpenAPI checks that every route is documented in the OpenAPI
// document, and that the document describes no routes that don't exist.
func TestRoutesMatchOpenAPI(t *testing.T) {
	var routes routeRecorder
	addRoutes(&routes, slog.New(slog.DiscardHandler), Deps{})

	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(openAPITemplate, &spec); err != nil {
		t.Fatalf("parse openapi.json: %v", err)
	}
	var documented []string
	for path, ops := range spec.Paths {
		for method := range ops {
			documented = append(documented, strings.ToUpper(method)+" "+path)
		}
	}

	slices.Sort(routes)
	slices.Sort(documented)
	if !slices.Equal(routes, documented) {
		t.Errorf("routes and OpenAPI paths differ\nregistered: %q\ndocumented: %q", routes, documented)
	}
}

func TestOpenAPIVersionPlaceholder(t *testing.T) {
	if n := bytes.Count(openAPITemplate, []byte(versionPlaceholder)); n != 1 {
		t.Fatalf("openapi.json contains %s %d times, want exactly once (as info.version)", versionPlaceholder, n)
	}
}

func TestOpenAPI(t *testing.T) {
	srv := newTestServer(t)

	resp, err := srv.Client().Get(srv.URL + "/openapi.json")
	if err != nil {
		t.Fatalf("GET /openapi.json: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Version string `json:"version"`
		} `json:"info"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.OpenAPI != "3.1.0" {
		t.Errorf("openapi = %q, want 3.1.0", body.OpenAPI)
	}
	if body.Info.Version != Version() {
		t.Errorf("info.version = %q, want %q", body.Info.Version, Version())
	}
}

func TestDocs(t *testing.T) {
	srv := newTestServer(t)

	resp, err := srv.Client().Get(srv.URL + "/docs")
	if err != nil {
		t.Fatalf("GET /docs: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/html; charset=utf-8", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	for _, want := range []string{`Redoc.init("openapi.json"`, `integrity="sha384-`} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("body does not contain %q", want)
		}
	}
}
