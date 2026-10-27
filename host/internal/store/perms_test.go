package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestL6DatabaseIsOwnerOnly: a new database (and its WAL journal) is 0600 in
// a 0700 directory, and an existing world-readable one is tightened [SL-SEC-1].
func TestL6DatabaseIsOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	db := filepath.Join(dir, "studiolink.db")
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(&Session{ID: "s", Title: "t", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	check := func(path string, want os.FileMode) {
		t.Helper()
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode %v, want %v", filepath.Base(path), info.Mode().Perm(), want)
		}
	}
	check(dir, 0o700)
	check(db, 0o600)
	check(db+"-wal", 0o600)
	check(db+"-shm", 0o600)
	_ = s.Close()

	if err := os.Chmod(db, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err = Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check(db, 0o600)
}
