package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDesktopTokenPersistsWithOwnerOnlyMode(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreateDesktopToken(dir)
	if err != nil || len(first) != 64 {
		t.Fatalf("token length %d err %v", len(first), err)
	}
	second, err := LoadOrCreateDesktopToken(dir)
	if err != nil || second != first {
		t.Fatalf("token not stable (%v)", err)
	}
	info, err := os.Stat(filepath.Join(dir, DesktopTokenFile))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("token mode %v err %v", info.Mode(), err)
	}
}

func TestDesktopTokenRejectsTruncatedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, DesktopTokenFile), []byte("short"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateDesktopToken(dir); err == nil {
		t.Fatal("truncated token accepted")
	}
}
