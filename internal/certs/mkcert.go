// Package certs wraps the `mkcert` CLI to generate locally-trusted TLS certs
// for the hostnames configured in kubetunnel.
package certs

import (
	"crypto/tls"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Ensure verifies mkcert is available on PATH. Returns a helpful error
// otherwise.
func Ensure() error {
	if _, err := exec.LookPath("mkcert"); err != nil {
		return fmt.Errorf("mkcert not found on PATH — install with `brew install mkcert`")
	}
	return nil
}

// Install runs `mkcert -install`, which installs the local CA in the system
// trust store (and Firefox if present).
func Install() error {
	if err := Ensure(); err != nil {
		return err
	}
	cmd := exec.Command("mkcert", "-install")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// EnsureCert makes sure a cert+key pair exists for the given hostname in
// certDir. If missing, it invokes mkcert to generate one.
// Returns (certPath, keyPath).
func EnsureCert(certDir, hostname string) (string, string, error) {
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir cert dir: %w", err)
	}
	certPath := filepath.Join(certDir, hostname+".pem")
	keyPath := filepath.Join(certDir, hostname+"-key.pem")
	if _, err := os.Stat(certPath); err == nil {
		if _, err := os.Stat(keyPath); err == nil {
			return certPath, keyPath, nil
		}
	}
	if err := Ensure(); err != nil {
		return "", "", err
	}
	cmd := exec.Command("mkcert",
		"-cert-file", certPath,
		"-key-file", keyPath,
		hostname,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("mkcert for %s: %w — output: %s", hostname, err, out)
	}
	return certPath, keyPath, nil
}

// Loader is a GetCertificate callback for tls.Config that serves a cert per
// SNI hostname.
type Loader struct {
	certs map[string]*tls.Certificate
}

func NewLoader() *Loader {
	return &Loader{certs: make(map[string]*tls.Certificate)}
}

// Add loads a cert for the given hostname.
func (l *Loader) Add(hostname, certPath, keyPath string) error {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return fmt.Errorf("load %s: %w", hostname, err)
	}
	l.certs[hostname] = &cert
	return nil
}

// GetCertificate implements the tls.Config.GetCertificate signature.
func (l *Loader) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if c, ok := l.certs[hello.ServerName]; ok {
		return c, nil
	}
	// Fallback: any cert (for health probes or non-SNI requests).
	for _, c := range l.certs {
		return c, nil
	}
	return nil, fmt.Errorf("no certificate available (SNI=%q)", hello.ServerName)
}
