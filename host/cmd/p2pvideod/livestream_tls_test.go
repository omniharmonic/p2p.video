package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Production keeps Go's default (system trust store); a test CA only adds a
// root and must be a real PEM certificate [SL-P3-3].
func TestLivestreamTLS(t *testing.T) {
	if cfg, err := livestreamTLS(""); cfg != nil || err != nil {
		t.Fatalf("default must be nil (system trust store): %v %v", cfg, err)
	}
	dir := t.TempDir()
	if _, err := livestreamTLS(filepath.Join(dir, "missing.pem")); err == nil {
		t.Fatal("missing file accepted")
	}
	junk := filepath.Join(dir, "junk.pem")
	_ = os.WriteFile(junk, []byte("not a certificate"), 0o600)
	if _, err := livestreamTLS(junk); err == nil {
		t.Fatal("non-PEM accepted")
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "ca.pem")
	_ = os.WriteFile(good, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	cfg, err := livestreamTLS(good)
	if err != nil || cfg == nil || cfg.RootCAs == nil || cfg.InsecureSkipVerify {
		t.Fatalf("test CA config %+v %v", cfg, err)
	}
}
