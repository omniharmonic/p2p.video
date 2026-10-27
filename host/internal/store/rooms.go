package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// ErrSlugTaken is returned when no unique slug could be derived for a room.
var ErrSlugTaken = errors.New("room slug unavailable")

// Room is a persistent meeting place with a durable guest link [SL-P1-5].
// The guest link secret is never stored: only its SHA-256 (InviteSecretHash)
// and the random nonce the daemon derives it from (see httpserver/rooms.go).
type Room struct {
	ID               string       `json:"id"`
	Name             string       `json:"name"`
	Slug             string       `json:"slug"`
	CreatedAt        time.Time    `json:"createdAt"`
	UpdatedAt        time.Time    `json:"updatedAt"`
	InviteSecretHash string       `json:"-"`
	InviteNonce      string       `json:"-"`
	Settings         RoomSettings `json:"settings"`
	Archived         bool         `json:"archived"`
}

// LinkEnabled reports whether the room currently has a guest link.
func (r *Room) LinkEnabled() bool { return r.InviteSecretHash != "" && r.InviteNonce != "" }

// RoomSettings are defaults applied to every meeting opened from the room.
type RoomSettings struct {
	// AutoAdmit lets link holders straight into the call. Default off:
	// the guest link grants the waiting room only.
	AutoAdmit bool `json:"autoAdmit"`
	// LockOnStart opens each meeting already locked (no new join requests).
	LockOnStart bool `json:"lockOnStart"`
	// Recording is the default recording preset for new meetings; nil means
	// DefaultSessionSettings.
	Recording *SessionSettings `json:"recording,omitempty"`
	// HostName is shown to guests waiting for the host ("Waiting for Ben…").
	HostName string `json:"hostName,omitempty"`
}

const roomColumns = `id, name, slug, created_at, updated_at, invite_secret_hash, invite_nonce, settings_json, archived`

func scanRoom(row rowScanner) (*Room, error) {
	var r Room
	var created, updated, settings string
	var archived int
	if err := row.Scan(&r.ID, &r.Name, &r.Slug, &created, &updated, &r.InviteSecretHash, &r.InviteNonce, &settings, &archived); err != nil {
		return nil, err
	}
	r.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	r.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	r.Archived = archived == 1
	if settings != "" {
		if err := json.Unmarshal([]byte(settings), &r.Settings); err != nil {
			return nil, fmt.Errorf("decode room settings: %w", err)
		}
	}
	return &r, nil
}

// Slugify derives a URL-safe room slug from a display name.
func Slugify(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 40 {
			break
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		slug = "room"
	}
	return slug
}

// CreateRoom inserts a room, suffixing its slug (-2, -3, …) until unique.
// The slug is fixed at creation so renaming never breaks a guest link.
func (s *Store) CreateRoom(r *Room) error {
	settings, err := json.Marshal(r.Settings)
	if err != nil {
		return fmt.Errorf("encode room settings: %w", err)
	}
	base := r.Slug
	if base == "" {
		base = Slugify(r.Name)
	}
	for i := 1; i <= 50; i++ {
		slug := base
		if i > 1 {
			slug = fmt.Sprintf("%s-%d", base, i)
		}
		var exists int
		if err := s.db.QueryRow(`SELECT COUNT(1) FROM rooms WHERE slug = ?`, slug).Scan(&exists); err != nil {
			return fmt.Errorf("check room slug: %w", err)
		}
		if exists > 0 {
			continue
		}
		r.Slug = slug
		_, err = s.db.Exec(`INSERT INTO rooms (`+roomColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.Name, r.Slug, r.CreatedAt.Format(time.RFC3339Nano), r.UpdatedAt.Format(time.RFC3339Nano),
			r.InviteSecretHash, r.InviteNonce, string(settings), boolInt(r.Archived))
		if err != nil {
			return fmt.Errorf("insert room: %w", err)
		}
		return nil
	}
	return ErrSlugTaken
}

// GetRoom returns a room by ID (sql.ErrNoRows if absent).
func (s *Store) GetRoom(id string) (*Room, error) {
	return scanRoom(s.db.QueryRow(`SELECT `+roomColumns+` FROM rooms WHERE id = ?`, id))
}

// GetRoomBySlug returns a room by its guest-link slug.
func (s *Store) GetRoomBySlug(slug string) (*Room, error) {
	return scanRoom(s.db.QueryRow(`SELECT `+roomColumns+` FROM rooms WHERE slug = ?`, slug))
}

// ListRooms returns rooms by name; archived rooms only when asked.
func (s *Store) ListRooms(includeArchived bool) ([]Room, error) {
	q := `SELECT ` + roomColumns + ` FROM rooms`
	if !includeArchived {
		q += ` WHERE archived = 0`
	}
	rows, err := s.db.Query(q + ` ORDER BY name COLLATE NOCASE, created_at`)
	if err != nil {
		return nil, fmt.Errorf("list rooms: %w", err)
	}
	defer rows.Close()
	list := make([]Room, 0)
	for rows.Next() {
		r, err := scanRoom(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, *r)
	}
	return list, rows.Err()
}

// UpdateRoom persists name, settings and archived state.
func (s *Store) UpdateRoom(r *Room) error {
	settings, err := json.Marshal(r.Settings)
	if err != nil {
		return fmt.Errorf("encode room settings: %w", err)
	}
	r.UpdatedAt = time.Now().UTC()
	return affectOne(s.db.Exec(`UPDATE rooms SET name = ?, settings_json = ?, archived = ?, updated_at = ? WHERE id = ?`,
		r.Name, string(settings), boolInt(r.Archived), r.UpdatedAt.Format(time.RFC3339Nano), r.ID))
}

// SetRoomInvite replaces the guest-link nonce and secret hash. Empty values
// revoke the link; a new pair rotates it, invalidating the old link at once.
func (s *Store) SetRoomInvite(id, nonce, secretHash string) error {
	return affectOne(s.db.Exec(`UPDATE rooms SET invite_nonce = ?, invite_secret_hash = ?, updated_at = ? WHERE id = ?`,
		nonce, secretHash, time.Now().UTC().Format(time.RFC3339Nano), id))
}

// DeleteRoom removes a room, its producer link and its production
// settings (script, branding, logo). Past meetings keep their
// recordings and lose only the link to the room (sessions.room_id ON DELETE
// SET NULL).
func (s *Store) DeleteRoom(id string) error {
	if err := affectOne(s.db.Exec(`DELETE FROM rooms WHERE id = ?`, id)); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM producer_links WHERE scope = ?`, RoomProducerScope(id)); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM production WHERE scope = ?`, RoomProducerScope(id))
	return err
}

// Producer link scopes [SL-P1-11].
func SessionProducerScope(sessionID string) string { return "session:" + sessionID }
func RoomProducerScope(roomID string) string       { return "room:" + roomID }

// ProducerLink returns a scope's producer-link nonce and secret hash; both
// are empty when no link was ever issued or it was revoked.
func (s *Store) ProducerLink(scope string) (nonce, secretHash string, err error) {
	err = s.db.QueryRow(`SELECT nonce, secret_hash FROM producer_links WHERE scope = ?`, scope).Scan(&nonce, &secretHash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read producer link: %w", err)
	}
	return nonce, secretHash, nil
}

// SetProducerLink replaces a scope's producer link. Empty values revoke it;
// a new pair rotates it, invalidating the old link at once.
func (s *Store) SetProducerLink(scope, nonce, secretHash string) error {
	_, err := s.db.Exec(`INSERT INTO producer_links (scope, nonce, secret_hash, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(scope) DO UPDATE SET nonce = excluded.nonce, secret_hash = excluded.secret_hash, updated_at = excluded.updated_at`,
		scope, nonce, secretHash, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("write producer link: %w", err)
	}
	return nil
}

// LiveRoomSession returns the room's open meeting, or sql.ErrNoRows.
func (s *Store) LiveRoomSession(roomID string) (*Session, error) {
	return scanSession(s.db.QueryRow(`SELECT `+sessionColumns+` FROM sessions WHERE room_id = ? AND ended_at IS NULL ORDER BY created_at DESC LIMIT 1`, roomID))
}

// EndSession marks a session ended. Its invite and room link stop admitting
// anyone to it; recordings and uploads already granted are unaffected.
func (s *Store) EndSession(id string, at time.Time) error {
	return affectOne(s.db.Exec(`UPDATE sessions SET ended_at = ? WHERE id = ? AND ended_at IS NULL`, at.UTC().Format(time.RFC3339Nano), id))
}

// SessionSummary is a past or live meeting for the host home screen.
type SessionSummary struct {
	Session
	RoomName string `json:"roomName,omitempty"`
	Takes    []Take `json:"takes"`
}

// ListRecentSessions returns the newest sessions with their room name and
// takes. Record-link submissions [SL-P5-3] are not meetings and are listed
// under their link instead.
func (s *Store) ListRecentSessions(limit int) ([]SessionSummary, error) {
	rows, err := s.db.Query(`SELECT s.id, s.title, s.created_at, s.preset_json, '', s.auto_admit, s.ended_at, s.room_id, COALESCE(r.name, '')
		FROM sessions s LEFT JOIN rooms r ON r.id = s.room_id
		WHERE NOT EXISTS (SELECT 1 FROM record_submissions rs WHERE rs.session_id = s.id)
		ORDER BY s.created_at DESC, s.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list recent sessions: %w", err)
	}
	list := make([]SessionSummary, 0)
	for rows.Next() {
		var sum SessionSummary
		var created string
		var ended, roomID sql.NullString
		var autoAdmit int
		if err := rows.Scan(&sum.ID, &sum.Title, &created, &sum.PresetJSON, &sum.InviteTokenHash, &autoAdmit, &ended, &roomID, &sum.RoomName); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan recent session: %w", err)
		}
		sum.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		sum.AutoAdmit = autoAdmit == 1
		sum.RoomID = roomID.String
		if ended.Valid {
			t, _ := time.Parse(time.RFC3339Nano, ended.String)
			sum.EndedAt = &t
		}
		list = append(list, sum)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range list {
		takes, err := s.ListTakesBySession(list[i].ID)
		if err != nil {
			return nil, err
		}
		if takes == nil {
			takes = []Take{}
		}
		list[i].Takes = takes
	}
	return list, nil
}

func affectOne(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
