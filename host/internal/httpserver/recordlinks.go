package httpserver

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/omniharmonic/studiolink/host/internal/clock"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

// Async record links [SL-P5-3] (Riverside's "record solo"). A host creates
// a link with a title, an optional prompt/script, a per-take duration limit,
// an expiry and caps. A guest opens
//
//	<guest base>/record?link=<id>&token=<secret>
//
// with no call and no host present, records takes locally (the same OPFS
// recorder and progressive upload as a call), reviews or re-records them and
// submits. Each guest visit is a submission: an ordinary session with one
// admitted participant, owned by the link, so takes, tracks, uploads,
// export, originals and the transcript/Parachute handoffs are the call
// pipeline unchanged.
//
// Credentials. secret = hex(HMAC-SHA256(daemon key, "studiolink/record-link/v1"
// ‖ link ID ‖ nonce)): 256 bits, never stored (nonce + SHA-256 only), so the
// dashboard can show the link again but a copied database cannot. Rotating
// replaces the nonce (the old URL is refused at once); revoking clears both.
// Every guest call needs the current secret. A submission's own calls also
// need its submission key: 256 random bits handed to the browser that
// created it (stored hashed), so one guest can never read, change or submit
// another's recording. Uploads use the submission participant's ordinary
// signed grant (12 h), re-minted on every submission read.
//
// The link grants only this: no WebSocket (submission sessions refuse calls),
// no host route (all of /api/v1/recordlinks is admin-only and Funnel-denied),
// no other submission. Caps are enforced here and in the upload handler:
// submissions (submitted count), drafts open at once, takes per submission,
// per-take duration (the server stops the take), and total bytes per link.
// Guest entry points are rate-limited like call joins.
const (
	recordLinkIDPrefix = "rl_"
	maxRecordTitle     = 120
	maxRecordPrompt    = 64 * 1024

	defaultRecordDuration = 30 * time.Minute
	minRecordDuration     = 10 * time.Second
	maxRecordDuration     = 4 * time.Hour

	defaultRecordExpiry = 7 * 24 * time.Hour
	maxRecordExpiry     = 90 * 24 * time.Hour

	defaultRecordSubmissions = 10
	maxRecordSubmissionsCap  = 500

	defaultRecordBytes = 20 << 30
	minRecordBytes     = 50 << 20
	maxRecordBytes     = 2 << 40

	// recordDraftsOpen bounds drafts started in the last recordDraftWindow
	// (a leaked link can't create unbounded sessions).
	recordDraftsOpen  = 5
	recordDraftWindow = 24 * time.Hour
	// Takes per submission: kept at once, and started in total (discarded
	// re-records included).
	recordLiveTakes    = 10
	recordStartedTakes = 30
	// recordTakeGrace is how long past the limit a take may run before the
	// server stops it (the page stops itself at the limit).
	recordTakeGrace = 2 * time.Second

	submissionKeyHeader = "X-Submission-Key"
	recordGrantTTL      = 12 * time.Hour
)

var (
	errRecordLinkInvalid = "This recording link isn’t valid any more. Ask the host for a new one."
	errRecordLinkExpired = "This recording link has expired. Ask the host for a new one."
)

// recordSecret derives a record link's secret from its current nonce.
func (s *Server) recordSecret(linkID, nonce string) string {
	m := hmac.New(sha256.New, s.Config.SecretKey)
	m.Write([]byte("studiolink/record-link/v1\x00" + linkID + "\x00" + nonce))
	return hex.EncodeToString(m.Sum(nil))
}

// newRecordSecret returns a fresh 128-bit nonce and the hash of its secret.
func (s *Server) newRecordSecret(linkID string) (nonce, secretHash string, err error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("record link entropy: %w", err)
	}
	nonce = hex.EncodeToString(b)
	return nonce, hashToken(s.recordSecret(linkID, nonce)), nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ---------------------------------------------------------------- views

// RecordTakeView is one take of a submission as the host sees it.
type RecordTakeView struct {
	store.Take
	DurationSec    float64 `json:"durationSec"`
	Tracks         int     `json:"tracks"`
	Assembled      int     `json:"assembled"`
	OriginalsReady bool    `json:"originalsReady"`
}

// RecordSubmissionView is a submission on the host dashboard. HostToken is a
// short-lived host credential for its session (export, downloads, handoffs);
// only submitted submissions carry one.
type RecordSubmissionView struct {
	store.RecordSubmission
	HostToken string           `json:"hostToken,omitempty"`
	Takes     []RecordTakeView `json:"takes"`
}

// RecordLinkView is a record link on the host dashboard. URL carries the
// secret: management responses only, never logged.
type RecordLinkView struct {
	store.RecordLink
	LinkEnabled bool   `json:"linkEnabled"`
	LinkLost    bool   `json:"linkLost,omitempty"`
	URL         string `json:"url,omitempty"`
	Path        string `json:"path,omitempty"`
	URLRemote   bool   `json:"urlRemote"`
	Expired     bool   `json:"expired"`
	RoomName    string `json:"roomName,omitempty"`
	Submitted   int    `json:"submitted"`
	Drafts      int    `json:"drafts"`
	// AbandonedDrafts counts drafts idle long enough to clean up [SL-P5-4]
	// (included in Drafts); see GET /api/v1/recordlinks/drafts.
	AbandonedDrafts int   `json:"abandonedDrafts"`
	UsedBytes       int64 `json:"usedBytes"`
	// Submissions are newest first.
	Submissions []RecordSubmissionView `json:"submissions"`
}

func (s *Server) recordLinkView(l *store.RecordLink, base guestBaseFn) RecordLinkView {
	v := RecordLinkView{RecordLink: *l, LinkEnabled: l.LinkEnabled(), Expired: l.Expired(time.Now()), Submissions: []RecordSubmissionView{}}
	if v.LinkEnabled {
		secret := s.recordSecret(l.ID, l.Nonce)
		if hashToken(secret) != l.SecretHash {
			v.LinkLost = true
		} else {
			v.Path = "/record?" + url.Values{"link": {l.ID}, "token": {secret}}.Encode()
			b, remote := base()
			v.URL, v.URLRemote = b+v.Path, remote
		}
	}
	if l.RoomID != "" {
		if room, err := s.Store.GetRoom(l.RoomID); err == nil {
			v.RoomName = room.Name
		}
	}
	v.UsedBytes, _ = s.Store.LinkStoredBytes(l.ID)
	subs, err := s.Store.ListSubmissions(l.ID)
	if err != nil {
		slog.Warn("list record submissions failed", "linkId", l.ID, "err", err)
		return v
	}
	for i := len(subs) - 1; i >= 0; i-- {
		sub := subs[i]
		if sub.Status == store.SubmissionSubmitted {
			v.Submitted++
		} else {
			v.Drafts++
		}
		sv := RecordSubmissionView{RecordSubmission: sub, Takes: s.recordTakeViews(sub.SessionID)}
		if sub.Status == store.SubmissionSubmitted {
			sv.HostToken = signaling.MintHostToken(sub.SessionID, homeTokenTTL, s.Config.SecretKey)
		}
		v.Submissions = append(v.Submissions, sv)
	}
	if v.Drafts > 0 {
		if drafts, _, err := s.draftViews(time.Now()); err == nil {
			for _, d := range drafts {
				if d.LinkID == l.ID && d.Abandoned {
					v.AbandonedDrafts++
				}
			}
		}
	}
	return v
}

func takeDurationSec(tk *store.Take) float64 {
	if tk.StopAtHostUs > tk.StartAtHostUs && tk.StartAtHostUs > 0 {
		return float64(tk.StopAtHostUs-tk.StartAtHostUs) / 1e6
	}
	if tk.Status == "recording" && !tk.WallClockStart.IsZero() {
		return max(time.Since(tk.WallClockStart).Seconds(), 0)
	}
	return 0
}

func (s *Server) recordTakeViews(sessionID string) []RecordTakeView {
	takes, _ := s.Store.ListTakesBySession(sessionID)
	out := make([]RecordTakeView, 0, len(takes))
	for i := range takes {
		tk := takes[i]
		v := RecordTakeView{Take: tk, DurationSec: takeDurationSec(&tk)}
		tracks, _ := s.Store.ListTracksByTake(tk.ID)
		v.Tracks = len(tracks)
		for _, tr := range tracks {
			if tr.Status == "assembled" || tr.Status == "processed" {
				v.Assembled++
			}
		}
		missing, err := s.Store.ListMissingTakeParticipants(tk.ID)
		v.OriginalsReady = err == nil && len(missing) == 0 && tk.Status != "recording" && v.Tracks > 0 && v.Assembled == v.Tracks
		out = append(out, v)
	}
	return out
}

// RecordLinkPublic is what a link holder may know before recording.
type RecordLinkPublic struct {
	ID             string    `json:"id"`
	Title          string    `json:"title"`
	Prompt         string    `json:"prompt"`
	MaxDurationSec int       `json:"maxDurationSec"`
	ExpiresAt      time.Time `json:"expiresAt"`
	// Accepting is false once the link holds all its submissions.
	Accepting bool                  `json:"accepting"`
	Reason    string                `json:"reason,omitempty"`
	Recording store.SessionSettings `json:"recording"`
}

// RecordGuestTrack is one uploaded track as its guest sees it.
type RecordGuestTrack struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Status string `json:"status"`
	Bytes  int64  `json:"bytes"`
}

// RecordGuestTake is one of the guest's own takes.
type RecordGuestTake struct {
	ID          string             `json:"id"`
	Number      int                `json:"number"`
	Status      string             `json:"status"`
	DurationSec float64            `json:"durationSec"`
	Tracks      []RecordGuestTrack `json:"tracks"`
}

// RecordGuestSubmission is the guest's own submission. UploadToken is the
// participant grant for /api/v1/takes uploads (fresh on every read).
type RecordGuestSubmission struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Status         string            `json:"status"`
	ParticipantID  string            `json:"participantId"`
	UploadToken    string            `json:"uploadToken"`
	MaxDurationSec int               `json:"maxDurationSec"`
	Takes          []RecordGuestTake `json:"takes"`
}

func (s *Server) guestSubmission(link *store.RecordLink, sub *store.RecordSubmission) RecordGuestSubmission {
	out := RecordGuestSubmission{ID: sub.SessionID, Name: sub.Name, Status: sub.Status, ParticipantID: sub.ParticipantID,
		MaxDurationSec: link.MaxDurationSec, Takes: []RecordGuestTake{},
		UploadToken: signaling.MintUploadToken(sub.ParticipantID, sub.SessionID, recordGrantTTL, s.Config.SecretKey)}
	takes, _ := s.Store.ListTakesBySession(sub.SessionID)
	for i := range takes {
		tk := takes[i]
		gt := RecordGuestTake{ID: tk.ID, Number: tk.Number, Status: tk.Status, DurationSec: takeDurationSec(&tk), Tracks: []RecordGuestTrack{}}
		tracks, _ := s.Store.ListTracksByTake(tk.ID)
		for _, tr := range tracks {
			label := ""
			if src, err := s.Store.GetSource(tr.SourceID); err == nil {
				label = src.Label
			}
			bytes, _ := s.Store.StoredChunkBytes(tr.ID)
			gt.Tracks = append(gt.Tracks, RecordGuestTrack{ID: tr.ID, Kind: tr.Kind, Label: label, Status: tr.Status, Bytes: bytes})
		}
		out.Takes = append(out.Takes, gt)
	}
	return out
}

// ---------------------------------------------------------------- host API

// handleRecordLinks serves /api/v1/recordlinks[...] (admin only, never Funnel).
func (s *Server) handleRecordLinks(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(r) {
		adminDenied(w, r, "Managing recording links")
		return
	}
	s.sweepRecordTakes()
	rel := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/recordlinks"), "/")
	if rel == "" {
		switch r.Method {
		case http.MethodGet:
			links, err := s.Store.ListRecordLinks()
			if err != nil {
				http.Error(w, "cannot list recording links", http.StatusInternalServerError)
				return
			}
			base := s.memoBase(r)
			out := make([]RecordLinkView, 0, len(links))
			for i := range links {
				out = append(out, s.recordLinkView(&links[i], base))
			}
			writeSecretJSON(w, http.StatusOK, map[string]any{"links": out})
		case http.MethodPost:
			s.createRecordLink(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	id, action, _ := strings.Cut(rel, "/")
	if id == "drafts" { // link IDs start with recordLinkIDPrefix
		s.handleRecordDrafts(w, r, action)
		return
	}
	link, err := s.Store.GetRecordLink(id)
	if err != nil {
		http.Error(w, "recording link not found", http.StatusNotFound)
		return
	}
	switch {
	case action == "" && r.Method == http.MethodGet:
		writeSecretJSON(w, http.StatusOK, s.recordLinkView(link, s.memoBase(r)))
	case action == "" && r.Method == http.MethodPatch:
		s.updateRecordLink(w, r, link)
	case action == "" && r.Method == http.MethodDelete:
		if err := s.Store.DeleteRecordLink(link.ID); errors.Is(err, store.ErrLinkHasSubmissions) {
			http.Error(w, "delete this link's recordings first", http.StatusConflict)
			return
		} else if err != nil {
			http.Error(w, "cannot delete the recording link", http.StatusInternalServerError)
			return
		}
		slog.Info("Record link deleted", "linkId", link.ID)
		w.WriteHeader(http.StatusNoContent)
	case (action == "rotate" || action == "revoke") && r.Method == http.MethodPost:
		nonce, hash := "", ""
		if action == "rotate" {
			if nonce, hash, err = s.newRecordSecret(link.ID); err != nil {
				http.Error(w, "cannot rotate the recording link", http.StatusInternalServerError)
				return
			}
		}
		if err := s.Store.SetRecordLinkSecret(link.ID, nonce, hash); err != nil {
			http.Error(w, "cannot update the recording link", http.StatusInternalServerError)
			return
		}
		slog.Info("Record link "+action+"d", "linkId", link.ID)
		link, _ = s.Store.GetRecordLink(link.ID)
		writeSecretJSON(w, http.StatusOK, s.recordLinkView(link, s.memoBase(r)))
	case strings.HasPrefix(action, "submissions/") && r.Method == http.MethodDelete:
		s.deleteSubmission(w, link, strings.TrimPrefix(action, "submissions/"))
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// recordLinkInput is the create/update body. Durations are seconds; bytes
// are megabytes on the wire (simpler for the form).
type recordLinkInput struct {
	Title          *string  `json:"title"`
	Prompt         *string  `json:"prompt"`
	RoomID         *string  `json:"roomId"`
	MaxDurationSec *int     `json:"maxDurationSec"`
	ExpiresAt      *string  `json:"expiresAt"`
	ExpiresInHours *float64 `json:"expiresInHours"`
	MaxSubmissions *int     `json:"maxSubmissions"`
	MaxTotalMB     *int64   `json:"maxTotalMB"`
	// Recording is the take preset (bitrate, codec, fps, mic mode).
	Recording *store.SessionSettings `json:"recording"`
}

func cleanPrompt(p string) (string, bool) {
	p = strings.ReplaceAll(strings.ToValidUTF8(p, ""), "\r\n", "\n")
	p = strings.ReplaceAll(p, "\x00", "")
	return p, len(p) <= maxRecordPrompt
}

// apply validates in and writes it onto l. creating=true allows roomId.
func (s *Server) applyRecordLinkInput(in *recordLinkInput, l *store.RecordLink, creating bool, now time.Time) error {
	if in.Title != nil {
		t, ok := cleanName(*in.Title, maxRecordTitle)
		if !ok {
			return errors.New("title must be 1-120 characters without control characters")
		}
		l.Title = t
	}
	if in.Prompt != nil {
		p, ok := cleanPrompt(*in.Prompt)
		if !ok {
			return errors.New("the prompt is longer than 64 KB")
		}
		l.Prompt = p
	}
	if in.RoomID != nil {
		if !creating {
			return errors.New("a link's room is fixed at creation")
		}
		if *in.RoomID != "" {
			if _, err := s.Store.GetRoom(*in.RoomID); err != nil {
				return errors.New("room not found")
			}
		}
		l.RoomID = *in.RoomID
	}
	if in.MaxDurationSec != nil {
		d := time.Duration(*in.MaxDurationSec) * time.Second
		if d < minRecordDuration || d > maxRecordDuration {
			return fmt.Errorf("maxDurationSec must be %d-%d", int(minRecordDuration.Seconds()), int(maxRecordDuration.Seconds()))
		}
		l.MaxDurationSec = *in.MaxDurationSec
	}
	switch {
	case in.ExpiresAt != nil && in.ExpiresInHours != nil:
		return errors.New("give expiresAt or expiresInHours, not both")
	case in.ExpiresAt != nil:
		t, err := time.Parse(time.RFC3339, *in.ExpiresAt)
		if err != nil {
			return errors.New("expiresAt must be an RFC 3339 time")
		}
		// A host may expire a link now (or in the past) when editing it;
		// a new link must be usable.
		if (creating && !t.After(now)) || t.After(now.Add(maxRecordExpiry)) {
			return errors.New("expiresAt must be in the future and within 90 days")
		}
		l.ExpiresAt = t.UTC()
	case in.ExpiresInHours != nil:
		d := time.Duration(*in.ExpiresInHours * float64(time.Hour))
		if d <= 0 || d > maxRecordExpiry {
			return errors.New("expiresInHours must be more than 0 and at most 90 days")
		}
		l.ExpiresAt = now.Add(d).UTC()
	}
	if in.MaxSubmissions != nil {
		if *in.MaxSubmissions < 1 || *in.MaxSubmissions > maxRecordSubmissionsCap {
			return fmt.Errorf("maxSubmissions must be 1-%d", maxRecordSubmissionsCap)
		}
		l.MaxSubmissions = *in.MaxSubmissions
	}
	if in.MaxTotalMB != nil {
		b := *in.MaxTotalMB << 20
		if b < minRecordBytes || b > maxRecordBytes || *in.MaxTotalMB > maxRecordBytes>>20 {
			return fmt.Errorf("maxTotalMB must be %d-%d", minRecordBytes>>20, maxRecordBytes>>20)
		}
		l.MaxTotalBytes = b
	}
	if in.Recording != nil {
		var patch roomSettingsPatch
		patch.Recording = in.Recording
		var dst store.RoomSettings
		if err := patch.apply(&dst); err != nil {
			return err
		}
		l.Recording = *dst.Recording
	}
	return nil
}

func (s *Server) createRecordLink(w http.ResponseWriter, r *http.Request) {
	var in recordLinkInput
	if !decodeBody(w, r, &in) {
		return
	}
	if in.Title == nil {
		http.Error(w, "a title is required", http.StatusBadRequest)
		return
	}
	idHex, err := randomHex(6)
	if err != nil {
		http.Error(w, "cannot create the recording link", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	l := &store.RecordLink{ID: recordLinkIDPrefix + idHex, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(defaultRecordExpiry),
		MaxDurationSec: int(defaultRecordDuration.Seconds()), MaxSubmissions: defaultRecordSubmissions, MaxTotalBytes: defaultRecordBytes,
		Recording: store.DefaultSessionSettings()}
	if err := s.applyRecordLinkInput(&in, l, true, now); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if l.Nonce, l.SecretHash, err = s.newRecordSecret(l.ID); err != nil {
		http.Error(w, "cannot create the recording link", http.StatusInternalServerError)
		return
	}
	if err := s.Store.CreateRecordLink(l); err != nil {
		slog.Error("create record link failed", "err", err)
		http.Error(w, "cannot create the recording link", http.StatusInternalServerError)
		return
	}
	slog.Info("Record link created", "linkId", l.ID, "roomId", l.RoomID)
	writeSecretJSON(w, http.StatusCreated, s.recordLinkView(l, s.memoBase(r)))
}

func (s *Server) updateRecordLink(w http.ResponseWriter, r *http.Request, l *store.RecordLink) {
	var in recordLinkInput
	if !decodeBody(w, r, &in) {
		return
	}
	if err := s.applyRecordLinkInput(&in, l, false, time.Now().UTC()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.Store.UpdateRecordLink(l); err != nil {
		http.Error(w, "cannot update the recording link", http.StatusInternalServerError)
		return
	}
	writeSecretJSON(w, http.StatusOK, s.recordLinkView(l, s.memoBase(r)))
}

// deleteSubmission removes a submission and its files. Its participant grant
// stops working at once (the participant row is gone).
func (s *Server) deleteSubmission(w http.ResponseWriter, link *store.RecordLink, sessionID string) {
	sub, err := s.Store.GetSubmission(sessionID)
	if err != nil || sub.LinkID != link.ID {
		http.Error(w, "recording not found", http.StatusNotFound)
		return
	}
	if err := s.Store.DeleteSubmissionSession(sub.SessionID); err != nil {
		slog.Error("delete submission failed", "sessionId", sub.SessionID, "err", err)
		http.Error(w, "cannot delete the recording", http.StatusInternalServerError)
		return
	}
	if err := os.RemoveAll(filepath.Join(s.Config.DataDir, "sessions", sub.SessionID)); err != nil {
		slog.Warn("remove submission files failed", "sessionId", sub.SessionID, "err", err)
	}
	slog.Info("Record submission deleted", "linkId", link.ID, "sessionId", sub.SessionID)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- guest API

// recordAuth checks the link secret (Authorization: Bearer). Every failure
// but expiry is the same 401, so the endpoint does not reveal which links
// exist; an expired link with its correct secret says so (410).
func (s *Server) recordAuth(w http.ResponseWriter, r *http.Request, linkID string) (*store.RecordLink, bool) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	link, err := s.Store.GetRecordLink(linkID)
	if err != nil || !link.LinkEnabled() || !tokenHashMatches(token, link.SecretHash) {
		http.Error(w, errRecordLinkInvalid, http.StatusUnauthorized)
		return nil, false
	}
	if link.Expired(time.Now()) {
		http.Error(w, errRecordLinkExpired, http.StatusGone)
		return nil, false
	}
	return link, true
}

// submissionAuth additionally checks the submission key.
func (s *Server) submissionAuth(w http.ResponseWriter, r *http.Request, link *store.RecordLink, sessionID string) (*store.RecordSubmission, bool) {
	sub, err := s.Store.GetSubmission(sessionID)
	if err != nil || sub.LinkID != link.ID || !tokenHashMatches(r.Header.Get(submissionKeyHeader), sub.KeyHash) {
		http.Error(w, "this recording isn’t available from this browser", http.StatusNotFound)
		return nil, false
	}
	return sub, true
}

// handleRecordGuest serves /api/v1/record/{linkId}[...]: the guest side,
// reachable through Serve and Funnel.
func (s *Server) handleRecordGuest(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/record/"), "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	linkID := parts[0]
	// Opening a link and starting a submission are join-like: rate-limited
	// per client address and per link before any database work [SL-SEC-1].
	if len(parts) == 1 || (len(parts) == 2 && parts[1] == "submissions") {
		if !s.allowJoin(r, "record:"+linkID) {
			w.Header().Set("Retry-After", "10")
			http.Error(w, "too many attempts; wait a moment and reopen the link", http.StatusTooManyRequests)
			return
		}
	}
	link, ok := s.recordAuth(w, r, linkID)
	if !ok {
		return
	}
	s.sweepRecordTakes()
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		s.recordLinkInfo(w, link)
	case len(parts) == 2 && parts[1] == "submissions" && r.Method == http.MethodPost:
		s.createSubmission(w, r, link)
	case len(parts) >= 3 && parts[1] == "submissions":
		sub, ok := s.submissionAuth(w, r, link, parts[2])
		if !ok {
			return
		}
		rest := parts[3:]
		switch {
		case len(rest) == 0 && r.Method == http.MethodGet:
			writeSecretJSON(w, http.StatusOK, s.guestSubmission(link, sub))
		case len(rest) == 1 && rest[0] == "clock" && r.Method == http.MethodPost:
			s.recordClock(w, r)
		case len(rest) == 1 && rest[0] == "takes" && r.Method == http.MethodPost:
			s.startSoloTake(w, link, sub)
		case len(rest) == 3 && rest[0] == "takes" && rest[2] == "stop" && r.Method == http.MethodPost:
			s.stopSoloTakeRequest(w, link, sub, rest[1])
		case len(rest) == 2 && rest[0] == "takes" && r.Method == http.MethodDelete:
			s.discardSoloTake(w, sub, rest[1])
		case len(rest) == 1 && rest[0] == "submit" && r.Method == http.MethodPost:
			s.submit(w, link, sub)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (s *Server) recordLinkInfo(w http.ResponseWriter, link *store.RecordLink) {
	submitted, _, err := s.Store.CountSubmissions(link.ID, time.Now().Add(-recordDraftWindow))
	if err != nil {
		http.Error(w, "cannot read the recording link", http.StatusInternalServerError)
		return
	}
	info := RecordLinkPublic{ID: link.ID, Title: link.Title, Prompt: link.Prompt, MaxDurationSec: link.MaxDurationSec,
		ExpiresAt: link.ExpiresAt, Accepting: submitted < link.MaxSubmissions, Recording: link.Recording}
	if !info.Accepting {
		info.Reason = "full"
	}
	writeSecretJSON(w, http.StatusOK, info)
}

func (s *Server) createSubmission(w http.ResponseWriter, r *http.Request, link *store.RecordLink) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	name := signaling.SanitizeDisplayText(req.Name, 80)
	if name == "" {
		http.Error(w, "enter your name", http.StatusBadRequest)
		return
	}
	submitted, drafts, err := s.Store.CountSubmissions(link.ID, time.Now().Add(-recordDraftWindow))
	if err != nil {
		http.Error(w, "cannot start a recording", http.StatusInternalServerError)
		return
	}
	if submitted >= link.MaxSubmissions {
		http.Error(w, "this recording link has all the recordings it can take", http.StatusConflict)
		return
	}
	if drafts >= recordDraftsOpen {
		http.Error(w, "too many recordings are in progress on this link; try again later", http.StatusTooManyRequests)
		return
	}
	key, err := randomHex(32)
	if err != nil {
		http.Error(w, "cannot start a recording", http.StatusInternalServerError)
		return
	}
	invite, err := randomHex(32)
	if err != nil {
		http.Error(w, "cannot start a recording", http.StatusInternalServerError)
		return
	}
	preset, _ := json.Marshal(link.Recording)
	now := time.Now().UTC()
	sess := &store.Session{ID: uuid.New().String()[:8], Title: link.Title + " — " + name, CreatedAt: now, PresetJSON: string(preset),
		// Never used: the session is reached through the link only.
		InviteTokenHash: hashToken(invite)}
	ua := r.UserAgent()
	if len(ua) > 300 {
		ua = ua[:300]
	}
	part := &store.Participant{ID: "p_" + uuid.NewString(), SessionID: sess.ID, Name: name, Role: signaling.RoleGuest, Status: "admitted", JoinedAt: now, UserAgent: ua}
	sub := &store.RecordSubmission{SessionID: sess.ID, LinkID: link.ID, ParticipantID: part.ID, Name: name, KeyHash: hashToken(key), Status: store.SubmissionDraft, CreatedAt: now}
	if err := s.Store.CreateSubmission(sess, part, sub); err != nil {
		slog.Error("create submission failed", "linkId", link.ID, "err", err)
		http.Error(w, "cannot start a recording", http.StatusInternalServerError)
		return
	}
	slog.Info("Record submission started", "linkId", link.ID, "sessionId", sess.ID)
	writeSecretJSON(w, http.StatusCreated, map[string]any{"key": key, "submission": s.guestSubmission(link, sub)})
}

// recordClock answers one clock ping over HTTP, so the solo recorder's
// sidecars carry the same host-clock fit as a call's.
func (s *Server) recordClock(w http.ResponseWriter, r *http.Request) {
	t1 := clock.MonotonicNowUs()
	var ping signaling.ClockPingPayload
	if !decodeBody(w, r, &ping) {
		return
	}
	writeSecretJSON(w, http.StatusOK, signaling.ClockPongPayload{T0: ping.T0, T1: t1, T2: clock.MonotonicNowUs()})
}

// RecordTakeStart is the server's answer to a solo take start.
type RecordTakeStart struct {
	TakeID         string `json:"takeId"`
	Number         int    `json:"number"`
	StartAtHostUs  int64  `json:"startAtHostUs"`
	MaxDurationSec int    `json:"maxDurationSec"`
}

func (s *Server) startSoloTake(w http.ResponseWriter, link *store.RecordLink, sub *store.RecordSubmission) {
	if sub.Status != store.SubmissionDraft {
		http.Error(w, "this recording was already submitted", http.StatusConflict)
		return
	}
	s.recordMu.Lock()
	defer s.recordMu.Unlock()
	takes, err := s.Store.ListTakesBySession(sub.SessionID)
	if err != nil {
		http.Error(w, "cannot start a take", http.StatusInternalServerError)
		return
	}
	for _, tk := range takes {
		if tk.Status == "recording" {
			http.Error(w, "a take is already recording", http.StatusConflict)
			return
		}
	}
	if len(takes) >= recordLiveTakes {
		http.Error(w, fmt.Sprintf("you can keep at most %d takes; delete one to record another", recordLiveTakes), http.StatusConflict)
		return
	}
	if sub.TakesStarted >= recordStartedTakes {
		http.Error(w, "this recording has reached its number of takes", http.StatusTooManyRequests)
		return
	}
	n, err := s.Store.NextSubmissionTake(sub.SessionID)
	if err != nil {
		http.Error(w, "cannot start a take", http.StatusInternalServerError)
		return
	}
	start := clock.MonotonicNowUs()
	tk := &store.Take{ID: fmt.Sprintf("take_%s_%02d", sub.SessionID, n), SessionID: sub.SessionID, Number: n, StartAtHostUs: start,
		WallClockStart: clock.HostWallClockFromMonotonic(start), Status: "recording"}
	// The expected-recorder snapshot is just this guest.
	if err := s.Store.CreateTakeWithParticipants(tk, []string{sub.ParticipantID}); err != nil {
		slog.Error("create solo take failed", "sessionId", sub.SessionID, "err", err)
		http.Error(w, "cannot start a take", http.StatusInternalServerError)
		return
	}
	limit := time.Duration(link.MaxDurationSec)*time.Second + recordTakeGrace
	time.AfterFunc(limit, func() { s.stopSoloTake(tk.ID, link.MaxDurationSec, "limit") })
	slog.Info("Solo take started", "linkId", link.ID, "takeId", tk.ID)
	writeSecretJSON(w, http.StatusCreated, RecordTakeStart{TakeID: tk.ID, Number: n, StartAtHostUs: start, MaxDurationSec: link.MaxDurationSec})
}

// stopSoloTake ends a recording take, never later than its limit. The stop
// time fixes the take's duration and so its upload quota.
func (s *Server) stopSoloTake(takeID string, maxDurationSec int, why string) *store.Take {
	s.recordMu.Lock()
	defer s.recordMu.Unlock()
	tk, err := s.Store.GetTake(takeID)
	if err != nil {
		return nil
	}
	if tk.Status != "recording" {
		return tk
	}
	limitUs := tk.StartAtHostUs + int64(maxDurationSec)*1_000_000 + recordTakeGrace.Microseconds()
	stop := clock.MonotonicNowUs()
	// Past the limit (or after a daemon restart, when the monotonic clock
	// started over): the take ends at its limit.
	if stop > limitUs || stop < tk.StartAtHostUs || time.Since(tk.WallClockStart) > time.Duration(maxDurationSec)*time.Second+recordTakeGrace {
		stop = limitUs
	}
	tk.StopAtHostUs, tk.Status = stop, "stopped"
	if err := s.Store.UpdateTake(tk); err != nil {
		slog.Error("stop solo take failed", "takeId", takeID, "err", err)
		return nil
	}
	slog.Info("Solo take stopped", "takeId", takeID, "reason", why)
	return tk
}

// sweepRecordTakes stops solo takes that outlived their link's limit (the
// timer is lost when the daemon restarts).
func (s *Server) sweepRecordTakes() {
	takes, err := s.Store.ListRecordingSoloTakes()
	if err != nil {
		return
	}
	for _, tk := range takes {
		if time.Since(tk.WallClockStart) > time.Duration(tk.MaxDurationSec)*time.Second+recordTakeGrace {
			s.stopSoloTake(tk.TakeID, tk.MaxDurationSec, "limit")
		}
	}
}

func (s *Server) soloTake(w http.ResponseWriter, sub *store.RecordSubmission, takeID string) (*store.Take, bool) {
	tk, err := s.Store.GetTake(takeID)
	if err != nil || tk.SessionID != sub.SessionID {
		http.Error(w, "take not found", http.StatusNotFound)
		return nil, false
	}
	return tk, true
}

func (s *Server) stopSoloTakeRequest(w http.ResponseWriter, link *store.RecordLink, sub *store.RecordSubmission, takeID string) {
	if _, ok := s.soloTake(w, sub, takeID); !ok {
		return
	}
	tk := s.stopSoloTake(takeID, link.MaxDurationSec, "guest")
	if tk == nil {
		http.Error(w, "cannot stop the take", http.StatusInternalServerError)
		return
	}
	writeSecretJSON(w, http.StatusOK, map[string]any{"takeId": tk.ID, "status": tk.Status, "durationSec": takeDurationSec(tk)})
}

// discardSoloTake deletes a draft's take and its files (re-record).
func (s *Server) discardSoloTake(w http.ResponseWriter, sub *store.RecordSubmission, takeID string) {
	if sub.Status != store.SubmissionDraft {
		http.Error(w, "a submitted recording can’t be changed", http.StatusConflict)
		return
	}
	if _, ok := s.soloTake(w, sub, takeID); !ok {
		return
	}
	s.recordMu.Lock()
	err := s.Store.DeleteTake(takeID)
	s.recordMu.Unlock()
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "cannot delete the take", http.StatusInternalServerError)
		return
	}
	if err := os.RemoveAll(filepath.Join(s.Config.DataDir, "sessions", sub.SessionID, "takes", takeID)); err != nil {
		slog.Warn("remove discarded take files failed", "takeId", takeID, "err", err)
	}
	slog.Info("Solo take discarded", "sessionId", sub.SessionID, "takeId", takeID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) submit(w http.ResponseWriter, link *store.RecordLink, sub *store.RecordSubmission) {
	if sub.Status == store.SubmissionSubmitted {
		writeSecretJSON(w, http.StatusOK, s.guestSubmission(link, sub))
		return
	}
	takes, err := s.Store.ListTakesBySession(sub.SessionID)
	if err != nil {
		http.Error(w, "cannot submit", http.StatusInternalServerError)
		return
	}
	if len(takes) == 0 {
		http.Error(w, "record a take before submitting", http.StatusConflict)
		return
	}
	for _, tk := range takes {
		if tk.Status == "recording" {
			http.Error(w, "stop recording before submitting", http.StatusConflict)
			return
		}
	}
	if err := s.Store.SubmitSubmission(sub.SessionID, link.ID, link.MaxSubmissions, time.Now()); errors.Is(err, store.ErrSubmissionsFull) {
		http.Error(w, "this recording link has all the recordings it can take", http.StatusConflict)
		return
	} else if err != nil {
		http.Error(w, "cannot submit", http.StatusInternalServerError)
		return
	}
	slog.Info("Record submission submitted", "linkId", link.ID, "sessionId", sub.SessionID, "takes", len(takes))
	sub, _ = s.Store.GetSubmission(sub.SessionID)
	writeSecretJSON(w, http.StatusOK, s.guestSubmission(link, sub))
}

// serveRecordPage serves the guest page at /record.
func (s *Server) serveRecordPage(w http.ResponseWriter, r *http.Request) {
	if s.WebFS == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFileFS(w, r, s.WebFS, "record.html")
}
