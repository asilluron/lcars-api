package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestRun starts the real server on free ports, with an embedded NATS server,
// checks /healthz, then cancels the context and expects a clean shutdown.
func TestRun(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(t.Context())

	getenv := func(k string) string {
		return map[string]string{"LCARS_ADDR": addr, "LCARS_NATS_ADDR": "127.0.0.1:0"}[k]
	}
	done := make(chan error, 1)
	go func() { done <- run(ctx, getenv, io.Discard) }()

	waitForReady(t, "http://"+addr+"/healthz")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel")
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

func waitForReady(t *testing.T, url string) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server at %s never became ready", url)
}
