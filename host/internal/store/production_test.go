package store

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"
)

// Migration 4 [SL-P5-1, SL-P5-2] upgrades a version-3 database in place:
// existing rooms, sessions and participants survive, and the production
// and lower_thirds tables appear.
func TestMigration4AddsProductionToVersion3Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.CreateRoom(&Room{ID: "r1", Name: "Show", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(&Session{ID: "s1", Title: "Ep 1", CreatedAt: now, RoomID: "r1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateParticipant(&Participant{ID: "p1", SessionID: "s1", Name: "Ana", Role: "guest", Status: "admitted", JoinedAt: now}); err != nil {
		t.Fatal(err)
	}
	// Roll the database back to what version 3 shipped.
	for _, q := range []string{`DROP TABLE production`, `DROP TABLE lower_thirds`, `PRAGMA user_version = 3`} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen v3 database: %v", err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion() || version < 4 {
		t.Fatalf("user_version %d (%v), want %d", version, err, SchemaVersion())
	}
	if p, err := s.GetParticipant("p1"); err != nil || p.Name != "Ana" {
		t.Fatalf("participant lost: %v %+v", err, p)
	}
	sess, err := s.GetSession("s1")
	if err != nil || sess.RoomID != "r1" {
		t.Fatalf("session lost: %v %+v", err, sess)
	}
	scope := ProductionScope(sess)
	if scope != "room:r1" {
		t.Fatalf("scope %q", scope)
	}
	if err := s.SetScript(scope, "# Intro\nHello", "everyone"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBrand(scope, Brand{ShowName: "The Show", Accent: "#ff8800", TileLabels: true}); err != nil {
		t.Fatal(err)
	}
	logo := []byte("\x89PNG\r\n\x1a\nfake")
	if err := s.SetLogo(scope, logo, "image/png", "abc"); err != nil {
		t.Fatal(err)
	}
	p, err := s.GetProduction(scope)
	if err != nil || p.Script != "# Intro\nHello" || p.ScriptVisibility != "everyone" || p.Brand.ShowName != "The Show" || !p.Brand.TileLabels || p.LogoType != "image/png" || p.LogoSize != len(logo) {
		t.Fatalf("production: %v %+v", err, p)
	}
	data, typ, sha, err := s.Logo(scope)
	if err != nil || !bytes.Equal(data, logo) || typ != "image/png" || sha != "abc" {
		t.Fatalf("logo: %v %q %q", err, typ, sha)
	}
	if err := s.SetLogo(scope, nil, "", ""); err != nil {
		t.Fatal(err)
	}
	if data, _, _, _ := s.Logo(scope); data != nil {
		t.Fatal("logo not removed")
	}
	// A scope without a row has defaults, not an error.
	if p, err := s.GetProduction("session:none"); err != nil || p.Script != "" || p.LogoType != "" {
		t.Fatalf("empty scope: %v %+v", err, p)
	}

	if err := s.SetLowerThird("s1", LowerThird{ParticipantID: "p1", Title: "Founder, X"}); err != nil {
		t.Fatal(err)
	}
	lts, err := s.ListLowerThirds("s1")
	if err != nil || lts["p1"].Title != "Founder, X" {
		t.Fatalf("lower thirds: %v %+v", err, lts)
	}
	if err := s.SetLowerThird("s1", LowerThird{ParticipantID: "p1"}); err != nil {
		t.Fatal(err)
	}
	if lts, _ := s.ListLowerThirds("s1"); len(lts) != 0 {
		t.Fatalf("cleared lower third remains: %+v", lts)
	}

	// Deleting the room deletes its production settings.
	if err := s.DeleteRoom("r1"); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.GetProduction(scope); p.Script != "" || p.Brand.ShowName != "" {
		t.Fatalf("room production survived delete: %+v", p)
	}
}
