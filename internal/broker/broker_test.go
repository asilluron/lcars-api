package broker

import (
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/asilluron/lcars-api/internal/config"
)

var discard = slog.New(slog.DiscardHandler)

// echo checks that a request on b's connection reaches a subscriber on nc.
func echo(t *testing.T, b *Broker, nc *nats.Conn) {
	t.Helper()
	sub, err := nc.Subscribe("test.echo", func(m *nats.Msg) { _ = m.Respond(m.Data) })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	msg, err := b.Conn.Request("test.echo", []byte("ping"), 5*time.Second)
	if err != nil || string(msg.Data) != "ping" {
		t.Fatalf("echo = %v, %v; want ping", msg, err)
	}
}

func TestEmbedded(t *testing.T) {
	b, err := Start(config.NATS{Addr: "127.0.0.1:0"}, discard)
	if err != nil {
		t.Fatal(err)
	}
	addr := b.server.Addr().String()

	// Network clients, like the display, can connect to the embedded server.
	client, err := nats.Connect("nats://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	echo(t, b, client)

	if err := b.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !b.Conn.IsClosed() {
		t.Error("connection still open after Close")
	}
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		conn.Close()
		t.Error("embedded server still listening after Close")
	}
}

func TestEmbeddedPortInUse(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	_, err = Start(config.NATS{Addr: l.Addr().String()}, discard)
	if err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Errorf("Start = %v, want an address in use error", err)
	}
}

func TestExternal(t *testing.T) {
	ns, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: server.RANDOM_PORT, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	ns.Start()
	defer ns.Shutdown()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("test server not ready")
	}

	// Addr must be ignored: nothing may listen on it.
	unused := freeAddr(t)
	b, err := Start(config.NATS{URL: ns.ClientURL(), Addr: unused}, discard)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close(t.Context()) }()
	if b.server != nil {
		t.Error("started an embedded server despite URL being set")
	}
	if conn, err := net.DialTimeout("tcp", unused, time.Second); err == nil {
		conn.Close()
		t.Errorf("something listens on %s", unused)
	}

	client, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	echo(t, b, client)
}

func TestExternalUnreachable(t *testing.T) {
	_, err := Start(config.NATS{URL: "nats://user:hunter2@" + freeAddr(t)}, discard)
	if err == nil {
		t.Fatal("Start: want error for unreachable server")
	}
	if !strings.Contains(err.Error(), "connect to NATS") {
		t.Errorf("error = %q, want it to mention connecting to NATS", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaks the password: %q", err)
	}
}

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"nats://192.0.2.10:4222":                       "nats://192.0.2.10:4222",
		"nats://user:secret@192.0.2.10:4222":           "nats://user:xxxxx@192.0.2.10:4222",
		"nats://s3cr3t-token@192.0.2.10:4222":          "nats://xxxxx@192.0.2.10:4222",
		"nats://a:b@192.0.2.10:4222,nats://192.0.2.11": "nats://a:xxxxx@192.0.2.10:4222,nats://192.0.2.11",
	} {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}
