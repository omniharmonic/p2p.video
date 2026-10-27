package store

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeV4Fixture builds testdata/pre_recordlinks.db: a schema-version-4
// database (rooms, producer links, production, lower thirds) holding a room,
// a finished meeting with a take, a track and a chunk. Regenerate with
// STUDIOLINK_WRITE_FIXTURES=1 go test ./internal/store -run TestWriteV4Fixture.
func TestWriteV4Fixture(t *testing.T) {
	if os.Getenv("STUDIOLINK_WRITE_FIXTURES") != "1" {
		t.Skip("fixture generator")
	}
	path := filepath.Join(t.TempDir(), "v4.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 25, 18, 0, 0, 0, time.UTC)
	for _, err := range []error{
		s.CreateRoom(&Room{ID: "room_v4", Name: "Weekly Show", CreatedAt: now, UpdatedAt: now, InviteNonce: "n", InviteSecretHash: "h"}),
		s.CreateSession(&Session{ID: "sess_v4", Title: "Episode 42", CreatedAt: now, PresetJSON: `{"preset":"1080p30"}`, InviteTokenHash: "inv", RoomID: "room_v4"}),
		s.CreateParticipant(&Participant{ID: "p_v4", SessionID: "sess_v4", Name: "Grace", Role: "guest", Status: "admitted", JoinedAt: now}),
		s.CreateSource(&Source{ID: "src_v4", ParticipantID: "p_v4", Kind: "camera", Label: "Camera"}),
		s.CreateTakeWithParticipants(&Take{ID: "take_v4_01", SessionID: "sess_v4", Number: 1, StartAtHostUs: 1, StopAtHostUs: 2, WallClockStart: now, Status: "complete"}, []string{"p_v4"}),
		s.CreateTrack(&Track{ID: "trk_v4", TakeID: "take_v4_01", SourceID: "src_v4", Kind: "video", Mime: "video/webm", LastSeq: 0, TotalBytes: 2048, Status: "assembled"}),
		s.StoreChunk(&Chunk{TrackID: "trk_v4", Seq: 0, Size: 2048, SHA256: "abc", ReceivedAt: now}),
		s.SetScript(RoomProducerScope("room_v4"), "# Cold open", "everyone"),
		s.EndSession("sess_v4", now.Add(time.Hour)),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{`DROP INDEX record_submissions_link`, `DROP TABLE record_submissions`, `DROP TABLE record_links`, `PRAGMA user_version = 4`, `PRAGMA journal_mode = DELETE`, `VACUUM`} {
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
	out, err := os.Create(filepath.Join("testdata", "pre_recordlinks.db"))
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

// Migration 5 [SL-P5-3] upgrades a real version-4 database file: its rooms,
// meetings, takes, tracks, chunks and scripts survive, record links and
// submissions work, and submissions stay out of the recent meetings.
func TestMigration5AddsRecordLinksToVersion4Fixture(t *testing.T) {
	path := copyFixture(t, "pre_recordlinks.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open v4 fixture: %v", err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion() || version < 5 {
		t.Fatalf("user_version %d (%v), want %d", version, err, SchemaVersion())
	}
	sess, err := s.GetSession("sess_v4")
	if err != nil || sess.Title != "Episode 42" || sess.RoomID != "room_v4" || sess.EndedAt == nil {
		t.Fatalf("meeting lost: %v %+v", err, sess)
	}
	if tracks, err := s.ListTracksByTake("take_v4_01"); err != nil || len(tracks) != 1 || tracks[0].TotalBytes != 2048 {
		t.Fatalf("tracks: %v %+v", err, tracks)
	}
	if p, err := s.GetProduction(RoomProducerScope("room_v4")); err != nil || p.Script != "# Cold open" {
		t.Fatalf("script lost: %v %+v", err, p)
	}

	now := time.Now().UTC()
	link := &RecordLink{ID: "rl_1", Title: "Intro", Prompt: "Say hi", RoomID: "room_v4", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
		MaxDurationSec: 60, MaxSubmissions: 1, MaxTotalBytes: 1 << 20, Nonce: "nonce", SecretHash: "hash", Recording: DefaultSessionSettings()}
	if err := s.CreateRecordLink(link); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRecordLink("rl_1")
	if err != nil || got.Title != "Intro" || got.RoomID != "room_v4" || got.Recording.VideoBitrateBps != DefaultSessionSettings().VideoBitrateBps || !got.LinkEnabled() || got.Expired(now) {
		t.Fatalf("record link: %v %+v", err, got)
	}
	newSub := func(id string) {
		t.Helper()
		if err := s.CreateSubmission(&Session{ID: id, Title: "Intro — " + id, CreatedAt: now, PresetJSON: "{}", InviteTokenHash: "x" + id},
			&Participant{ID: "p_" + id, SessionID: id, Name: id, Role: "guest", Status: "admitted", JoinedAt: now},
			&RecordSubmission{SessionID: id, LinkID: "rl_1", ParticipantID: "p_" + id, Name: id, KeyHash: "k", Status: SubmissionDraft, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	newSub("subA")
	newSub("subB")
	if !s.IsSubmissionSession("subA") || s.IsSubmissionSession("sess_v4") {
		t.Fatal("submission detection")
	}
	if n, err := s.NextSubmissionTake("subA"); err != nil || n != 1 {
		t.Fatalf("take number %d %v", n, err)
	}
	if err := s.CreateTakeWithParticipants(&Take{ID: "take_subA_01", SessionID: "subA", Number: 1, WallClockStart: now, Status: "recording"}, []string{"p_subA"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSource(&Source{ID: "src_a", ParticipantID: "p_subA", Kind: "camera"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTrack(&Track{ID: "trk_a", TakeID: "take_subA_01", SourceID: "src_a", Kind: "video", Mime: "video/webm", Status: "recording"}); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreChunk(&Chunk{TrackID: "trk_a", Seq: 0, Size: 700, SHA256: "x", ReceivedAt: now}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.LinkStoredBytes("rl_1"); err != nil || n != 700 {
		t.Fatalf("link bytes %d %v", n, err)
	}
	if l, err := s.SubmissionLink("take_subA_01"); err == nil {
		t.Fatalf("take id resolved as a session: %+v", l)
	}
	if l, err := s.SubmissionLink("subA"); err != nil || l.ID != "rl_1" {
		t.Fatalf("submission link %v %+v", err, l)
	}
	if takes, err := s.ListRecordingSoloTakes(); err != nil || len(takes) != 1 || takes[0].MaxDurationSec != 60 {
		t.Fatalf("recording solo takes %v %+v", err, takes)
	}
	// Submitting is capped atomically: one of two.
	if err := s.SubmitSubmission("subA", "rl_1", 1, now); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitSubmission("subB", "rl_1", 1, now); !errors.Is(err, ErrSubmissionsFull) {
		t.Fatalf("second submit: %v", err)
	}
	if sub, drafts, err := s.CountSubmissions("rl_1", now.Add(-time.Hour)); err != nil || sub != 1 || drafts != 1 {
		t.Fatalf("counts %d %d %v", sub, drafts, err)
	}
	recent, err := s.ListRecentSessions(10)
	if err != nil || len(recent) != 1 || recent[0].ID != "sess_v4" {
		t.Fatalf("recent meetings include submissions: %v %+v", err, recent)
	}
	if err := s.DeleteRecordLink("rl_1"); !errors.Is(err, ErrLinkHasSubmissions) {
		t.Fatalf("delete link with submissions: %v", err)
	}
	// Discarding a take removes its rows; deleting a submission removes all.
	if err := s.DeleteTake("take_subA_01"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSource("src_a"); err == nil {
		t.Fatal("discarded take's source remains")
	}
	for _, id := range []string{"subA", "subB"} {
		if err := s.DeleteSubmissionSession(id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.GetParticipant("p_subA"); err == nil {
		t.Fatal("deleted submission's participant remains")
	}
	if err := s.DeleteRecordLink("rl_1"); err != nil {
		t.Fatal(err)
	}
	// Deleting the room keeps nothing dangling: the link's room is cleared.
	if err := s.CreateRecordLink(link); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRoom("room_v4"); err != nil {
		t.Fatal(err)
	}
	if l, err := s.GetRecordLink("rl_1"); err != nil || l.RoomID != "" {
		t.Fatalf("room delete: %v %+v", err, l)
	}
}
