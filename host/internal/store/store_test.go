package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStoreCRUD(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer s.Close()

	// 1. Session
	sess := &Session{
		ID:              "sess_123",
		Title:           "Episode 1",
		CreatedAt:       time.Now().UTC(),
		PresetJSON:      `{"preset":"1080p30"}`,
		InviteTokenHash: "hash_xyz_123",
		AutoAdmit:       true,
	}
	if err := s.CreateSession(sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	gotSess, err := s.GetSession("sess_123")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if gotSess.Title != "Episode 1" || !gotSess.AutoAdmit {
		t.Errorf("Unexpected session: %+v", gotSess)
	}

	gotByHash, err := s.GetSessionByTokenHash("hash_xyz_123")
	if err != nil || gotByHash.ID != "sess_123" {
		t.Fatalf("GetSessionByTokenHash failed: %v, got: %+v", err, gotByHash)
	}

	// 2. Participant
	p := &Participant{
		ID:        "part_ben",
		SessionID: sess.ID,
		Name:      "Ben",
		Role:      "host",
		Status:    "admitted",
		JoinedAt:  time.Now().UTC(),
		UserAgent: "Mozilla/5.0 Mac",
	}
	if err := s.CreateParticipant(p); err != nil {
		t.Fatalf("CreateParticipant: %v", err)
	}

	gotP, err := s.GetParticipant("part_ben")
	if err != nil || gotP.Name != "Ben" {
		t.Fatalf("GetParticipant: %v", err)
	}

	// 3. Source
	src := &Source{
		ID:            "src_cam1",
		ParticipantID: p.ID,
		Kind:          "camera",
		Label:         "FaceTime HD Camera",
	}
	if err := s.CreateSource(src); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}

	sources, err := s.ListSourcesByParticipant(p.ID)
	if err != nil || len(sources) != 1 {
		t.Fatalf("ListSourcesByParticipant: %v, len=%d", err, len(sources))
	}

	// 4. Take
	tk := &Take{
		ID:             "take_01",
		SessionID:      sess.ID,
		Number:         1,
		StartAtHostUs:  1000000,
		StopAtHostUs:   0,
		WallClockStart: time.Now().UTC(),
		Status:         "recording",
	}
	if err := s.CreateTake(tk); err != nil {
		t.Fatalf("CreateTake: %v", err)
	}

	// 5. Track
	tr := &Track{
		ID:         "trk_video1",
		TakeID:     tk.ID,
		SourceID:   src.ID,
		Kind:       "video",
		Mime:       "video/webm",
		Codec:      "vp9",
		LastSeq:    -1,
		TotalBytes: 0,
		Status:     "recording",
	}
	if err := s.CreateTrack(tr); err != nil {
		t.Fatalf("CreateTrack: %v", err)
	}

	// 6. Chunks
	c1 := &Chunk{
		TrackID:    tr.ID,
		Seq:        0,
		Size:       1024,
		SHA256:     "sha_0",
		ReceivedAt: time.Now().UTC(),
	}
	c2 := &Chunk{
		TrackID:    tr.ID,
		Seq:        1,
		Size:       2048,
		SHA256:     "sha_1",
		ReceivedAt: time.Now().UTC(),
	}
	if err := s.StoreChunk(c1); err != nil {
		t.Fatalf("StoreChunk 0: %v", err)
	}
	if err := s.StoreChunk(c2); err != nil {
		t.Fatalf("StoreChunk 1: %v", err)
	}

	exists, sha, err := s.CheckChunkExists(tr.ID, 0)
	if err != nil || !exists || sha != "sha_0" {
		t.Errorf("CheckChunkExists: exists=%v, sha=%s, err=%v", exists, sha, err)
	}

	seqs, err := s.GetStoredChunkSeqs(tr.ID)
	if err != nil || len(seqs) != 2 || seqs[0] != 0 || seqs[1] != 1 {
		t.Errorf("GetStoredChunkSeqs unexpected: %v, seqs=%v", err, seqs)
	}
}
