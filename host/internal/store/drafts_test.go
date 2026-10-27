package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// Draft activity and the guarded delete [SL-P5-4]: activity is the latest of
// creation, take start and chunk upload (parsed, whatever the zone); a
// submitted, recording or recently active draft is never deleted; a delete
// removes every row of the draft, clock samples included.
func TestDraftActivityAndDeleteIdleDraft(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	day := 24 * time.Hour
	if err := s.CreateRecordLink(&RecordLink{ID: "rl", Title: "Intro", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(day),
		MaxDurationSec: 60, MaxSubmissions: 5, MaxTotalBytes: 1 << 30, Nonce: "n", SecretHash: "h", Recording: DefaultSessionSettings()}); err != nil {
		t.Fatal(err)
	}
	draft := func(id string, created time.Time, status string) {
		t.Helper()
		if err := s.CreateSubmission(&Session{ID: id, Title: id, CreatedAt: created, PresetJSON: "{}", InviteTokenHash: "x"},
			&Participant{ID: "p_" + id, SessionID: id, Name: id, Role: "guest", Status: "admitted", JoinedAt: created},
			&RecordSubmission{SessionID: id, LinkID: "rl", ParticipantID: "p_" + id, Name: id, KeyHash: "k", Status: SubmissionDraft, CreatedAt: created}); err != nil {
			t.Fatal(err)
		}
		if status == SubmissionSubmitted {
			if err := s.SubmitSubmission(id, "rl", 5, created); err != nil {
				t.Fatal(err)
			}
		}
	}
	take := func(sess string, n int, start time.Time, status string, chunkAt time.Time, size int64) {
		t.Helper()
		tk := &Take{ID: sess + "_t" + string(rune('0'+n)), SessionID: sess, Number: n, WallClockStart: start, Status: status}
		if err := s.CreateTakeWithParticipants(tk, []string{"p_" + sess}); err != nil {
			t.Fatal(err)
		}
		if chunkAt.IsZero() {
			return
		}
		src := &Source{ID: tk.ID + "_src", ParticipantID: "p_" + sess, Kind: "camera", Label: "Camera"}
		tr := &Track{ID: tk.ID + "_trk", TakeID: tk.ID, SourceID: src.ID, Kind: "video", Mime: "video/webm", LastSeq: -1, Status: "uploading"}
		for _, err := range []error{s.CreateSource(src), s.CreateTrack(tr), s.StoreChunk(&Chunk{TrackID: tr.ID, Seq: 0, Size: size, SHA256: "x", ReceivedAt: chunkAt})} {
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	old := now.Add(-3 * day)
	draft("idle", old, SubmissionDraft)
	take("idle", 1, old, "stopped", old.Add(time.Minute).In(time.FixedZone("PDT", -7*3600)), 1000) // local-time row
	if err := s.SaveClockSample("p_idle", 1, 2, 3, 4); err != nil {
		t.Fatal(err)
	}
	draft("uploading", old, SubmissionDraft)
	take("uploading", 1, old, "stopped", now.Add(-5*time.Minute), 500) // upload resumed 5 min ago
	draft("recording", old, SubmissionDraft)
	take("recording", 1, old, "recording", time.Time{}, 0)
	draft("sent", old, SubmissionSubmitted)
	take("sent", 1, old, "stopped", old, 10)

	list, err := s.ListDraftActivity(now)
	if err != nil || len(list) != 3 {
		t.Fatalf("drafts %v %+v", err, list)
	}
	byID := map[string]DraftActivity{}
	for _, d := range list {
		byID[d.SessionID] = d
	}
	if d := byID["idle"]; d.Bytes != 1000 || d.Takes != 1 || !d.LastActivity.Equal(old.Add(time.Minute)) || d.LinkClosed || d.LinkTitle != "Intro" {
		t.Fatalf("idle %+v", d)
	}
	if list[0].SessionID != "idle" && list[0].SessionID != "recording" {
		t.Fatalf("not oldest first: %+v", list)
	}
	if !byID["recording"].Recording || byID["uploading"].LastUpload.Before(now.Add(-6*time.Minute)) {
		t.Fatalf("activity %+v", byID)
	}

	cutoff := now.Add(-time.Hour)
	for id, want := range map[string]error{"uploading": ErrDraftBusy, "recording": ErrDraftBusy, "sent": ErrDraftBusy} {
		if _, err := s.DeleteIdleDraft(id, cutoff); !errors.Is(err, want) {
			t.Errorf("delete %s: %v, want %v", id, err, want)
		}
	}
	if sub, err := s.GetSubmission("sent"); err != nil || sub.Status != SubmissionSubmitted {
		t.Fatal("submitted recording touched")
	}
	d, err := s.DeleteIdleDraft("idle", cutoff)
	if err != nil || d.Bytes != 1000 {
		t.Fatalf("delete idle: %v %+v", err, d)
	}
	var rows int
	for _, q := range []string{
		`SELECT COUNT(1) FROM sessions WHERE id = 'idle'`, `SELECT COUNT(1) FROM participants WHERE session_id = 'idle'`,
		`SELECT COUNT(1) FROM takes WHERE session_id = 'idle'`, `SELECT COUNT(1) FROM tracks WHERE take_id LIKE 'idle%'`,
		`SELECT COUNT(1) FROM chunks WHERE track_id LIKE 'idle%'`, `SELECT COUNT(1) FROM sources WHERE id LIKE 'idle%'`,
		`SELECT COUNT(1) FROM clock_samples WHERE participant_id = 'p_idle'`, `SELECT COUNT(1) FROM record_submissions WHERE session_id = 'idle'`,
	} {
		if err := s.db.QueryRow(q).Scan(&rows); err != nil || rows != 0 {
			t.Errorf("%s = %d (%v)", q, rows, err)
		}
	}
	if _, err := s.DeleteIdleDraft("idle", cutoff); err == nil {
		t.Fatal("deleted twice")
	}
	// A turned-off link marks its drafts closed.
	if err := s.SetRecordLinkSecret("rl", "", ""); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListDraftActivity(now); len(list) == 0 || !list[0].LinkClosed {
		t.Fatalf("closed link %+v", list)
	}
}
