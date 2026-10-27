package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestGetenvPrefersP2PVideoAlias(t *testing.T) {
	t.Setenv("P2PVIDEO_DATA_DIR", "")
	t.Setenv("STUDIOLINK_DATA_DIR", "/legacy")
	if got := Getenv("DATA_DIR"); got != "/legacy" {
		t.Fatalf("legacy prefix: got %q", got)
	}
	t.Setenv("P2PVIDEO_DATA_DIR", "/new")
	if got := Getenv("DATA_DIR"); got != "/new" {
		t.Fatalf("P2PVIDEO_ must win: got %q", got)
	}
}

func TestSecretFailureDoesNotRotateIdentity(t *testing.T) {
	t.Setenv("P2PVIDEO_SECRET_KEY", "")
	t.Setenv("STUDIOLINK_SECRET_KEY", "")
	dir := t.TempDir()
	p := filepath.Join(dir, ".secret_key")
	if err := os.WriteFile(p, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if key := LoadOrCreateSecretKey(dir); len(key) != 0 {
		t.Fatal("corrupt identity silently replaced with a new secret")
	}
	data, _ := os.ReadFile(p)
	if string(data) != "corrupt" {
		t.Fatal("corrupt identity overwritten")
	}
	if key := LoadOrCreateSecretKey(filepath.Join(p, "not-a-directory")); len(key) != 0 {
		t.Fatal("unpersisted identity returned as usable")
	}
}
func TestSecretPersists(t *testing.T) {
	t.Setenv("STUDIOLINK_SECRET_KEY", "")
	dir := t.TempDir()
	first := LoadOrCreateSecretKey(dir)
	second := LoadOrCreateSecretKey(dir)
	if len(first) != 32 || !bytes.Equal(first, second) {
		t.Fatal("identity did not persist")
	}
}
