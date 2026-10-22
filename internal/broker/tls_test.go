package broker

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/asilluron/lcars-api/internal/config"
)

func TestEnsureServerCertKeepsTheCA(t *testing.T) {
	dir := t.TempDir()
	hosts := []string{"192.0.2.10", "nats.example.com"}
	cert, _, err := ensureServerCert(dir, hosts, discard)
	if err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(cert)
	if err != nil {
		t.Fatal(err)
	}
	parsed := parseCert(t, first)
	if !hasHost(parsed, "192.0.2.10") || !hasHost(parsed, "nats.example.com") {
		t.Fatalf("SANs = dns %v ips %v", parsed.DNSNames, parsed.IPAddresses)
	}

	if _, _, err := ensureServerCert(dir, hosts, discard); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(cert)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("certificate was regenerated")
	}

	_, _, err = ensureServerCert(dir, []string{"192.0.2.10", "192.0.2.11"}, discard)
	if err == nil || !strings.Contains(err.Error(), "IP 192.0.2.11") {
		t.Fatalf("error = %v, want it to mention the missing IP", err)
	}
	kept, err := os.ReadFile(cert)
	if err != nil {
		t.Fatal(err)
	}
	if string(kept) != string(first) {
		t.Fatal("a SAN mismatch replaced the certificate")
	}
}

func TestEmbeddedTLS(t *testing.T) {
	dir := t.TempDir()
	b, err := Start(config.NATS{
		Addr:     "127.0.0.1:0",
		TLSDir:   dir,
		TLSHosts: []string{"127.0.0.1"},
	}, nil, discard)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close(t.Context()) }()
	addr := b.server.Addr().String()

	plain, err := nats.Connect("nats://"+addr, nats.Timeout(2*time.Second), nats.MaxReconnects(0))
	if err == nil {
		plain.Close()
		t.Fatal("plain nats connected; tls should be required")
	}

	client, err := nats.Connect("tls://"+addr, nats.RootCAs(filepath.Join(dir, "ca.pem")), nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	echo(t, b, client)
}

func parseCert(t *testing.T, raw []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatal("not a PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}
