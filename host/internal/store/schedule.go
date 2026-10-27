package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Scheduled meeting states [SL-P6-1].
const (
	MeetingScheduled = "scheduled"
	MeetingCancelled = "cancelled"
)

// ScheduledMeeting is a meeting planned in a room: what invites and the
// calendar feed describe. Starting it opens the room's meeting as usual.
type ScheduledMeeting struct {
	ID          string    `json:"id"`
	RoomID      string    `json:"roomId"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	StartsAt    time.Time `json:"startsAt"`
	DurationMin int       `json:"durationMin"`
	// TimeZone is the IANA zone the host scheduled in (display only).
	TimeZone       string   `json:"timeZone"`
	GuestEmails    []string `json:"guestEmails"`
	OrganizerEmail string   `json:"organizerEmail,omitempty"`
	// UID is the stable iCalendar UID; Sequence is bumped on every change
	// guests should see (time, title, details, cancellation).
	UID       string    `json:"uid"`
	Sequence  int       `json:"sequence"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// EndsAt is the meeting's planned end.
func (m *ScheduledMeeting) EndsAt() time.Time {
	return m.StartsAt.Add(time.Duration(m.DurationMin) * time.Minute)
}

// Cancelled reports whether the meeting was cancelled.
func (m *ScheduledMeeting) Cancelled() bool { return m.Status == MeetingCancelled }

// startLayout is fixed-width so starts_at sorts and compares as text.
const startLayout = "2006-01-02T15:04:05Z"

// maxMeetingMinutes bounds how far back a "still running" meeting can start.
const maxMeetingMinutes = 12 * 60

const meetingColumns = `id, room_id, title, description, starts_at, duration_min, time_zone, guest_emails, organizer_email, uid, sequence, status, created_at, updated_at`

func scanMeeting(row rowScanner) (*ScheduledMeeting, error) {
	var m ScheduledMeeting
	var starts, created, updated, emails string
	if err := row.Scan(&m.ID, &m.RoomID, &m.Title, &m.Description, &starts, &m.DurationMin, &m.TimeZone, &emails, &m.OrganizerEmail, &m.UID, &m.Sequence, &m.Status, &created, &updated); err != nil {
		return nil, err
	}
	m.StartsAt, _ = time.Parse(startLayout, starts)
	m.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	m.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	m.GuestEmails = []string{}
	if emails != "" {
		if err := json.Unmarshal([]byte(emails), &m.GuestEmails); err != nil {
			return nil, fmt.Errorf("decode guest emails: %w", err)
		}
	}
	return &m, nil
}

func encodeEmails(list []string) (string, error) {
	if list == nil {
		list = []string{}
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return "", fmt.Errorf("encode guest emails: %w", err)
	}
	return string(raw), nil
}

// CreateScheduledMeeting inserts a meeting. StartsAt is stored in UTC to the second.
func (s *Store) CreateScheduledMeeting(m *ScheduledMeeting) error {
	emails, err := encodeEmails(m.GuestEmails)
	if err != nil {
		return err
	}
	if m.Status == "" {
		m.Status = MeetingScheduled
	}
	m.StartsAt = m.StartsAt.UTC().Truncate(time.Second)
	_, err = s.db.Exec(`INSERT INTO scheduled_meetings (`+meetingColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.RoomID, m.Title, m.Description, m.StartsAt.Format(startLayout), m.DurationMin, m.TimeZone, emails, m.OrganizerEmail,
		m.UID, m.Sequence, m.Status, m.CreatedAt.UTC().Format(time.RFC3339Nano), m.UpdatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("insert scheduled meeting: %w", err)
	}
	return nil
}

// GetScheduledMeeting returns a meeting by ID (sql.ErrNoRows if absent).
func (s *Store) GetScheduledMeeting(id string) (*ScheduledMeeting, error) {
	return scanMeeting(s.db.QueryRow(`SELECT `+meetingColumns+` FROM scheduled_meetings WHERE id = ?`, id))
}

// ListScheduledMeetings returns meetings still running or ahead at `since`
// (end after since), by start time. Cancelled ones only when asked.
func (s *Store) ListScheduledMeetings(since time.Time, includeCancelled bool) ([]ScheduledMeeting, error) {
	q := `SELECT ` + meetingColumns + ` FROM scheduled_meetings WHERE starts_at >= ?`
	if !includeCancelled {
		q += ` AND status = '` + MeetingScheduled + `'`
	}
	rows, err := s.db.Query(q+` ORDER BY starts_at, created_at`, since.UTC().Add(-maxMeetingMinutes*time.Minute).Format(startLayout))
	if err != nil {
		return nil, fmt.Errorf("list scheduled meetings: %w", err)
	}
	defer rows.Close()
	list := make([]ScheduledMeeting, 0)
	for rows.Next() {
		m, err := scanMeeting(rows)
		if err != nil {
			return nil, err
		}
		if m.EndsAt().After(since) {
			list = append(list, *m)
		}
	}
	return list, rows.Err()
}

// UpdateScheduledMeeting persists every mutable field, including sequence
// and status; the caller decides when a change bumps the sequence.
func (s *Store) UpdateScheduledMeeting(m *ScheduledMeeting) error {
	emails, err := encodeEmails(m.GuestEmails)
	if err != nil {
		return err
	}
	m.StartsAt = m.StartsAt.UTC().Truncate(time.Second)
	m.UpdatedAt = time.Now().UTC()
	return affectOne(s.db.Exec(`UPDATE scheduled_meetings SET room_id = ?, title = ?, description = ?, starts_at = ?, duration_min = ?, time_zone = ?,
		guest_emails = ?, organizer_email = ?, sequence = ?, status = ?, updated_at = ? WHERE id = ?`,
		m.RoomID, m.Title, m.Description, m.StartsAt.Format(startLayout), m.DurationMin, m.TimeZone, emails, m.OrganizerEmail,
		m.Sequence, m.Status, m.UpdatedAt.Format(time.RFC3339Nano), m.ID))
}

// DeleteScheduledMeeting removes a meeting for good.
func (s *Store) DeleteScheduledMeeting(id string) error {
	return affectOne(s.db.Exec(`DELETE FROM scheduled_meetings WHERE id = ?`, id))
}

// CalendarFeed returns the host calendar feed's nonce and secret hash; both
// are empty when the feed was never turned on or was turned off.
func (s *Store) CalendarFeed() (nonce, secretHash string, err error) {
	err = s.db.QueryRow(`SELECT nonce, secret_hash FROM calendar_feed WHERE id = 1`).Scan(&nonce, &secretHash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read calendar feed: %w", err)
	}
	return nonce, secretHash, nil
}

// SetCalendarFeed replaces the feed secret (empty values turn the feed off;
// a new pair rotates it and the old address stops working at once).
func (s *Store) SetCalendarFeed(nonce, secretHash string) error {
	_, err := s.db.Exec(`INSERT INTO calendar_feed (id, nonce, secret_hash, updated_at) VALUES (1, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET nonce = excluded.nonce, secret_hash = excluded.secret_hash, updated_at = excluded.updated_at`,
		nonce, secretHash, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("write calendar feed: %w", err)
	}
	return nil
}
