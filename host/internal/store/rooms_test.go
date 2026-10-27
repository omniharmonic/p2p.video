package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func copyFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "studiolink.db")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// testdata/pre_rooms.db was written by the store as it was before versioned
// migrations (user_version 0, no rooms table, no sessions.room_id).
func TestMigratesPreRoomsDatabaseWithDataIntact(t *testing.T) {
	path := copyFixture(t, "pre_rooms.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion() {
		t.Fatalf("user_version = %d (%v), want %d", version, err, SchemaVersion())
	}
	sess, err := s.GetSession("legacy01")
	if err != nil {
		t.Fatalf("legacy session lost: %v", err)
	}
	if sess.Title != "Episode 11" || sess.RoomID != "" || sess.EndedAt != nil || sess.AutoAdmit {
		t.Fatalf("legacy session changed: %+v", sess)
	}
	if got, err := s.GetSessionByTokenHash(sess.InviteTokenHash); err != nil || got.ID != "legacy01" {
		t.Fatalf("legacy invite lookup: %v %+v", err, got)
	}
	parts, err := s.ListParticipants("legacy01")
	if err != nil || len(parts) != 2 || parts[1].Name != "Ada" {
		t.Fatalf("participants: %v %+v", err, parts)
	}
	tk, err := s.GetTake("take_legacy01_01")
	if err != nil || tk.Status != "complete" {
		t.Fatalf("take: %v %+v", err, tk)
	}
	tracks, err := s.ListTracksByTake(tk.ID)
	if err != nil || len(tracks) != 1 || tracks[0].TotalBytes != 4096 {
		t.Fatalf("tracks: %v %+v", err, tracks)
	}
	if seqs, err := s.GetStoredChunkSeqs("trk_ada_v"); err != nil || len(seqs) != 1 {
		t.Fatalf("chunks: %v %v", err, seqs)
	}
	missing, err := s.ListMissingTakeParticipants(tk.ID)
	if err != nil || len(missing) != 0 {
		t.Fatalf("take roster: %v %+v", err, missing)
	}
	// The migrated schema accepts rooms and room-linked sessions.
	room := &Room{ID: "r1", Name: "The Podcast", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := s.CreateRoom(room); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(&Session{ID: "new1", Title: "x", CreatedAt: time.Now(), PresetJSON: "{}", InviteTokenHash: "h", RoomID: "r1"}); err != nil {
		t.Fatal(err)
	}
	recent, err := s.ListRecentSessions(10)
	if err != nil || len(recent) != 2 || recent[0].RoomName != "The Podcast" || len(recent[1].Takes) != 1 {
		t.Fatalf("recent: %v %+v", err, recent)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopening is a no-op migration.
	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	if got, _ := s.GetSession("new1"); got == nil || got.RoomID != "r1" {
		t.Fatalf("room link lost on reopen: %+v", got)
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA user_version = 999`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("opened a database from a newer p2p.video")
	}
}

func TestRoomCRUDAndLiveSession(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	a := &Room{ID: "ra", Name: "Weekly sync!", CreatedAt: now, UpdatedAt: now, InviteNonce: "n1", InviteSecretHash: "h1", Settings: RoomSettings{LockOnStart: true}}
	if err := s.CreateRoom(a); err != nil {
		t.Fatal(err)
	}
	b := &Room{ID: "rb", Name: "Weekly  sync", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateRoom(b); err != nil {
		t.Fatal(err)
	}
	if a.Slug != "weekly-sync" || b.Slug != "weekly-sync-2" {
		t.Fatalf("slugs %q %q", a.Slug, b.Slug)
	}
	got, err := s.GetRoomBySlug("weekly-sync")
	if err != nil || got.ID != "ra" || !got.Settings.LockOnStart || !got.LinkEnabled() {
		t.Fatalf("by slug: %v %+v", err, got)
	}
	got.Name = "Team sync"
	got.Settings.AutoAdmit = true
	if err := s.UpdateRoom(got); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetRoom("ra")
	if got.Name != "Team sync" || got.Slug != "weekly-sync" || !got.Settings.AutoAdmit {
		t.Fatalf("rename must keep the slug: %+v", got)
	}
	if err := s.SetRoomInvite("ra", "", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetRoom("ra"); got.LinkEnabled() {
		t.Fatal("revoked link still enabled")
	}
	b.Archived = true
	if err := s.UpdateRoom(b); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListRooms(false); len(list) != 1 || list[0].ID != "ra" {
		t.Fatalf("active rooms: %+v", list)
	}
	if list, _ := s.ListRooms(true); len(list) != 2 {
		t.Fatalf("all rooms: %+v", list)
	}

	if _, err := s.LiveRoomSession("ra"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("no meeting yet: %v", err)
	}
	if err := s.CreateSession(&Session{ID: "m1", Title: "Team sync", CreatedAt: now, PresetJSON: "{}", InviteTokenHash: "x1", RoomID: "ra"}); err != nil {
		t.Fatal(err)
	}
	live, err := s.LiveRoomSession("ra")
	if err != nil || live.ID != "m1" {
		t.Fatalf("live: %v %+v", err, live)
	}
	if err := s.EndSession("m1", now); err != nil {
		t.Fatal(err)
	}
	if err := s.EndSession("m1", now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second end: %v", err)
	}
	if _, err := s.LiveRoomSession("ra"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ended meeting still live: %v", err)
	}
	// Deleting the room keeps its past meeting, unlinked.
	if err := s.DeleteRoom("ra"); err != nil {
		t.Fatal(err)
	}
	m1, err := s.GetSession("m1")
	if err != nil || m1.RoomID != "" {
		t.Fatalf("past meeting after room delete: %v %+v", err, m1)
	}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"The Podcast":       "the-podcast",
		"  ¡Hola!  Café 2 ": "hola-caf-2",
		"🎙️":                "room",
		"a/b\\c?d=e&f":      "a-b-c-d-e-f",
	} {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Slugify("abcdefghijklmnopqrstuvwxyz abcdefghijklmnopqrstuvwxyz"); len(got) > 41 {
		t.Errorf("slug too long: %q", got)
	}
}
