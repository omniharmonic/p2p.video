package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLaunchTokenIsOwnerOnlyAndPerBoot: the launch token file is 0600 and a
// new daemon run replaces it (M4).
func TestLaunchTokenIsOwnerOnlyAndPerBoot(t *testing.T) {
	dir := t.TempDir()
	first, err := WriteLaunchToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, LaunchTokenFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("launch token mode %v, want 0600", info.Mode().Perm())
	}
	if got, err := ReadLaunchToken(dir); err != nil || got != first {
		t.Fatalf("read back %q, %v", got, err)
	}
	second, err := WriteLaunchToken(dir)
	if err != nil || second == first || len(second) != 64 {
		t.Fatalf("second boot token %q (first %q), %v", second, first, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}
