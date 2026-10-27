package store

import (
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWriteV5Fixture builds testdata/pre_calendar.db: a schema-version-5
// database (record links included) holding a room, a finished meeting with a
// take, a record link and a submission. Regenerate with
// STUDIOLINK_WRITE_FIXTURES=1 go test ./internal/store -run TestWriteV5Fixture.
func TestWriteV5Fixture(t *testing.T) {
	if os.Getenv("STUDIOLINK_WRITE_FIXTURES") != "1" {
		t.Skip("fixture generator")
	}
	path := filepath.Join(t.TempDir(), "v5.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	for _, err := range []error{
		s.CreateRoom(&Room{ID: "room_v5", Name: "Weekly Show", CreatedAt: now, UpdatedAt: now, InviteNonce: "n5", InviteSecretHash: "h5"}),
		s.CreateSession(&Session{ID: "sess_v5", Title: "Episode 43", CreatedAt: now, PresetJSON: `{"preset":"1080p30"}`, InviteTokenHash: "inv5", RoomID: "room_v5"}),
		s.CreateParticipant(&Participant{ID: "p_v5", SessionID: "sess_v5", Name: "Ada", Role: "guest", Status: "admitted", JoinedAt: now}),
		s.CreateTakeWithParticipants(&Take{ID: "take_v5_01", SessionID: "sess_v5", Number: 1, StartAtHostUs: 1, StopAtHostUs: 2, WallClockStart: now, Status: "complete"}, []string{"p_v5"}),
		s.EndSession("sess_v5", now.Add(time.Hour)),
		s.CreateRecordLink(&RecordLink{ID: "rl_v5", Title: "Intro", RoomID: "room_v5", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(48 * time.Hour),
			MaxDurationSec: 60, MaxSubmissions: 2, MaxTotalBytes: 1 << 20, Nonce: "rn", SecretHash: "rh", Recording: DefaultSessionSettings()}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{`DROP INDEX scheduled_meetings_start`, `DROP INDEX scheduled_meetings_room`, `DROP TABLE scheduled_meetings`, `DROP TABLE calendar_feed`,
		`PRAGMA user_version = 5`, `PRAGMA journal_mode = DELETE`, `VACUUM`} {
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
	out, err := os.Create(filepath.Join("testdata", "pre_calendar.db"))
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

// Migration 6 [SL-P6-1] upgrades a real version-5 database file: rooms,
// meetings, takes and record links survive, and scheduling works.
func TestMigration6AddsSchedulingToVersion5Fixture(t *testing.T) {
	s, err := Open(copyFixture(t, "pre_calendar.db"))
	if err != nil {
		t.Fatalf("open v5 fixture: %v", err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion() || version < 6 {
		t.Fatalf("user_version %d (%v), want %d", version, err, SchemaVersion())
	}
	if sess, err := s.GetSession("sess_v5"); err != nil || sess.Title != "Episode 43" || sess.RoomID != "room_v5" || sess.EndedAt == nil {
		t.Fatalf("meeting lost: %v %+v", err, sess)
	}
	if tk, err := s.GetTake("take_v5_01"); err != nil || tk.Status != "complete" {
		t.Fatalf("take lost: %v", err)
	}
	if l, err := s.GetRecordLink("rl_v5"); err != nil || l.Title != "Intro" || l.RoomID != "room_v5" {
		t.Fatalf("record link lost: %v %+v", err, l)
	}
	if n, h, err := s.CalendarFeed(); err != nil || n != "" || h != "" {
		t.Fatalf("feed starts off: %q %q %v", n, h, err)
	}
	now := time.Now().UTC()
	m := &ScheduledMeeting{ID: "mtg_1", RoomID: "room_v5", Title: "Episode 44", StartsAt: now.Add(24 * time.Hour), DurationMin: 60, TimeZone: "Europe/Berlin",
		GuestEmails: []string{"ada@example.com"}, UID: "mtg_1@studiolink", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateScheduledMeeting(m); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetScheduledMeeting("mtg_1")
	if err != nil || got.Title != "Episode 44" || got.Status != MeetingScheduled || len(got.GuestEmails) != 1 || got.TimeZone != "Europe/Berlin" || !got.StartsAt.Equal(m.StartsAt) {
		t.Fatalf("meeting: %v %+v", err, got)
	}
}

func TestScheduledMeetingsListUpdateCancelAndCascade(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	if err := s.CreateRoom(&Room{ID: "r1", Name: "Show", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRoom(&Room{ID: "r2", Name: "Other", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	add := func(id, room string, start time.Time, minutes int) {
		t.Helper()
		if err := s.CreateScheduledMeeting(&ScheduledMeeting{ID: id, RoomID: room, Title: id, StartsAt: start, DurationMin: minutes, UID: id + "@studiolink", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	add("past", "r1", now.Add(-3*time.Hour), 60)       // ended 2 h ago
	add("running", "r1", now.Add(-30*time.Minute), 60) // still on
	add("later", "r1", now.Add(48*time.Hour), 30)      //
	add("soon", "r2", now.Add(2*time.Hour+500), 45)    // sub-second start is truncated
	add("marathon", "r2", now.Add(-11*time.Hour), 720) // 12 h, still running
	list, err := s.ListScheduledMeetings(now, false)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range list {
		ids = append(ids, m.ID)
	}
	if want := []string{"marathon", "running", "soon", "later"}; len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] || ids[3] != want[3] {
		t.Fatalf("upcoming = %v, want %v", ids, want)
	}

	m, _ := s.GetScheduledMeeting("later")
	m.Status, m.Sequence, m.GuestEmails = MeetingCancelled, m.Sequence+1, []string{"x@example.com"}
	if err := s.UpdateScheduledMeeting(m); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetScheduledMeeting("later"); !got.Cancelled() || got.Sequence != 1 || got.GuestEmails[0] != "x@example.com" {
		t.Fatalf("update: %+v", got)
	}
	if list, _ := s.ListScheduledMeetings(now, false); len(list) != 3 {
		t.Fatalf("cancelled hidden by default: %d", len(list))
	}
	if list, _ := s.ListScheduledMeetings(now, true); len(list) != 4 {
		t.Fatalf("cancelled listed on request: %d", len(list))
	}

	// Deleting a room deletes its scheduled meetings.
	if err := s.DeleteRoom("r2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetScheduledMeeting("soon"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("room delete must cascade: %v", err)
	}
	if err := s.DeleteScheduledMeeting("running"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteScheduledMeeting("running"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("double delete: %v", err)
	}

	// The feed secret rotates and turns off.
	if err := s.SetCalendarFeed("n1", "h1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCalendarFeed("n2", "h2"); err != nil {
		t.Fatal(err)
	}
	if n, h, _ := s.CalendarFeed(); n != "n2" || h != "h2" {
		t.Fatalf("feed %q %q", n, h)
	}
	if err := s.SetCalendarFeed("", ""); err != nil {
		t.Fatal(err)
	}
	if n, h, _ := s.CalendarFeed(); n != "" || h != "" {
		t.Fatal("feed off")
	}
}
