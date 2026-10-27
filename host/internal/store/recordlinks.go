package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// RecordLink is an async "record solo" invitation [SL-P5-3]. Its secret is
// never stored: only the random nonce the daemon derives it from and its
// SHA-256 (see httpserver/recordlinks.go), like a room's guest link.
type RecordLink struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Prompt    string    `json:"prompt"`
	RoomID    string    `json:"roomId,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	// MaxDurationSec bounds every take; the server stops a take at the limit.
	MaxDurationSec int `json:"maxDurationSec"`
	// MaxSubmissions bounds submitted (finished) submissions.
	MaxSubmissions int `json:"maxSubmissions"`
	// MaxTotalBytes bounds every byte uploaded under the link.
	MaxTotalBytes int64           `json:"maxTotalBytes"`
	Nonce         string          `json:"-"`
	SecretHash    string          `json:"-"`
	Recording     SessionSettings `json:"recording"`
}

// LinkEnabled reports whether the link currently has a secret (not revoked).
func (l *RecordLink) LinkEnabled() bool { return l.Nonce != "" && l.SecretHash != "" }

// Expired reports whether the link no longer accepts recordings at now.
func (l *RecordLink) Expired(now time.Time) bool { return !now.Before(l.ExpiresAt) }

// Submission states.
const (
	SubmissionDraft     = "draft"
	SubmissionSubmitted = "submitted"
)

// RecordSubmission is one guest's visit to a record link: a session (with
// one participant) owned by the link [SL-P5-3].
type RecordSubmission struct {
	SessionID     string     `json:"id"`
	LinkID        string     `json:"linkId"`
	ParticipantID string     `json:"participantId"`
	Name          string     `json:"name"`
	KeyHash       string     `json:"-"`
	Status        string     `json:"status"`
	CreatedAt     time.Time  `json:"createdAt"`
	SubmittedAt   *time.Time `json:"submittedAt,omitempty"`
	// TakesStarted counts every take ever started (discarded ones too), so
	// take numbers and IDs are never reused.
	TakesStarted int `json:"takesStarted"`
}

const recordLinkColumns = `id, title, prompt, room_id, created_at, updated_at, expires_at, max_duration_sec, max_submissions, max_total_bytes, nonce, secret_hash, settings_json`

func scanRecordLink(row rowScanner) (*RecordLink, error) {
	var l RecordLink
	var room sql.NullString
	var created, updated, expires, settings string
	if err := row.Scan(&l.ID, &l.Title, &l.Prompt, &room, &created, &updated, &expires, &l.MaxDurationSec, &l.MaxSubmissions, &l.MaxTotalBytes, &l.Nonce, &l.SecretHash, &settings); err != nil {
		return nil, err
	}
	l.RoomID = room.String
	l.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	l.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	l.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires)
	l.Recording = DefaultSessionSettings()
	if settings != "" && settings != "{}" {
		if err := json.Unmarshal([]byte(settings), &l.Recording); err != nil {
			return nil, fmt.Errorf("decode record link settings: %w", err)
		}
	}
	return &l, nil
}

// CreateRecordLink inserts a record link.
func (s *Store) CreateRecordLink(l *RecordLink) error {
	settings, err := json.Marshal(l.Recording)
	if err != nil {
		return fmt.Errorf("encode record link settings: %w", err)
	}
	_, err = s.db.Exec(`INSERT INTO record_links (`+recordLinkColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.ID, l.Title, l.Prompt, nullString(l.RoomID), l.CreatedAt.UTC().Format(time.RFC3339Nano), l.UpdatedAt.UTC().Format(time.RFC3339Nano),
		l.ExpiresAt.UTC().Format(time.RFC3339Nano), l.MaxDurationSec, l.MaxSubmissions, l.MaxTotalBytes, l.Nonce, l.SecretHash, string(settings))
	if err != nil {
		return fmt.Errorf("insert record link: %w", err)
	}
	return nil
}

// GetRecordLink returns a record link (sql.ErrNoRows if absent).
func (s *Store) GetRecordLink(id string) (*RecordLink, error) {
	return scanRecordLink(s.db.QueryRow(`SELECT `+recordLinkColumns+` FROM record_links WHERE id = ?`, id))
}

// ListRecordLinks returns every record link, newest first.
func (s *Store) ListRecordLinks() ([]RecordLink, error) {
	rows, err := s.db.Query(`SELECT ` + recordLinkColumns + ` FROM record_links ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("list record links: %w", err)
	}
	defer rows.Close()
	list := make([]RecordLink, 0)
	for rows.Next() {
		l, err := scanRecordLink(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, *l)
	}
	return list, rows.Err()
}

// UpdateRecordLink persists the editable fields (title, prompt, expiry, caps).
func (s *Store) UpdateRecordLink(l *RecordLink) error {
	l.UpdatedAt = time.Now().UTC()
	return affectOne(s.db.Exec(`UPDATE record_links SET title = ?, prompt = ?, expires_at = ?, max_duration_sec = ?, max_submissions = ?, max_total_bytes = ?, updated_at = ? WHERE id = ?`,
		l.Title, l.Prompt, l.ExpiresAt.UTC().Format(time.RFC3339Nano), l.MaxDurationSec, l.MaxSubmissions, l.MaxTotalBytes, l.UpdatedAt.Format(time.RFC3339Nano), l.ID))
}

// SetRecordLinkSecret replaces the nonce and secret hash. Empty values revoke
// the link; a new pair rotates it, invalidating the old URL at once.
func (s *Store) SetRecordLinkSecret(id, nonce, secretHash string) error {
	return affectOne(s.db.Exec(`UPDATE record_links SET nonce = ?, secret_hash = ?, updated_at = ? WHERE id = ?`,
		nonce, secretHash, time.Now().UTC().Format(time.RFC3339Nano), id))
}

// DeleteRecordLink removes a link that has no submissions left.
func (s *Store) DeleteRecordLink(id string) error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM record_submissions WHERE link_id = ?`, id).Scan(&n); err != nil {
		return fmt.Errorf("count submissions: %w", err)
	}
	if n > 0 {
		return ErrLinkHasSubmissions
	}
	return affectOne(s.db.Exec(`DELETE FROM record_links WHERE id = ?`, id))
}

// ErrLinkHasSubmissions refuses deleting a record link that still owns recordings.
var ErrLinkHasSubmissions = errors.New("record link still has submissions")

const submissionColumns = `session_id, link_id, participant_id, name, key_hash, status, created_at, submitted_at, takes_started`

func scanSubmission(row rowScanner) (*RecordSubmission, error) {
	var sub RecordSubmission
	var created string
	var submitted sql.NullString
	if err := row.Scan(&sub.SessionID, &sub.LinkID, &sub.ParticipantID, &sub.Name, &sub.KeyHash, &sub.Status, &created, &submitted, &sub.TakesStarted); err != nil {
		return nil, err
	}
	sub.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if submitted.Valid {
		t, _ := time.Parse(time.RFC3339Nano, submitted.String)
		sub.SubmittedAt = &t
	}
	return &sub, nil
}

// CreateSubmission creates the submission's session, its one admitted
// participant and the submission row in one transaction.
func (s *Store) CreateSubmission(sess *Session, part *Participant, sub *RecordSubmission) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin submission: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO sessions (id, title, created_at, preset_json, invite_token_hash, auto_admit, room_id) VALUES (?, ?, ?, ?, ?, 0, NULL)`,
		sess.ID, sess.Title, sess.CreatedAt.UTC().Format(time.RFC3339Nano), sess.PresetJSON, sess.InviteTokenHash); err != nil {
		return fmt.Errorf("insert submission session: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO participants (id, session_id, name, role, parent_participant_id, status, joined_at, ua) VALUES (?, ?, ?, ?, NULL, ?, ?, ?)`,
		part.ID, part.SessionID, part.Name, part.Role, part.Status, part.JoinedAt.UTC().Format(time.RFC3339Nano), part.UserAgent); err != nil {
		return fmt.Errorf("insert submission participant: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO record_submissions (`+submissionColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, NULL, 0)`,
		sub.SessionID, sub.LinkID, sub.ParticipantID, sub.Name, sub.KeyHash, sub.Status, sub.CreatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("insert submission: %w", err)
	}
	return tx.Commit()
}

// GetSubmission returns a submission by its session ID (sql.ErrNoRows if the
// session is not a submission).
func (s *Store) GetSubmission(sessionID string) (*RecordSubmission, error) {
	return scanSubmission(s.db.QueryRow(`SELECT `+submissionColumns+` FROM record_submissions WHERE session_id = ?`, sessionID))
}

// IsSubmissionSession reports whether a session belongs to a record link (and
// so must never be joined as a call).
func (s *Store) IsSubmissionSession(sessionID string) bool {
	var n int
	return s.db.QueryRow(`SELECT COUNT(1) FROM record_submissions WHERE session_id = ?`, sessionID).Scan(&n) == nil && n > 0
}

// ListSubmissions returns a link's submissions, oldest first.
func (s *Store) ListSubmissions(linkID string) ([]RecordSubmission, error) {
	rows, err := s.db.Query(`SELECT `+submissionColumns+` FROM record_submissions WHERE link_id = ? ORDER BY created_at, session_id`, linkID)
	if err != nil {
		return nil, fmt.Errorf("list submissions: %w", err)
	}
	defer rows.Close()
	list := make([]RecordSubmission, 0)
	for rows.Next() {
		sub, err := scanSubmission(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, *sub)
	}
	return list, rows.Err()
}

// CountSubmissions returns a link's submitted and draft counts; drafts only
// those created after draftSince (older drafts are abandoned).
func (s *Store) CountSubmissions(linkID string, draftSince time.Time) (submitted, drafts int, err error) {
	err = s.db.QueryRow(`SELECT
		COALESCE(SUM(status = ?), 0),
		COALESCE(SUM(status = ? AND created_at > ?), 0)
		FROM record_submissions WHERE link_id = ?`, SubmissionSubmitted, SubmissionDraft, draftSince.UTC().Format(time.RFC3339Nano), linkID).Scan(&submitted, &drafts)
	if err != nil {
		err = fmt.Errorf("count submissions: %w", err)
	}
	return submitted, drafts, err
}

// NextSubmissionTake reserves the next take number of a draft submission.
func (s *Store) NextSubmissionTake(sessionID string) (int, error) {
	var n int
	err := s.db.QueryRow(`UPDATE record_submissions SET takes_started = takes_started + 1 WHERE session_id = ? AND status = ? RETURNING takes_started`, sessionID, SubmissionDraft).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("reserve take number: %w", err)
	}
	return n, nil
}

// SubmitSubmission marks a draft submitted, unless the link already holds
// maxSubmitted submitted ones (ErrSubmissionsFull). The check and the
// update are one statement, so concurrent submits cannot overshoot.
func (s *Store) SubmitSubmission(sessionID, linkID string, maxSubmitted int, at time.Time) error {
	res, err := s.db.Exec(`UPDATE record_submissions SET status = ?, submitted_at = ?
		WHERE session_id = ? AND status = ? AND (SELECT COUNT(1) FROM record_submissions WHERE link_id = ? AND status = ?) < ?`,
		SubmissionSubmitted, at.UTC().Format(time.RFC3339Nano), sessionID, SubmissionDraft, linkID, SubmissionSubmitted, maxSubmitted)
	if err != nil {
		return fmt.Errorf("submit: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSubmissionsFull
	}
	return nil
}

// ErrSubmissionsFull is returned when a link has taken all its submissions
// (or the submission is no longer a draft).
var ErrSubmissionsFull = errors.New("record link is full")

// LinkStoredBytes is every chunk byte stored under a record link's
// submissions (the per-link storage cap).
func (s *Store) LinkStoredBytes(linkID string) (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(c.size), 0) FROM chunks c
		JOIN tracks t ON t.id = c.track_id
		JOIN takes k ON k.id = t.take_id
		JOIN record_submissions rs ON rs.session_id = k.session_id
		WHERE rs.link_id = ?`, linkID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("sum link bytes: %w", err)
	}
	return n, nil
}

// SubmissionLink returns the record link that owns a session, or
// sql.ErrNoRows for an ordinary meeting.
func (s *Store) SubmissionLink(sessionID string) (*RecordLink, error) {
	return scanRecordLink(s.db.QueryRow(`SELECT `+prefixed("l.", recordLinkColumns)+` FROM record_links l
		JOIN record_submissions rs ON rs.link_id = l.id WHERE rs.session_id = ?`, sessionID))
}

// RecordingSoloTake is a submission take still recording, with its link's
// per-take limit.
type RecordingSoloTake struct {
	TakeID         string
	WallClockStart time.Time
	MaxDurationSec int
}

// ListRecordingSoloTakes returns every record-link take still recording.
func (s *Store) ListRecordingSoloTakes() ([]RecordingSoloTake, error) {
	rows, err := s.db.Query(`SELECT k.id, k.wall_clock_start, l.max_duration_sec FROM takes k
		JOIN record_submissions rs ON rs.session_id = k.session_id
		JOIN record_links l ON l.id = rs.link_id
		WHERE k.status = 'recording'`)
	if err != nil {
		return nil, fmt.Errorf("list recording solo takes: %w", err)
	}
	defer rows.Close()
	var list []RecordingSoloTake
	for rows.Next() {
		var t RecordingSoloTake
		var wall string
		if err := rows.Scan(&t.TakeID, &wall, &t.MaxDurationSec); err != nil {
			return nil, err
		}
		t.WallClockStart, _ = time.Parse(time.RFC3339Nano, wall)
		list = append(list, t)
	}
	return list, rows.Err()
}

// DeleteTake removes a take with its tracks, chunks rows and sources. The
// caller removes the files.
func (s *Store) DeleteTake(takeID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin delete take: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT source_id FROM tracks WHERE take_id = ?`, takeID)
	if err != nil {
		return fmt.Errorf("list take sources: %w", err)
	}
	var sources []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		sources = append(sources, id)
	}
	rows.Close()
	if err := affectOne(tx.Exec(`DELETE FROM takes WHERE id = ?`, takeID)); err != nil {
		return err
	}
	for _, id := range sources {
		if _, err := tx.Exec(`DELETE FROM sources WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete source: %w", err)
		}
	}
	return tx.Commit()
}

// DeleteSubmissionSession removes a submission's session with everything
// in it (takes first: take_participants restricts deleting participants).
func (s *Store) DeleteSubmissionSession(sessionID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin delete submission: %w", err)
	}
	defer tx.Rollback()
	if err := deleteSubmissionTx(tx, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

// deleteSubmissionTx deletes a submission's rows: takes (cascading to
// tracks, chunks, take participants and postproc jobs), its clock samples,
// the submission, and the session (cascading to its participant, sources
// and lower thirds).
func deleteSubmissionTx(tx *sql.Tx, sessionID string) error {
	for _, q := range []string{
		`DELETE FROM takes WHERE session_id = ?`,
		`DELETE FROM clock_samples WHERE participant_id IN (SELECT id FROM participants WHERE session_id = ?)`,
		`DELETE FROM record_submissions WHERE session_id = ?`,
		`DELETE FROM sessions WHERE id = ?`,
	} {
		if _, err := tx.Exec(q, sessionID); err != nil {
			return fmt.Errorf("delete submission: %w", err)
		}
	}
	return nil
}

// DraftActivity is a draft submission with what the cleanup policy needs
// [SL-P5-4]: when it was last touched and how much it holds.
type DraftActivity struct {
	RecordSubmission
	LinkTitle string `json:"linkTitle"`
	// LinkClosed is true when the link is expired or turned off (revoked).
	LinkClosed bool `json:"linkClosed"`
	// LastActivity is the latest of the draft's creation, a take start and
	// a chunk upload.
	LastActivity time.Time `json:"lastActivity"`
	// LastUpload is the latest chunk upload (zero without one).
	LastUpload time.Time `json:"lastUpload,omitempty"`
	Recording  bool      `json:"recording"`
	Takes      int       `json:"takes"`
	Bytes      int64     `json:"bytes"`
}

// ListDraftActivity returns every draft submission with its activity, oldest
// activity first. Timestamps are parsed (not compared as text), since older
// rows were written in local time.
func (s *Store) ListDraftActivity(now time.Time) ([]DraftActivity, error) {
	rows, err := s.db.Query(`SELECT `+prefixed("rs.", submissionColumns)+`, l.title, l.expires_at, l.nonce, l.secret_hash
		FROM record_submissions rs JOIN record_links l ON l.id = rs.link_id WHERE rs.status = ?`, SubmissionDraft)
	if err != nil {
		return nil, fmt.Errorf("list drafts: %w", err)
	}
	var out []DraftActivity
	for rows.Next() {
		var d DraftActivity
		var created, expires, nonce, hash string
		var submitted sql.NullString
		if err := rows.Scan(&d.SessionID, &d.LinkID, &d.ParticipantID, &d.Name, &d.KeyHash, &d.Status, &created, &submitted, &d.TakesStarted,
			&d.LinkTitle, &expires, &nonce, &hash); err != nil {
			rows.Close()
			return nil, err
		}
		d.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		exp, _ := time.Parse(time.RFC3339Nano, expires)
		d.LinkClosed = nonce == "" || hash == "" || !now.Before(exp)
		out = append(out, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := s.draftActivity(s.db, &out[i]); err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastActivity.Before(out[j].LastActivity) })
	return out, nil
}

type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// draftActivity fills d's activity, size and take counts.
func (s *Store) draftActivity(q querier, d *DraftActivity) error {
	d.LastActivity, d.LastUpload, d.Recording, d.Takes, d.Bytes = d.CreatedAt, time.Time{}, false, 0, 0
	rows, err := q.Query(`SELECT wall_clock_start, status FROM takes WHERE session_id = ?`, d.SessionID)
	if err != nil {
		return fmt.Errorf("draft takes: %w", err)
	}
	for rows.Next() {
		var wall, status string
		if err := rows.Scan(&wall, &status); err != nil {
			rows.Close()
			return err
		}
		d.Takes++
		d.Recording = d.Recording || status == "recording"
		if t, err := time.Parse(time.RFC3339Nano, wall); err == nil && t.After(d.LastActivity) {
			d.LastActivity = t
		}
	}
	rows.Close()
	rows, err = q.Query(`SELECT c.received_at, c.size FROM chunks c JOIN tracks t ON t.id = c.track_id JOIN takes k ON k.id = t.take_id WHERE k.session_id = ?`, d.SessionID)
	if err != nil {
		return fmt.Errorf("draft chunks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var at string
		var size int64
		if err := rows.Scan(&at, &size); err != nil {
			return err
		}
		d.Bytes += size
		if t, err := time.Parse(time.RFC3339Nano, at); err == nil && t.After(d.LastUpload) {
			d.LastUpload = t
		}
	}
	if d.LastUpload.After(d.LastActivity) {
		d.LastActivity = d.LastUpload
	}
	return rows.Err()
}

// ErrDraftBusy refuses deleting a draft that is no longer abandoned: it was
// submitted, is recording, or saw activity after the cutoff.
var ErrDraftBusy = errors.New("draft is in use")

// DeleteIdleDraft deletes a draft submission's rows only if it is still a
// draft, has no take recording and had no activity (take start or chunk
// upload) after idleBefore. The check and the delete are one transaction,
// so an upload that lands first keeps the draft. It returns the draft as it
// was (bytes included) for the caller to remove its files.
func (s *Store) DeleteIdleDraft(sessionID string, idleBefore time.Time) (*DraftActivity, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin delete draft: %w", err)
	}
	defer tx.Rollback()
	sub, err := scanSubmission(tx.QueryRow(`SELECT `+submissionColumns+` FROM record_submissions WHERE session_id = ?`, sessionID))
	if err != nil {
		return nil, err
	}
	d := &DraftActivity{RecordSubmission: *sub}
	if sub.Status != SubmissionDraft {
		return d, ErrDraftBusy
	}
	if err := s.draftActivity(tx, d); err != nil {
		return nil, err
	}
	if d.Recording || d.LastActivity.After(idleBefore) {
		return d, ErrDraftBusy
	}
	if err := deleteSubmissionTx(tx, sessionID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit delete draft: %w", err)
	}
	return d, nil
}

// prefixed qualifies a comma-separated column list with a table alias.
func prefixed(alias, columns string) string {
	cols := strings.Split(columns, ",")
	for i, c := range cols {
		cols[i] = alias + strings.TrimSpace(c)
	}
	return strings.Join(cols, ", ")
}
