package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// TestHealthzNATS checks that /healthz reports the NATS connection, and
// answers 503 while it is down.
func TestHealthzNATS(t *testing.T) {
	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: natsserver.RANDOM_PORT, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("NATS server not ready")
	}
	nc, err := nats.Connect(ns.ClientURL(), nats.MaxReconnects(-1))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	srv := httptest.NewServer(New(slog.New(slog.DiscardHandler), Deps{NATS: nc}))
	defer srv.Close()
	check := func(wantCode int, wantStatus, wantNATS string) {
		t.Helper()
		resp, err := srv.Client().Get(srv.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body healthResponse
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != wantCode || body.Status != wantStatus || body.NATS != wantNATS {
			t.Errorf("healthz = %d %+v, want %d status=%s nats=%s", resp.StatusCode, body, wantCode, wantStatus, wantNATS)
		}
	}

	check(http.StatusOK, "ok", "connected")
	ns.Shutdown()
	ns.WaitForShutdown()
	deadline := time.Now().Add(5 * time.Second)
	for nc.IsConnected() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	check(http.StatusServiceUnavailable, "unavailable", "disconnected")
}
