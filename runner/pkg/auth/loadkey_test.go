package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func writePEM(t *testing.T, blockType string, der []byte) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "key.pem")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := pem.Encode(f, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadPrivateKey_PKCS1(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der := x509.MarshalPKCS1PrivateKey(key)
	path := writePEM(t, "RSA PRIVATE KEY", der)

	got, err := LoadPrivateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.N.Cmp(key.N) != 0 {
		t.Error("loaded key does not match")
	}
}

func TestLoadPrivateKey_PKCS8(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := writePEM(t, "PRIVATE KEY", der)

	got, err := LoadPrivateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.N.Cmp(key.N) != 0 {
		t.Error("loaded key does not match")
	}
}

func TestLoadPrivateKey_FileNotFound(t *testing.T) {
	if _, err := LoadPrivateKey("/no/such/file"); err == nil {
		t.Error("expected error for missing file")
	}
}

func TestLoadPrivateKey_NotPEM(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "notpem.pem")
	_ = os.WriteFile(p, []byte("not-a-pem-file"), 0o644)
	if _, err := LoadPrivateKey(p); err == nil {
		t.Error("expected error for non-PEM file")
	}
}

func TestLoadPrivateKey_UnsupportedBlockType(t *testing.T) {
	path := writePEM(t, "EC PRIVATE KEY", []byte("ignored"))
	if _, err := LoadPrivateKey(path); err == nil {
		t.Error("expected error for EC PRIVATE KEY block type")
	}
}
