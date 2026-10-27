package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Store encapsulates SQLite database operations for p2p.video.
type Store struct {
	db *sql.DB
}

// Session represents a p2p.video recording room.
type Session struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	CreatedAt  time.Time `json:"createdAt"`
	PresetJSON string    `json:"presetJson"`
	// InviteTokenHash is never serialized: no client needs it, and it would
	// let anyone who sees it confirm a guessed or leaked invite [SL-SEC-1].
	InviteTokenHash string     `json:"-"`
	AutoAdmit       bool       `json:"autoAdmit"`
	EndedAt         *time.Time `json:"endedAt,omitempty"`
	// RoomID links a meeting to the persistent room it was opened from;
	// empty for an instant (ephemeral) meeting.
	RoomID string `json:"roomId,omitempty"`
}

// SessionSettings captures fine-tuning parameters for recording, codecs, audio, and sync.
type SessionSettings struct {
	VideoBitrateBps int     `json:"video_bitrate_bps"`
	VideoCodec      string  `json:"video_codec"`
	TargetFPS       float64 `json:"target_fps"`
	AudioSampleRate int     `json:"audio_sample_rate"`
	AudioBitDepth   int     `json:"audio_bit_depth"`
	XcorrThreshold  float64 `json:"xcorr_threshold"`
	XcorrWindowMs   int     `json:"xcorr_window_ms"`
	Engine          string  `json:"engine"`
	// AutoTranscribe starts the Minutes transcript after each export (SL-P4-1).
	AutoTranscribe bool `json:"auto_transcribe"`
	// ExportCodec is the editor package's video codec: "h264" (default,
	// compact) or "prores_proxy" (faster, editor-native, larger) [SL-P0-9].
	ExportCodec string `json:"export_codec,omitempty"`
	// RecordingMicMode is the recording microphone default for everyone in
	// the session: "auto" (empty; each participant's headphones decide),
	// "raw" or "clean" [SL-P1-10]. A participant's explicit choice wins.
	RecordingMicMode string `json:"recording_mic_mode,omitempty"`
}

// DefaultSessionSettings provides safe production defaults for studio recording.
func DefaultSessionSettings() SessionSettings {
	return SessionSettings{
		VideoBitrateBps: 12000000,
		VideoCodec:      "h264",
		TargetFPS:       30.0,
		AudioSampleRate: 48000,
		AudioBitDepth:   24,
		XcorrThreshold:  0.65,
		XcorrWindowMs:   500,
		Engine:          "webcodecs",
	}
}

// Participant represents a guest or host in a session.
type Participant struct {
	ID                  string    `json:"id"`
	SessionID           string    `json:"sessionId"`
	Name                string    `json:"name"`
	Role                string    `json:"role"` // "host", "guest", "subsource", "producer"
	ParentParticipantID string    `json:"parentParticipantId,omitempty"`
	Status              string    `json:"status"` // "pending", "admitted", "denied", "disconnected"
	JoinedAt            time.Time `json:"joinedAt"`
	UserAgent           string    `json:"userAgent"`
}

// Source represents an input device (camera, mic, screen).
type Source struct {
	ID            string `json:"id"`
	ParticipantID string `json:"participantId"`
	Kind          string `json:"kind"` // "camera", "mic", "screen"
	Label         string `json:"label"`
	DeviceHint    string `json:"deviceHint"`
}

// Take represents a recording take.
type Take struct {
	ID             string    `json:"id"`
	SessionID      string    `json:"sessionId"`
	Number         int       `json:"number"`
	StartAtHostUs  int64     `json:"startAtHostUs"`
	StopAtHostUs   int64     `json:"stopAtHostUs"`
	WallClockStart time.Time `json:"wallClockStart"`
	Status         string    `json:"status"` // "recording", "stopped", "uploading", "ready", "processing", "complete", "failed"
}

// Track represents an individual recording track for a source in a take.
type Track struct {
	ID         string `json:"id"`
	TakeID     string `json:"takeId"`
	SourceID   string `json:"sourceId"`
	Kind       string `json:"kind"` // "video", "audio"
	Mime       string `json:"mime"`
	Codec      string `json:"codec"`
	LastSeq    int    `json:"lastSeq"`
	TotalBytes int64  `json:"totalBytes"`
	SHA256     string `json:"sha256"`
	Status     string `json:"status"` // "recording", "uploading", "complete", "assembled", "processed", "failed"
}

// Chunk represents a single uploaded media slice.
type Chunk struct {
	TrackID    string    `json:"trackId"`
	Seq        int       `json:"seq"`
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256"`
	ReceivedAt time.Time `json:"receivedAt"`
}

// PostprocJob represents an asynchronous post-processing task.
type PostprocJob struct {
	ID         string     `json:"id"`
	TakeID     string     `json:"takeId"`
	Step       string     `json:"step"`
	Status     string     `json:"status"` // "pending", "running", "completed", "failed"
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Log        string     `json:"log"`
}

// Open creates or opens the SQLite database and executes migrations.
func Open(dbPath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	// The database holds names and session metadata: owner-only [SL-SEC-1].
	// Creating it 0600 first makes SQLite give its -wal/-shm files the same
	// mode; existing databases and journals are tightened in place.
	f, err := os.OpenFile(dbPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create db file: %w", err)
	}
	_ = f.Close()
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("restrict db file %s: %w", filepath.Base(p), err)
		}
	}

	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite db: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}
	return s, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// baselineSchema is the schema every data directory had before versioned
// migrations (user_version 0). It is idempotent, so it is also migration 1.
const baselineSchema = `
	CREATE TABLE IF NOT EXISTS sessions (
		id TEXT PRIMARY KEY,
		title TEXT NOT NULL,
		created_at TEXT NOT NULL,
		preset_json TEXT NOT NULL DEFAULT '{}',
		invite_token_hash TEXT NOT NULL,
		auto_admit INTEGER NOT NULL DEFAULT 1,
		ended_at TEXT
	);

	CREATE TABLE IF NOT EXISTS participants (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		name TEXT NOT NULL,
		role TEXT NOT NULL,
		parent_participant_id TEXT,
		status TEXT NOT NULL,
		joined_at TEXT NOT NULL,
		ua TEXT NOT NULL,
		FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS sources (
		id TEXT PRIMARY KEY,
		participant_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		label TEXT NOT NULL,
		device_hint TEXT NOT NULL DEFAULT '',
		FOREIGN KEY(participant_id) REFERENCES participants(id) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS takes (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		number INTEGER NOT NULL,
		start_at_host_us INTEGER NOT NULL DEFAULT 0,
		stop_at_host_us INTEGER NOT NULL DEFAULT 0,
		wall_clock_start TEXT NOT NULL,
		status TEXT NOT NULL,
		FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS tracks (
		id TEXT PRIMARY KEY,
		take_id TEXT NOT NULL,
		source_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		mime TEXT NOT NULL,
		codec TEXT NOT NULL DEFAULT '',
		last_seq INTEGER NOT NULL DEFAULT -1,
		total_bytes INTEGER NOT NULL DEFAULT 0,
		sha256 TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL,
		FOREIGN KEY(take_id) REFERENCES takes(id) ON DELETE CASCADE,
		FOREIGN KEY(source_id) REFERENCES sources(id) ON DELETE CASCADE
	);

 CREATE TABLE IF NOT EXISTS take_participants (
  take_id TEXT NOT NULL,
  participant_id TEXT NOT NULL,
  PRIMARY KEY(take_id, participant_id),
  FOREIGN KEY(take_id) REFERENCES takes(id) ON DELETE CASCADE,
  FOREIGN KEY(participant_id) REFERENCES participants(id) ON DELETE RESTRICT
 );

	CREATE TABLE IF NOT EXISTS chunks (
		track_id TEXT NOT NULL,
		seq INTEGER NOT NULL,
		size INTEGER NOT NULL,
		sha256 TEXT NOT NULL,
		received_at TEXT NOT NULL,
		PRIMARY KEY(track_id, seq),
		FOREIGN KEY(track_id) REFERENCES tracks(id) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS clock_samples (
		participant_id TEXT NOT NULL,
		t0 INTEGER NOT NULL,
		t1 INTEGER NOT NULL,
		t2 INTEGER NOT NULL,
		t3 INTEGER NOT NULL,
		recorded_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS postproc_jobs (
		id TEXT PRIMARY KEY,
		take_id TEXT NOT NULL,
		step TEXT NOT NULL,
		status TEXT NOT NULL,
		started_at TEXT NOT NULL,
		finished_at TEXT,
		log TEXT NOT NULL DEFAULT '',
		FOREIGN KEY(take_id) REFERENCES takes(id) ON DELETE CASCADE
	);
	`

// CreateSession persists a new session.
func (s *Store) CreateSession(sess *Session) error {
	autoAdmit := 0
	if sess.AutoAdmit {
		autoAdmit = 1
	}
	_, err := s.db.Exec(`
		INSERT INTO sessions (id, title, created_at, preset_json, invite_token_hash, auto_admit, room_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, sess.ID, sess.Title, sess.CreatedAt.Format(time.RFC3339Nano), sess.PresetJSON, sess.InviteTokenHash, autoAdmit, nullString(sess.RoomID))
	return err
}

// GetSession retrieves a session by ID.
func (s *Store) GetSession(id string) (*Session, error) {
	return scanSession(s.db.QueryRow(`SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id))
}

// GetSessionByTokenHash retrieves a session using the hashed invite token.
func (s *Store) GetSessionByTokenHash(tokenHash string) (*Session, error) {
	return scanSession(s.db.QueryRow(`SELECT `+sessionColumns+` FROM sessions WHERE invite_token_hash = ?`, tokenHash))
}

// UpdateSessionSettings updates the preset_json column for a session.
func (s *Store) UpdateSessionSettings(sessionID string, presetJSON string) error {
	res, err := s.db.Exec(`UPDATE sessions SET preset_json = ? WHERE id = ?`, presetJSON, sessionID)
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

// UpdateSessionInviteTokenHash updates the invite token hash for a session.
func (s *Store) UpdateSessionInviteTokenHash(sessionID string, tokenHash string) error {
	res, err := s.db.Exec(`UPDATE sessions SET invite_token_hash = ? WHERE id = ?`, tokenHash, sessionID)
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

// CreateParticipant creates a new participant record.
func (s *Store) CreateParticipant(p *Participant) error {
	_, err := s.db.Exec(`
		INSERT INTO participants (id, session_id, name, role, parent_participant_id, status, joined_at, ua)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, p.ID, p.SessionID, p.Name, p.Role, p.ParentParticipantID, p.Status, p.JoinedAt.Format(time.RFC3339Nano), p.UserAgent)
	return err
}

// GetParticipant retrieves a participant by ID.
func (s *Store) GetParticipant(id string) (*Participant, error) {
	row := s.db.QueryRow(`
		SELECT id, session_id, name, role, parent_participant_id, status, joined_at, ua
		FROM participants WHERE id = ?
	`, id)

	var p Participant
	var joinedAtStr string
	var parentID sql.NullString

	err := row.Scan(&p.ID, &p.SessionID, &p.Name, &p.Role, &parentID, &p.Status, &joinedAtStr, &p.UserAgent)
	if err != nil {
		return nil, err
	}
	p.JoinedAt, _ = time.Parse(time.RFC3339Nano, joinedAtStr)
	if parentID.Valid {
		p.ParentParticipantID = parentID.String
	}
	return &p, nil
}

// UpdateParticipantStatus updates a participant's status.
func (s *Store) UpdateParticipantStatus(id string, status string) error {
	_, err := s.db.Exec(`UPDATE participants SET status = ? WHERE id = ?`, status, id)
	return err
}

// ListParticipants returns all participants in a session.
func (s *Store) ListParticipants(sessionID string) ([]Participant, error) {
	rows, err := s.db.Query(`
		SELECT id, session_id, name, role, parent_participant_id, status, joined_at, ua
		FROM participants WHERE session_id = ? ORDER BY joined_at ASC
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []Participant
	for rows.Next() {
		var p Participant
		var joinedAtStr string
		var parentID sql.NullString
		if err := rows.Scan(&p.ID, &p.SessionID, &p.Name, &p.Role, &parentID, &p.Status, &joinedAtStr, &p.UserAgent); err != nil {
			return nil, err
		}
		p.JoinedAt, _ = time.Parse(time.RFC3339Nano, joinedAtStr)
		if parentID.Valid {
			p.ParentParticipantID = parentID.String
		}
		list = append(list, p)
	}
	return list, rows.Err()
}

// CreateSource registers a camera, mic, or screen source.
func (s *Store) CreateSource(src *Source) error {
	_, err := s.db.Exec(`
		INSERT INTO sources (id, participant_id, kind, label, device_hint)
		VALUES (?, ?, ?, ?, ?)
	`, src.ID, src.ParticipantID, src.Kind, src.Label, src.DeviceHint)
	return err
}

// GetSource retrieves a source by ID.
func (s *Store) GetSource(id string) (*Source, error) {
	row := s.db.QueryRow(`
		SELECT id, participant_id, kind, label, device_hint
		FROM sources WHERE id = ?
	`, id)
	var src Source
	err := row.Scan(&src.ID, &src.ParticipantID, &src.Kind, &src.Label, &src.DeviceHint)
	if err != nil {
		return nil, err
	}
	return &src, nil
}

// ListSourcesByParticipant returns sources belonging to a participant.
func (s *Store) ListSourcesByParticipant(participantID string) ([]Source, error) {
	rows, err := s.db.Query(`
		SELECT id, participant_id, kind, label, device_hint
		FROM sources WHERE participant_id = ?
	`, participantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []Source
	for rows.Next() {
		var src Source
		if err := rows.Scan(&src.ID, &src.ParticipantID, &src.Kind, &src.Label, &src.DeviceHint); err != nil {
			return nil, err
		}
		list = append(list, src)
	}
	return list, rows.Err()
}

// CreateTake registers a new take.
func (s *Store) CreateTake(t *Take) error {
	_, err := s.db.Exec(`
		INSERT INTO takes (id, session_id, number, start_at_host_us, stop_at_host_us, wall_clock_start, status)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, t.ID, t.SessionID, t.Number, t.StartAtHostUs, t.StopAtHostUs, t.WallClockStart.Format(time.RFC3339Nano), t.Status)
	return err
}

// GetTake retrieves a take by ID.
func (s *Store) GetTake(id string) (*Take, error) {
	row := s.db.QueryRow(`
		SELECT id, session_id, number, start_at_host_us, stop_at_host_us, wall_clock_start, status
		FROM takes WHERE id = ?
	`, id)

	var t Take
	var wallClockStr string
	err := row.Scan(&t.ID, &t.SessionID, &t.Number, &t.StartAtHostUs, &t.StopAtHostUs, &wallClockStr, &t.Status)
	if err != nil {
		return nil, err
	}
	t.WallClockStart, _ = time.Parse(time.RFC3339Nano, wallClockStr)
	return &t, nil
}

// UpdateTake updates take timing and status.
func (s *Store) UpdateTake(t *Take) error {
	_, err := s.db.Exec(`
		UPDATE takes SET stop_at_host_us = ?, status = ? WHERE id = ?
	`, t.StopAtHostUs, t.Status, t.ID)
	return err
}

// ListTakesBySession retrieves all takes for a session.
func (s *Store) ListTakesBySession(sessionID string) ([]Take, error) {
	rows, err := s.db.Query(`
		SELECT id, session_id, number, start_at_host_us, stop_at_host_us, wall_clock_start, status
		FROM takes WHERE session_id = ? ORDER BY number ASC
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []Take
	for rows.Next() {
		var t Take
		var wallClockStr string
		if err := rows.Scan(&t.ID, &t.SessionID, &t.Number, &t.StartAtHostUs, &t.StopAtHostUs, &wallClockStr, &t.Status); err != nil {
			return nil, err
		}
		t.WallClockStart, _ = time.Parse(time.RFC3339Nano, wallClockStr)
		list = append(list, t)
	}
	return list, rows.Err()
}

// CreateTrack registers a recording track.
func (s *Store) CreateTrack(tr *Track) error {
	_, err := s.db.Exec(`
		INSERT INTO tracks (id, take_id, source_id, kind, mime, codec, last_seq, total_bytes, sha256, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, tr.ID, tr.TakeID, tr.SourceID, tr.Kind, tr.Mime, tr.Codec, tr.LastSeq, tr.TotalBytes, tr.SHA256, tr.Status)
	return err
}

// GetTrack retrieves a track by ID.
func (s *Store) GetTrack(id string) (*Track, error) {
	row := s.db.QueryRow(`
		SELECT id, take_id, source_id, kind, mime, codec, last_seq, total_bytes, sha256, status
		FROM tracks WHERE id = ?
	`, id)
	var tr Track
	err := row.Scan(&tr.ID, &tr.TakeID, &tr.SourceID, &tr.Kind, &tr.Mime, &tr.Codec, &tr.LastSeq, &tr.TotalBytes, &tr.SHA256, &tr.Status)
	if err != nil {
		return nil, err
	}
	return &tr, nil
}

// UpdateTrack updates track metadata and completion state.
func (s *Store) UpdateTrack(tr *Track) error {
	_, err := s.db.Exec(`
		UPDATE tracks SET last_seq = ?, total_bytes = ?, sha256 = ?, status = ? WHERE id = ?
	`, tr.LastSeq, tr.TotalBytes, tr.SHA256, tr.Status, tr.ID)
	return err
}

// ListTracksByTake returns all tracks for a take.
func (s *Store) ListTracksByTake(takeID string) ([]Track, error) {
	rows, err := s.db.Query(`
		SELECT id, take_id, source_id, kind, mime, codec, last_seq, total_bytes, sha256, status
		FROM tracks WHERE take_id = ?
	`, takeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []Track
	for rows.Next() {
		var tr Track
		if err := rows.Scan(&tr.ID, &tr.TakeID, &tr.SourceID, &tr.Kind, &tr.Mime, &tr.Codec, &tr.LastSeq, &tr.TotalBytes, &tr.SHA256, &tr.Status); err != nil {
			return nil, err
		}
		list = append(list, tr)
	}
	return list, rows.Err()
}

// StoreChunk registers an uploaded chunk in the database.
func (s *Store) StoreChunk(c *Chunk) error {
	_, err := s.db.Exec(`
		INSERT INTO chunks (track_id, seq, size, sha256, received_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(track_id, seq) DO UPDATE SET
			size = excluded.size,
			sha256 = excluded.sha256,
			received_at = excluded.received_at
	`, c.TrackID, c.Seq, c.Size, c.SHA256, c.ReceivedAt.Format(time.RFC3339Nano))
	return err
}

// StoredChunkBytes is the total size of a track's stored chunks.
func (s *Store) StoredChunkBytes(trackID string) (int64, error) {
	var n int64
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(size), 0) FROM chunks WHERE track_id = ?`, trackID).Scan(&n); err != nil {
		return 0, fmt.Errorf("sum chunk bytes: %w", err)
	}
	return n, nil
}

// CountParticipantTracks counts the tracks a participant registered in a take.
func (s *Store) CountParticipantTracks(takeID, participantID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM tracks t JOIN sources src ON src.id = t.source_id
		WHERE t.take_id = ? AND src.participant_id = ?`, takeID, participantID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count participant tracks: %w", err)
	}
	return n, nil
}

// GetStoredChunkSeqs returns all received chunk sequence numbers for a track, sorted ascending.
func (s *Store) GetStoredChunkSeqs(trackID string) ([]int, error) {
	rows, err := s.db.Query(`SELECT seq FROM chunks WHERE track_id = ? ORDER BY seq ASC`, trackID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var seqs []int
	for rows.Next() {
		var seq int
		if err := rows.Scan(&seq); err != nil {
			return nil, err
		}
		seqs = append(seqs, seq)
	}
	return seqs, rows.Err()
}

// CheckChunkExists checks if a chunk with matching SHA256 is already stored.
func (s *Store) CheckChunkExists(trackID string, seq int) (bool, string, error) {
	var sha string
	err := s.db.QueryRow(`SELECT sha256 FROM chunks WHERE track_id = ? AND seq = ?`, trackID, seq).Scan(&sha)
	if err == sql.ErrNoRows {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, sha, nil
}

// SaveClockSample records a clock sync sample for diagnostics.
func (s *Store) SaveClockSample(participantID string, t0, t1, t2, t3 int64) error {
	_, err := s.db.Exec(`
		INSERT INTO clock_samples (participant_id, t0, t1, t2, t3, recorded_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, participantID, t0, t1, t2, t3, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// CreateTakeWithParticipants commits the take and its expected recording roster
// together, before recording instructions can be sent to browsers.
func (s *Store) CreateTakeWithParticipants(t *Take, participantIDs []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin take snapshot: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO takes (id, session_id, number, start_at_host_us, stop_at_host_us, wall_clock_start, status) VALUES (?, ?, ?, ?, ?, ?, ?)`, t.ID, t.SessionID, t.Number, t.StartAtHostUs, t.StopAtHostUs, t.WallClockStart.Format(time.RFC3339Nano), t.Status)
	if err != nil {
		return fmt.Errorf("create snapshotted take: %w", err)
	}
	for _, id := range participantIDs {
		if err := expectTakeParticipant(tx, t.ID, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit take snapshot: %w", err)
	}
	return nil
}

type takeParticipantWriter interface {
	Exec(string, ...any) (sql.Result, error)
	QueryRow(string, ...any) *sql.Row
}

func expectTakeParticipant(writer takeParticipantWriter, takeID, participantID string) error {
	var sameSession bool
	if err := writer.QueryRow(`SELECT t.session_id=p.session_id FROM takes t JOIN participants p ON p.id=? WHERE t.id=?`, participantID, takeID).Scan(&sameSession); err != nil {
		return fmt.Errorf("validate expected participant: %w", err)
	}
	if !sameSession {
		return fmt.Errorf("expected participant belongs to another session")
	}
	if _, err := writer.Exec(`INSERT INTO take_participants(take_id,participant_id) VALUES(?,?) ON CONFLICT(take_id,participant_id) DO NOTHING`, takeID, participantID); err != nil {
		return fmt.Errorf("persist expected participant: %w", err)
	}
	return nil
}

// ExpectTakeParticipant appends an expectation idempotently; leaving a room does
// not remove it, because a missing local recording still needs recovery.
func (s *Store) ExpectTakeParticipant(takeID, participantID string) error {
	return expectTakeParticipant(s.db, takeID, participantID)
}

// ListMissingTakeParticipants identifies expected recorders without any assembled
// camera/microphone recording. Existing per-track checks separately reject
// partial uploads. A screen-share track never satisfies the expectation: it is
// an optional extra source that may start and stop at any time during a take
// (its timeline offset comes from its own sidecar), so it neither substitutes
// for a missing camera recording nor is itself expected.
func (s *Store) ListMissingTakeParticipants(takeID string) ([]Participant, error) {
	rows, err := s.db.Query(`SELECT p.id,p.session_id,p.name,p.role,p.parent_participant_id,p.status,p.joined_at,p.ua
 FROM take_participants expected JOIN participants p ON p.id=expected.participant_id
 WHERE expected.take_id=? AND NOT EXISTS (
  SELECT 1 FROM tracks tr JOIN sources source ON source.id=tr.source_id
  WHERE tr.take_id=expected.take_id AND source.participant_id=p.id AND source.kind<>'screen' AND tr.status IN ('assembled','processed')
 ) ORDER BY p.id`, takeID)
	if err != nil {
		return nil, fmt.Errorf("list missing take participants: %w", err)
	}
	defer rows.Close()
	list := make([]Participant, 0)
	for rows.Next() {
		var p Participant
		var parent sql.NullString
		var joined string
		if err := rows.Scan(&p.ID, &p.SessionID, &p.Name, &p.Role, &parent, &p.Status, &joined, &p.UserAgent); err != nil {
			return nil, err
		}
		p.ParentParticipantID = parent.String
		p.JoinedAt, err = time.Parse(time.RFC3339Nano, joined)
		if err != nil {
			return nil, fmt.Errorf("decode participant joined time: %w", err)
		}
		list = append(list, p)
	}
	return list, rows.Err()
}

// ListRecentTakes returns the most recently started takes across all sessions.
func (s *Store) ListRecentTakes(limit int) ([]Take, error) {
	rows, err := s.db.Query(`
		SELECT id, session_id, number, start_at_host_us, stop_at_host_us, wall_clock_start, status
		FROM takes ORDER BY wall_clock_start DESC, id DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list recent takes: %w", err)
	}
	defer rows.Close()
	list := make([]Take, 0)
	for rows.Next() {
		var t Take
		var wallClockStr string
		if err := rows.Scan(&t.ID, &t.SessionID, &t.Number, &t.StartAtHostUs, &t.StopAtHostUs, &wallClockStr, &t.Status); err != nil {
			return nil, fmt.Errorf("scan recent take: %w", err)
		}
		t.WallClockStart, _ = time.Parse(time.RFC3339Nano, wallClockStr)
		list = append(list, t)
	}
	return list, rows.Err()
}
