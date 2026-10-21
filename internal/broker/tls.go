package broker

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/asilluron/lcars-api/internal/config"
)

const certLifetime = 10 * 365 * 24 * time.Hour

// prepareTLS returns the server certificate and key for the embedded NATS
// server, or empty paths when TLS is off. A generated certificate is reused
// until its directory is deleted, so devices can keep trusting the CA.
func prepareTLS(cfg config.NATS, logger *slog.Logger) (certFile, keyFile string, err error) {
	if cfg.TLSCert == "" && len(cfg.TLSHosts) == 0 {
		return "", "", nil
	}
	if cfg.TLSCert != "" {
		if err := certIncludes(cfg.TLSCert, cfg.TLSHosts); err != nil {
			return "", "", err
		}
		logger.Info("nats tls enabled", "cert", cfg.TLSCert, "hosts", strings.Join(cfg.TLSHosts, ","))
		return cfg.TLSCert, cfg.TLSKey, nil
	}
	certFile, keyFile, err = ensureServerCert(cfg.TLSDir, cfg.TLSHosts, logger)
	if err != nil {
		return "", "", err
	}
	logger.Info("nats tls enabled", "cert", certFile, "hosts", strings.Join(cfg.TLSHosts, ","), "ca", filepath.Join(cfg.TLSDir, "ca.pem"))
	return certFile, keyFile, nil
}

// ensureServerCert writes ca.pem, ca.key, server.pem and server.key into dir
// when they are absent. An existing certificate that does not list hosts is
// left in place and reported, so a restart cannot rotate a CA that devices
// already trust.
func ensureServerCert(dir string, hosts []string, logger *slog.Logger) (string, string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("create nats tls directory: %w", err)
	}
	certFile := filepath.Join(dir, "server.pem")
	keyFile := filepath.Join(dir, "server.key")
	caFile := filepath.Join(dir, "ca.pem")
	caKeyFile := filepath.Join(dir, "ca.key")
	files := []string{certFile, keyFile, caFile, caKeyFile}
	present := 0
	for _, path := range files {
		if _, err := os.Stat(path); err == nil {
			present++
		} else if !os.IsNotExist(err) {
			return "", "", fmt.Errorf("read nats tls file: %w", err)
		}
	}
	switch present {
	case 0:
		if err := writeCert(dir, hosts); err != nil {
			return "", "", err
		}
		logger.Info("nats tls certificate created", "dir", dir, "hosts", strings.Join(hosts, ","))
	case len(files):
		if err := certIncludes(certFile, hosts); err != nil {
			return "", "", err
		}
	default:
		return "", "", fmt.Errorf("nats tls directory %s is incomplete; delete it to mint a new CA", dir)
	}
	return certFile, keyFile, nil
}

func writeCert(dir string, hosts []string) error {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate nats tls key: %w", err)
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate nats tls key: %w", err)
	}
	notBefore := time.Now().Add(-time.Hour)
	notAfter := notBefore.Add(certLifetime)
	caTemplate := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "lcars-api"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, caKey.Public(), caKey)
	if err != nil {
		return fmt.Errorf("create nats tls CA: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return fmt.Errorf("parse nats tls CA: %w", err)
	}
	dns, ips := splitHosts(hosts)
	serverTemplate := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: hosts[0]},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dns,
		IPAddresses:  ips,
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, serverKey.Public(), caKey)
	if err != nil {
		return fmt.Errorf("create nats tls certificate: %w", err)
	}
	if err := writePEM(filepath.Join(dir, "ca.pem"), "CERTIFICATE", caDER, 0o644); err != nil {
		return err
	}
	if err := writeKey(filepath.Join(dir, "ca.key"), caKey); err != nil {
		return err
	}
	if err := writePEM(filepath.Join(dir, "server.pem"), "CERTIFICATE", serverDER, 0o644); err != nil {
		return err
	}
	return writeKey(filepath.Join(dir, "server.key"), serverKey)
}

func serial() *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	return n
}

func writeKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("encode nats tls key: %w", err)
	}
	return writePEM(path, "PRIVATE KEY", der, 0o600)
}

func writePEM(path, kind string, der []byte, mode os.FileMode) error {
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func certIncludes(path string, hosts []string) error {
	cert, err := readCert(path)
	if err != nil {
		return err
	}
	var missing []string
	for _, host := range hosts {
		if !hasHost(cert, host) {
			missing = append(missing, hostLabel(host))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("nats tls certificate %s is missing %s; delete the certificate directory to mint a new CA", path, strings.Join(missing, ", "))
}

func readCert(path string) (*x509.Certificate, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the operator chooses this path
	if err != nil {
		return nil, fmt.Errorf("read nats tls certificate: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("read nats tls certificate: %s is not a PEM certificate", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("read nats tls certificate: %w", err)
	}
	return cert, nil
}

func hasHost(cert *x509.Certificate, host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		for _, candidate := range cert.IPAddresses {
			if candidate.Equal(ip) {
				return true
			}
		}
		return false
	}
	for _, name := range cert.DNSNames {
		if strings.EqualFold(name, host) {
			return true
		}
	}
	return false
}

func hostLabel(host string) string {
	if net.ParseIP(host) != nil {
		return "IP " + host
	}
	return "DNS " + host
}

func splitHosts(hosts []string) (dns []string, ips []net.IP) {
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			ips = append(ips, ip)
			continue
		}
		dns = append(dns, host)
	}
	return dns, ips
}
