package store

import (
	"path/filepath"
	"testing"
	"time"
)

// A screen share is an optional extra source: it may be registered late in a
// take and must never stand in for the participant's camera/mic recording.
func TestScreenTrackNeitherSatisfiesNorBlocksExpectedRecorder(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateSession(&Session{ID: "room", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateParticipant(&Participant{ID: "alice", SessionID: "room", JoinedAt: time.Now(), Status: "admitted"}); err != nil {
		t.Fatal(err)
	}
	take := &Take{ID: "take", SessionID: "room", WallClockStart: time.Now(), Status: "recording"}
	if err := s.CreateTakeWithParticipants(take, []string{"alice"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSource(&Source{ID: "screen", ParticipantID: "alice", Kind: "screen"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTrack(&Track{ID: "screen-track", SourceID: "screen", TakeID: take.ID, Kind: "video", Mime: "video/webm", Status: "assembled"}); err != nil {
		t.Fatal(err)
	}
	missing, err := s.ListMissingTakeParticipants(take.ID)
	if err != nil || len(missing) != 1 || missing[0].ID != "alice" {
		t.Fatalf("screen-only recorder counted as complete: %+v %v", missing, err)
	}
	if err := s.CreateSource(&Source{ID: "cam", ParticipantID: "alice", Kind: "camera"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTrack(&Track{ID: "cam-track", SourceID: "cam", TakeID: take.ID, Kind: "video", Mime: "video/webm", Status: "assembled"}); err != nil {
		t.Fatal(err)
	}
	missing, err = s.ListMissingTakeParticipants(take.ID)
	if err != nil || len(missing) != 0 {
		t.Fatalf("camera + late screen not complete: %+v %v", missing, err)
	}
}
