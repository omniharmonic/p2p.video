package store

import (
	"database/sql"
	"fmt"
	"time"
)

// migrations are applied in order; the database's PRAGMA user_version is the
// number already applied. Data directories created before versioning are at
// 0 and already hold the baseline schema, which migration 1 recreates
// idempotently. Append new steps; never edit or reorder shipped ones.
var migrations = []func(tx *sql.Tx) error{
	// 1: the pre-versioning schema.
	func(tx *sql.Tx) error {
		_, err := tx.Exec(baselineSchema)
		return err
	},
	// 2: persistent rooms [SL-P1-5]. A room is a durable guest link; each
	// meeting opened from it is an ordinary session with room_id set.
	func(tx *sql.Tx) error {
		if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS rooms (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			slug TEXT NOT NULL UNIQUE,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			invite_secret_hash TEXT NOT NULL DEFAULT '',
			invite_nonce TEXT NOT NULL DEFAULT '',
			settings_json TEXT NOT NULL DEFAULT '{}',
			archived INTEGER NOT NULL DEFAULT 0
		)`); err != nil {
			return err
		}
		has, err := hasColumn(tx, "sessions", "room_id")
		if err != nil {
			return err
		}
		if !has {
			if _, err := tx.Exec(`ALTER TABLE sessions ADD COLUMN room_id TEXT REFERENCES rooms(id) ON DELETE SET NULL`); err != nil {
				return err
			}
		}
		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS sessions_room_id ON sessions(room_id, ended_at)`)
		return err
	},
	// 3: producer links [SL-P1-11]. One rotatable link per instant meeting
	// ("session:<id>") or room ("room:<id>"), stored like a room's guest
	// link: the random nonce and SHA-256 of the derived secret only.
	func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS producer_links (
			scope TEXT PRIMARY KEY,
			nonce TEXT NOT NULL DEFAULT '',
			secret_hash TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL
		)`)
		return err
	},
	// 4: production extras [SL-P5-1, SL-P5-2]. One row per room
	// ("room:<id>") or instant meeting ("session:<id>"): the teleprompter
	// script (rooms only; an instant meeting keeps its script in memory),
	// brand settings and the logo (PNG/JPEG bytes, validated on upload).
	// Lower-third name/title per participant, exported with each take.
	func(tx *sql.Tx) error {
		if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS production (
			scope TEXT PRIMARY KEY,
			script TEXT NOT NULL DEFAULT '',
			script_visibility TEXT NOT NULL DEFAULT '',
			brand_json TEXT NOT NULL DEFAULT '{}',
			logo BLOB,
			logo_type TEXT NOT NULL DEFAULT '',
			logo_sha TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL
		)`); err != nil {
			return err
		}
		_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS lower_thirds (
			participant_id TEXT PRIMARY KEY REFERENCES participants(id) ON DELETE CASCADE,
			session_id TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL
		)`)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS lower_thirds_session ON lower_thirds(session_id)`)
		return err
	},
	// 5: async record links [SL-P5-3]. A record link is a standalone (or
	// room-attached) "record solo" invitation: its secret is stored like a
	// room link (nonce + SHA-256 only). Each guest visit is a submission: an
	// ordinary session with one participant, owned by the link, holding the
	// guest's takes. The submission key (per guest browser) is stored hashed.
	func(tx *sql.Tx) error {
		if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS record_links (
			id TEXT PRIMARY KEY,
			title TEXT NOT NULL,
			prompt TEXT NOT NULL DEFAULT '',
			room_id TEXT REFERENCES rooms(id) ON DELETE SET NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			max_duration_sec INTEGER NOT NULL,
			max_submissions INTEGER NOT NULL,
			max_total_bytes INTEGER NOT NULL,
			nonce TEXT NOT NULL DEFAULT '',
			secret_hash TEXT NOT NULL DEFAULT '',
			settings_json TEXT NOT NULL DEFAULT '{}'
		)`); err != nil {
			return err
		}
		if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS record_submissions (
			session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
			link_id TEXT NOT NULL,
			participant_id TEXT NOT NULL,
			name TEXT NOT NULL,
			key_hash TEXT NOT NULL,
			status TEXT NOT NULL,
			created_at TEXT NOT NULL,
			submitted_at TEXT,
			takes_started INTEGER NOT NULL DEFAULT 0
		)`); err != nil {
			return err
		}
		_, err := tx.Exec(`CREATE INDEX IF NOT EXISTS record_submissions_link ON record_submissions(link_id, status)`)
		return err
	},
	// 6: scheduled meetings and the host calendar feed [SL-P6-1]. A
	// scheduled meeting belongs to a room (deleted with it); starts_at is
	// fixed-width UTC text so it sorts. The feed secret is stored like a
	// room link: the random nonce and SHA-256 of the derived secret only.
	func(tx *sql.Tx) error {
		if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS scheduled_meetings (
			id TEXT PRIMARY KEY,
			room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
			title TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			starts_at TEXT NOT NULL,
			duration_min INTEGER NOT NULL,
			time_zone TEXT NOT NULL DEFAULT '',
			guest_emails TEXT NOT NULL DEFAULT '[]',
			organizer_email TEXT NOT NULL DEFAULT '',
			uid TEXT NOT NULL UNIQUE,
			sequence INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'scheduled',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`); err != nil {
			return err
		}
		if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS scheduled_meetings_start ON scheduled_meetings(starts_at, status)`); err != nil {
			return err
		}
		if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS scheduled_meetings_room ON scheduled_meetings(room_id)`); err != nil {
			return err
		}
		_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS calendar_feed (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			nonce TEXT NOT NULL DEFAULT '',
			secret_hash TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL
		)`)
		return err
	},
	// 7: lower thirds remembered per room [SL-P5-4]. A meeting's lower
	// thirds are per participant (lower_thirds); a room also remembers the
	// last name/title set for each person, keyed by their normalized display
	// name, and applies it when they join the room's next episode. Deleted
	// with the room.
	func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS room_lower_thirds (
			room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
			name_key TEXT NOT NULL,
			display_name TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL,
			PRIMARY KEY (room_id, name_key)
		)`)
		return err
	},
}

// SchemaVersion is the user_version a fully migrated database reports.
func SchemaVersion() int { return len(migrations) }

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this p2p.video (%d); refusing to downgrade", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", i+1, err)
		}
		if err := migrations[i](tx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		// user_version is transactional in SQLite: it moves only on commit.
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", i+1, err)
		}
	}
	return nil
}

func hasColumn(tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

const sessionColumns = `id, title, created_at, preset_json, invite_token_hash, auto_admit, ended_at, room_id`

type rowScanner interface{ Scan(dest ...any) error }

func scanSession(row rowScanner) (*Session, error) {
	var sess Session
	var createdAtStr string
	var endedAtStr, roomID sql.NullString
	var autoAdmit int
	if err := row.Scan(&sess.ID, &sess.Title, &createdAtStr, &sess.PresetJSON, &sess.InviteTokenHash, &autoAdmit, &endedAtStr, &roomID); err != nil {
		return nil, err
	}
	sess.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAtStr)
	sess.AutoAdmit = autoAdmit == 1
	sess.RoomID = roomID.String
	if endedAtStr.Valid {
		t, _ := time.Parse(time.RFC3339Nano, endedAtStr.String)
		sess.EndedAt = &t
	}
	return &sess, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
