package store

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWriteV6Fixture builds testdata/pre_roomtitles.db: a schema-version-6
// database (scheduled meetings included) holding a room, a finished meeting
// with a participant's lower third, and a record link with a draft
// submission. It was first written by the version-6 store itself (before
// migration 7 existed). Regenerate with
// STUDIOLINK_WRITE_FIXTURES=1 go test ./internal/store -run TestWriteV6Fixture.
func TestWriteV6Fixture(t *testing.T) {
	if os.Getenv("STUDIOLINK_WRITE_FIXTURES") != "1" {
		t.Skip("fixture generator")
	}
	path := filepath.Join(t.TempDir(), "v6.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 26, 15, 0, 0, 0, time.UTC)
	for _, err := range []error{
		s.CreateRoom(&Room{ID: "room_v6", Name: "Weekly Show", CreatedAt: now, UpdatedAt: now, InviteNonce: "n6", InviteSecretHash: "h6"}),
		s.CreateSession(&Session{ID: "sess_v6", Title: "Episode 44", CreatedAt: now, PresetJSON: `{"preset":"1080p30"}`, InviteTokenHash: "inv6", RoomID: "room_v6"}),
		s.CreateParticipant(&Participant{ID: "p_v6", SessionID: "sess_v6", Name: "Ada Lovelace", Role: "guest", Status: "admitted", JoinedAt: now}),
		s.SetLowerThird("sess_v6", LowerThird{ParticipantID: "p_v6", Title: "Analyst"}),
		s.CreateTakeWithParticipants(&Take{ID: "take_v6_01", SessionID: "sess_v6", Number: 1, StartAtHostUs: 1, StopAtHostUs: 2, WallClockStart: now, Status: "complete"}, []string{"p_v6"}),
		s.EndSession("sess_v6", now.Add(time.Hour)),
		s.CreateRecordLink(&RecordLink{ID: "rl_v6", Title: "Intro", RoomID: "room_v6", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(48 * time.Hour),
			MaxDurationSec: 60, MaxSubmissions: 2, MaxTotalBytes: 1 << 20, Nonce: "rn6", SecretHash: "rh6", Recording: DefaultSessionSettings()}),
		s.CreateSubmission(&Session{ID: "sub_v6", Title: "Intro — Bo", CreatedAt: now, PresetJSON: "{}", InviteTokenHash: "x"},
			&Participant{ID: "p_sub_v6", SessionID: "sub_v6", Name: "Bo", Role: "guest", Status: "admitted", JoinedAt: now},
			&RecordSubmission{SessionID: "sub_v6", LinkID: "rl_v6", ParticipantID: "p_sub_v6", Name: "Bo", KeyHash: "k", Status: SubmissionDraft, CreatedAt: now}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{`DROP TABLE IF EXISTS room_lower_thirds`, `PRAGMA user_version = 6`, `PRAGMA journal_mode = DELETE`, `VACUUM`} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	s.Close()
	in, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(filepath.Join("testdata", "pre_roomtitles.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// Migration 7 [SL-P5-4] upgrades a real version-6 database file: rooms,
// meetings, lower thirds, takes, record links and draft submissions
// survive, and rooms can remember lower thirds.
func TestMigration7AddsRoomLowerThirdsToVersion6Fixture(t *testing.T) {
	s, err := Open(copyFixture(t, "pre_roomtitles.db"))
	if err != nil {
		t.Fatalf("open v6 fixture: %v", err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion() || version < 7 {
		t.Fatalf("user_version %d (%v), want %d", version, err, SchemaVersion())
	}
	if sess, err := s.GetSession("sess_v6"); err != nil || sess.RoomID != "room_v6" || sess.EndedAt == nil {
		t.Fatalf("meeting lost: %v %+v", err, sess)
	}
	if lts, err := s.ListLowerThirds("sess_v6"); err != nil || lts["p_v6"].Title != "Analyst" {
		t.Fatalf("lower third lost: %v %+v", err, lts)
	}
	if tk, err := s.GetTake("take_v6_01"); err != nil || tk.Status != "complete" {
		t.Fatalf("take lost: %v", err)
	}
	if sub, err := s.GetSubmission("sub_v6"); err != nil || sub.Status != SubmissionDraft || sub.LinkID != "rl_v6" {
		t.Fatalf("draft submission lost: %v %+v", err, sub)
	}
	if list, err := s.ListRoomLowerThirds("room_v6"); err != nil || len(list) != 0 {
		t.Fatalf("room titles start empty: %v %+v", err, list)
	}
	if err := s.RememberRoomLowerThird("room_v6", "Ada Lovelace", "", "Analyst"); err != nil {
		t.Fatal(err)
	}
	if rem, err := s.RoomLowerThirdFor("room_v6", "ADA  lovelace"); err != nil || rem.Title != "Analyst" || rem.DisplayName != "Ada Lovelace" {
		t.Fatalf("remembered: %v %+v", err, rem)
	}
}

func TestRoomLowerThirdsRememberApplyForgetAndCascade(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	for _, err := range []error{
		s.CreateRoom(&Room{ID: "r1", Name: "Show", CreatedAt: now, UpdatedAt: now}),
		s.CreateRoom(&Room{ID: "r2", Name: "Other", CreatedAt: now, UpdatedAt: now}),
		s.CreateSession(&Session{ID: "ep", RoomID: "r1", CreatedAt: now}),
		s.CreateParticipant(&Participant{ID: "p1", SessionID: "ep", Name: "Ana Ruiz", Role: "guest", Status: "admitted", JoinedAt: now}),
		s.CreateParticipant(&Participant{ID: "p2", SessionID: "ep", Name: "Bo", Role: "guest", Status: "admitted", JoinedAt: now}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if LowerThirdKey("  Ana\tRUIZ ") != "ana ruiz" || LowerThirdKey("   ") != "" {
		t.Fatal("key normalization")
	}
	if err := s.RememberRoomLowerThird("r1", "Ana Ruiz", "Dr. Ana Ruiz", "Founder"); err != nil {
		t.Fatal(err)
	}
	if err := s.RememberRoomLowerThird("r1", "   ", "x", "y"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListRoomLowerThirds("r1"); len(list) != 1 {
		t.Fatalf("blank display name stored: %+v", list)
	}
	if _, err := s.RoomLowerThirdFor("r2", "Ana Ruiz"); err == nil {
		t.Fatal("memory leaked into another room")
	}
	// Apply copies it once; an existing meeting lower third wins.
	if ok, err := s.ApplyRoomLowerThird("r1", "ep", "p1", "ana ruiz"); err != nil || !ok {
		t.Fatalf("apply: %v %v", ok, err)
	}
	if ok, _ := s.ApplyRoomLowerThird("r1", "ep", "p2", "Bo"); ok {
		t.Fatal("applied to someone without memory")
	}
	if err := s.SetLowerThird("ep", LowerThird{ParticipantID: "p1", Title: "Edited"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.ApplyRoomLowerThird("r1", "ep", "p1", "Ana Ruiz"); ok {
		t.Fatal("apply overwrote the meeting's lower third")
	}
	if lts, _ := s.ListLowerThirds("ep"); lts["p1"].Title != "Edited" {
		t.Fatalf("meeting lower third %+v", lts["p1"])
	}
	// Both empty forgets; Forget by key too.
	if err := s.RememberRoomLowerThird("r1", "ana ruiz", "", ""); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListRoomLowerThirds("r1"); len(list) != 0 {
		t.Fatalf("not forgotten: %+v", list)
	}
	_ = s.RememberRoomLowerThird("r1", "Bo", "", "Producer")
	if err := s.ForgetRoomLowerThird("r1", "bo"); err != nil {
		t.Fatal(err)
	}
	_ = s.RememberRoomLowerThird("r1", "Cy", "", "Editor")
	if err := s.DeleteRoom("r1"); err != nil {
		t.Fatalf("room delete: %v", err)
	}
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(1) FROM room_lower_thirds WHERE room_id = 'r1'`).Scan(&n)
	if n != 0 {
		t.Fatalf("room titles survive the room: %d", n)
	}
}
