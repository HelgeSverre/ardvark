// Command gencerts generates a throwaway CA and a server leaf certificate for
// the distributed-crawling smoke test (tools/smoketest). Probing is https-only
// by policy (internal/probe builds https:// URLs directly), so the fixture web
// server must serve real TLS that the ardvark worker containers trust. This
// tool produces:
//
//	certs/ca.crt      — the throwaway CA cert, installed into the ardvark image's
//	                    system trust store at build time (update-ca-certificates).
//	certs/server.crt  — the fixture server leaf cert, SAN-covering *.test plus
//	                    site1.test .. site20.test explicitly.
//	certs/server.key  — the fixture server private key.
//
// Everything here is DISPOSABLE TEST FIXTURE material: the CA and key are
// regenerated on every run and are gitignored (never committed). Generating the
// pair in Go rather than shelling out to openssl keeps the harness reproducible
// across hosts (macOS ships LibreSSL, whose -addext handling differs) and needs
// no external tooling.
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gencerts:", err)
		os.Exit(1)
	}
}

func run() error {
	outDir := "certs"
	if len(os.Args) > 1 {
		outDir = os.Args[1]
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	// -- CA ----------------------------------------------------------------
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("generating CA key: %w", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ardvark smoketest throwaway CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("creating CA cert: %w", err)
	}

	// -- Server leaf -------------------------------------------------------
	srvKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("generating server key: %w", err)
	}
	dnsNames := []string{"*.test", "localhost", "fixture"}
	for i := 1; i <= 20; i++ {
		dnsNames = append(dnsNames, fmt.Sprintf("site%d.test", i))
	}
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "ardvark smoketest fixture"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return fmt.Errorf("parsing CA cert: %w", err)
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("creating server cert: %w", err)
	}

	if err := writePEM(filepath.Join(outDir, "ca.crt"), "CERTIFICATE", caDER); err != nil {
		return err
	}
	if err := writePEM(filepath.Join(outDir, "server.crt"), "CERTIFICATE", srvDER); err != nil {
		return err
	}
	if err := writePEM(filepath.Join(outDir, "server.key"), "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(srvKey)); err != nil {
		return err
	}

	fmt.Printf("gencerts: wrote ca.crt, server.crt, server.key to %s/ (SANs: *.test, site1.test..site20.test)\n", outDir)
	return nil
}

func writePEM(path, typ string, der []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return pem.Encode(f, &pem.Block{Type: typ, Bytes: der})
}
