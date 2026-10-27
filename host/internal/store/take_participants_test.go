package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestTakeParticipantSnapshotSchema(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM take_participants`).Scan(&count); err != nil {
		t.Fatalf("expected participants cannot be persisted: %v", err)
	}
}

func TestExpectedParticipantsSurviveDisconnectAndRequireAssembly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	for _, id := range []string{"room", "other"} {
		if err := s.CreateSession(&Session{ID: id, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []Participant{{ID: "alice", SessionID: "room", JoinedAt: time.Now(), Status: "admitted"}, {ID: "bob", SessionID: "room", JoinedAt: time.Now(), Status: "admitted"}, {ID: "outsider", SessionID: "other", JoinedAt: time.Now(), Status: "admitted"}} {
		if err := s.CreateParticipant(&p); err != nil {
			t.Fatal(err)
		}
	}
	take := &Take{ID: "take", SessionID: "room", WallClockStart: time.Now(), Status: "recording"}
	if err := s.CreateTakeWithParticipants(take, []string{"alice", "bob", "bob"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ExpectTakeParticipant(take.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.ExpectTakeParticipant(take.ID, "outsider"); err == nil {
		t.Fatal("cross-room expected recorder accepted")
	}
	missing, err := s.ListMissingTakeParticipants(take.ID)
	if err != nil || len(missing) != 2 {
		t.Fatalf("no-track recorders=%+v error=%v", missing, err)
	}
	if err := s.CreateSource(&Source{ID: "mic", ParticipantID: "alice", Kind: "mic"}); err != nil {
		t.Fatal(err)
	}
	track := &Track{ID: "audio", SourceID: "mic", TakeID: take.ID, Kind: "audio", Mime: "audio/wav", Status: "uploading"}
	if err := s.CreateTrack(track); err != nil {
		t.Fatal(err)
	}
	missing, err = s.ListMissingTakeParticipants(take.ID)
	if err != nil || len(missing) != 2 {
		t.Fatalf("uploading recorder incorrectly ready: %+v %v", missing, err)
	}
	track.Status = "assembled"
	if err := s.UpdateTrack(track); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateParticipantStatus("bob", "disconnected"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	missing, err = s.ListMissingTakeParticipants(take.ID)
	if err != nil || len(missing) != 1 || missing[0].ID != "bob" {
		t.Fatalf("durable missing recorder=%+v error=%v", missing, err)
	}
	bad := &Take{ID: "rollback", SessionID: "room", WallClockStart: time.Now(), Status: "recording"}
	if err := s.CreateTakeWithParticipants(bad, []string{"alice", "outsider"}); err == nil {
		t.Fatal("bad snapshot committed")
	}
	if _, err := s.GetTake(bad.ID); err == nil {
		t.Fatal("failed snapshot left a take without expectations")
	}
}
