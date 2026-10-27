package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
)

// livestreamTLS returns the TLS config for rtmps:// destinations. With no
// test CA it is nil: Go's default, which on macOS verifies through the system
// trust store (Security framework) with hostname checks, exactly as a
// production ingest is verified. A test CA (end-to-end tests only) is added on
// top of the system roots; it never replaces them, and it is announced in the
// log so it cannot be left on silently.
func livestreamTLS(caFile string) (*tls.Config, error) {
	if caFile == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read test CA: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("test CA file holds no PEM certificate")
	}
	slog.Warn("Livestream trusts an extra TEST certificate authority for rtmps:// (--livestream-test-ca); never use this in production", "file", caFile)
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}
