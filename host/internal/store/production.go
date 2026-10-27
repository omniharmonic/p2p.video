package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Production extras [SL-P5-1, SL-P5-2]: teleprompter script, branding and
// lower thirds. A meeting opened from a room uses the room's scope, so the
// script, brand and logo carry over from episode to episode; an instant
// meeting uses its own session scope.

// ProductionScope is the scope a session's production settings live under.
func ProductionScope(sess *Session) string {
	if sess.RoomID != "" {
		return RoomProducerScope(sess.RoomID)
	}
	return SessionProducerScope(sess.ID)
}

// Brand is a room's (or instant meeting's) look for tiles and the livestream.
type Brand struct {
	// ShowName is drawn in the branded livestream layout's top band.
	ShowName string `json:"showName"`
	// Accent is a #rrggbb colour (lower-third strip, branded background).
	Accent string `json:"accent"`
	// TileLabels shows the polished name + title labels on call tiles.
	TileLabels bool `json:"tileLabels"`
	// LowerThirds is when the livestream shows lower thirds: "speaker"
	// (when someone becomes the active speaker, and on demand), "manual"
	// (on demand only) or "off". Empty means "speaker".
	LowerThirds string `json:"lowerThirds"`
}

// Production is one scope's stored production settings.
type Production struct {
	Scope            string
	Script           string
	ScriptVisibility string
	Brand            Brand
	LogoType         string
	LogoSHA          string
	LogoSize         int
	UpdatedAt        time.Time
}

// GetProduction returns scope's settings; a scope without a row returns
// zero values (no script, default brand, no logo).
func (s *Store) GetProduction(scope string) (*Production, error) {
	p := &Production{Scope: scope}
	var brand, updated string
	var size sql.NullInt64
	err := s.db.QueryRow(`SELECT script, script_visibility, brand_json, logo_type, logo_sha, length(logo), updated_at FROM production WHERE scope = ?`, scope).
		Scan(&p.Script, &p.ScriptVisibility, &brand, &p.LogoType, &p.LogoSHA, &size, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read production settings: %w", err)
	}
	if brand != "" {
		if err := json.Unmarshal([]byte(brand), &p.Brand); err != nil {
			return nil, fmt.Errorf("decode brand: %w", err)
		}
	}
	p.LogoSize = int(size.Int64)
	p.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return p, nil
}

func (s *Store) upsertProduction(scope, set string, args ...any) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`INSERT INTO production (scope, updated_at) VALUES (?, ?) ON CONFLICT(scope) DO NOTHING`, scope, now); err != nil {
		return fmt.Errorf("create production row: %w", err)
	}
	args = append(args, now, scope)
	if _, err := s.db.Exec(`UPDATE production SET `+set+`, updated_at = ? WHERE scope = ?`, args...); err != nil {
		return fmt.Errorf("update production settings: %w", err)
	}
	return nil
}

// SetScript stores scope's teleprompter script and visibility.
func (s *Store) SetScript(scope, script, visibility string) error {
	return s.upsertProduction(scope, `script = ?, script_visibility = ?`, script, visibility)
}

// SetBrand stores scope's brand settings.
func (s *Store) SetBrand(scope string, b Brand) error {
	raw, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("encode brand: %w", err)
	}
	return s.upsertProduction(scope, `brand_json = ?`, string(raw))
}

// SetLogo stores a validated logo (the caller checks type and size); empty
// data removes it.
func (s *Store) SetLogo(scope string, data []byte, contentType, sha string) error {
	if len(data) == 0 {
		return s.upsertProduction(scope, `logo = NULL, logo_type = '', logo_sha = ''`)
	}
	return s.upsertProduction(scope, `logo = ?, logo_type = ?, logo_sha = ?`, data, contentType, sha)
}

// Logo returns scope's logo bytes and content type (nil when none).
func (s *Store) Logo(scope string) (data []byte, contentType, sha string, err error) {
	err = s.db.QueryRow(`SELECT logo, logo_type, logo_sha FROM production WHERE scope = ?`, scope).Scan(&data, &contentType, &sha)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", "", nil
	}
	if err != nil {
		return nil, "", "", fmt.Errorf("read logo: %w", err)
	}
	return data, contentType, sha, nil
}

// LowerThird is one participant's lower-third text. Empty Name means the
// participant's display name.
type LowerThird struct {
	ParticipantID string `json:"participantId"`
	Name          string `json:"name,omitempty"`
	Title         string `json:"title,omitempty"`
}

// SetLowerThird stores a participant's lower third (both empty clears it).
func (s *Store) SetLowerThird(sessionID string, lt LowerThird) error {
	if lt.Name == "" && lt.Title == "" {
		_, err := s.db.Exec(`DELETE FROM lower_thirds WHERE participant_id = ?`, lt.ParticipantID)
		if err != nil {
			return fmt.Errorf("clear lower third: %w", err)
		}
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO lower_thirds (participant_id, session_id, name, title, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(participant_id) DO UPDATE SET name = excluded.name, title = excluded.title, updated_at = excluded.updated_at`,
		lt.ParticipantID, sessionID, lt.Name, lt.Title, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("write lower third: %w", err)
	}
	return nil
}

// ListLowerThirds returns a session's lower thirds by participant ID.
func (s *Store) ListLowerThirds(sessionID string) (map[string]LowerThird, error) {
	rows, err := s.db.Query(`SELECT participant_id, name, title FROM lower_thirds WHERE session_id = ?`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list lower thirds: %w", err)
	}
	defer rows.Close()
	out := map[string]LowerThird{}
	for rows.Next() {
		var lt LowerThird
		if err := rows.Scan(&lt.ParticipantID, &lt.Name, &lt.Title); err != nil {
			return nil, fmt.Errorf("scan lower third: %w", err)
		}
		out[lt.ParticipantID] = lt
	}
	return out, rows.Err()
}

// RoomLowerThird is a lower third a room remembers for a person across
// episodes [SL-P5-4], keyed by their normalized display name (see
// LowerThirdKey). Empty Name means the person's display name.
type RoomLowerThird struct {
	Key         string    `json:"key"`
	DisplayName string    `json:"displayName"`
	Name        string    `json:"name,omitempty"`
	Title       string    `json:"title,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// LowerThirdKey normalizes a display name for room memory: case-folded,
// with surrounding and repeated whitespace removed. Two people with the same
// display name in one room share a remembered lower third.
func LowerThirdKey(displayName string) string {
	return strings.ToLower(strings.Join(strings.Fields(displayName), " "))
}

// RememberRoomLowerThird stores (or, with both name and title empty,
// forgets) the lower third a room remembers for displayName.
func (s *Store) RememberRoomLowerThird(roomID, displayName, name, title string) error {
	key := LowerThirdKey(displayName)
	if key == "" {
		return nil
	}
	if name == "" && title == "" {
		return s.ForgetRoomLowerThird(roomID, key)
	}
	_, err := s.db.Exec(`INSERT INTO room_lower_thirds (room_id, name_key, display_name, name, title, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(room_id, name_key) DO UPDATE SET display_name = excluded.display_name, name = excluded.name, title = excluded.title, updated_at = excluded.updated_at`,
		roomID, key, strings.Join(strings.Fields(displayName), " "), name, title, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("remember room lower third: %w", err)
	}
	return nil
}

// ForgetRoomLowerThird removes a remembered lower third by key (no error if
// there is none).
func (s *Store) ForgetRoomLowerThird(roomID, key string) error {
	if _, err := s.db.Exec(`DELETE FROM room_lower_thirds WHERE room_id = ? AND name_key = ?`, roomID, key); err != nil {
		return fmt.Errorf("forget room lower third: %w", err)
	}
	return nil
}

// RoomLowerThirdFor returns the lower third a room remembers for
// displayName (sql.ErrNoRows if none).
func (s *Store) RoomLowerThirdFor(roomID, displayName string) (*RoomLowerThird, error) {
	return scanRoomLowerThird(s.db.QueryRow(`SELECT name_key, display_name, name, title, updated_at FROM room_lower_thirds WHERE room_id = ? AND name_key = ?`,
		roomID, LowerThirdKey(displayName)))
}

func scanRoomLowerThird(row rowScanner) (*RoomLowerThird, error) {
	var lt RoomLowerThird
	var updated string
	if err := row.Scan(&lt.Key, &lt.DisplayName, &lt.Name, &lt.Title, &updated); err != nil {
		return nil, err
	}
	lt.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return &lt, nil
}

// ListRoomLowerThirds returns a room's remembered lower thirds, by key.
func (s *Store) ListRoomLowerThirds(roomID string) ([]RoomLowerThird, error) {
	rows, err := s.db.Query(`SELECT name_key, display_name, name, title, updated_at FROM room_lower_thirds WHERE room_id = ? ORDER BY name_key`, roomID)
	if err != nil {
		return nil, fmt.Errorf("list room lower thirds: %w", err)
	}
	defer rows.Close()
	out := make([]RoomLowerThird, 0)
	for rows.Next() {
		lt, err := scanRoomLowerThird(rows)
		if err != nil {
			return nil, fmt.Errorf("scan room lower third: %w", err)
		}
		out = append(out, *lt)
	}
	return out, rows.Err()
}

// ApplyRoomLowerThird gives a participant of a room meeting the lower third
// the room remembers for their display name, unless the meeting already has
// one for them. It reports whether it set one.
func (s *Store) ApplyRoomLowerThird(roomID, sessionID, participantID, displayName string) (bool, error) {
	rem, err := s.RoomLowerThirdFor(roomID, displayName)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read room lower third: %w", err)
	}
	res, err := s.db.Exec(`INSERT INTO lower_thirds (participant_id, session_id, name, title, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(participant_id) DO NOTHING`, participantID, sessionID, rem.Name, rem.Title, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, fmt.Errorf("apply room lower third: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
