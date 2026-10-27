package httpserver

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
	"github.com/omniharmonic/studiolink/host/internal/tailscale"
)

// Persistent rooms [SL-P1-5]. A room's guest link is
//
//	<guest base>/join?room=<slug>&token=<secret>
//
// where secret = hex(HMAC-SHA256(daemon secret key, roomID, nonce)): 256
// bits, never stored. The database keeps only the random nonce and
// SHA-256(secret), so the host dashboard can show the link again while the
// database alone cannot reproduce it. Rotating replaces the nonce (the old link
// stops working at once); revoking clears both. The link grants the waiting
// room of the room's live meeting only; admission is still the host's call
// unless the room's auto-admit setting is on (default off).

const (
	maxRoomNameRunes = 80
	maxHostNameRunes = 60
	recentSessions   = 12
	homeTokenTTL     = 2 * time.Hour
)

var errInvalidName = errors.New("room name must be 1-80 characters without control characters")

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func tokenHashMatches(token, hash string) bool {
	return token != "" && hash != "" && subtle.ConstantTimeCompare([]byte(hashToken(token)), []byte(hash)) == 1
}

// roomSecret derives a room's guest-link secret from its current nonce.
func (s *Server) roomSecret(roomID, nonce string) string {
	m := hmac.New(sha256.New, s.Config.SecretKey)
	m.Write([]byte("studiolink/room-invite/v1\x00" + roomID + "\x00" + nonce))
	return hex.EncodeToString(m.Sum(nil))
}

// newRoomInvite returns a fresh nonce and the hash of the secret it yields.
func (s *Server) newRoomInvite(roomID string) (nonce, secretHash string, err error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("room invite entropy: %w", err)
	}
	nonce = hex.EncodeToString(b)
	return nonce, hashToken(s.roomSecret(roomID, nonce)), nil
}

// guestTokenValid reports whether token admits a guest to sess's waiting
// room: the session's own invite, or, for a meeting opened from a room, the
// room's current guest link (not archived, not revoked or rotated away).
func (s *Server) guestTokenValid(sess *store.Session, token string) bool {
	_, ok := s.guestCredential(sess, token)
	return ok
}

// guestCredential is guestTokenValid that also names which credential matched
// (signaling.Client.JoinCredential), so replacing that credential can deny
// the people still waiting under it.
func (s *Server) guestCredential(sess *store.Session, token string) (string, bool) {
	if token == "" || sess == nil {
		return "", false
	}
	if tokenHashMatches(token, sess.InviteTokenHash) {
		return inviteCredential(sess.InviteTokenHash), true
	}
	if sess.RoomID == "" {
		return "", false
	}
	room, err := s.Store.GetRoom(sess.RoomID)
	if err == nil && !room.Archived && room.LinkEnabled() && tokenHashMatches(token, room.InviteSecretHash) {
		return roomCredential(room.InviteNonce), true
	}
	return "", false
}

func inviteCredential(hash string) string {
	if hash == "" {
		return ""
	}
	return "invite:" + hash
}

func roomCredential(nonce string) string {
	if nonce == "" {
		return ""
	}
	return "room:" + nonce
}

// denyWaitingUnderRoomLink denies people waiting in the room's live meeting
// who joined with the link nonce that was just rotated or revoked (L4).
func (s *Server) denyWaitingUnderRoomLink(room *store.Room, oldNonce string) {
	if s.Hub == nil || oldNonce == "" {
		return
	}
	if live, err := s.Store.LiveRoomSession(room.ID); err == nil {
		s.Hub.DenyPendingWithCredential(live.ID, roomCredential(oldNonce))
	}
}

// remoteGuestBase reports whether base is an HTTPS address a remote guest can use.
func remoteGuestBase(base string) bool {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Hostname() == "localhost" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return ip == nil || !ip.IsLoopback()
}

// guestBase picks the origin for guest links: the configured public URL, the
// discovered Tailscale Serve address, else this dashboard's own origin (which
// only works on this machine; remote is false).
func (s *Server) guestBase(r *http.Request) (base string, remote bool) {
	if remoteGuestBase(s.Config.PublicURL) {
		return strings.TrimRight(s.Config.PublicURL, "/"), true
	}
	if discovered, err := tailscale.NewClient().DiscoverGuestURL(s.Config.HTTPPort); err == nil && remoteGuestBase(discovered) {
		return strings.TrimRight(discovered, "/"), true
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host, false
}

// RoomView is a room as the host dashboard sees it. GuestURL carries the
// link secret: management responses only, never logged.
type RoomView struct {
	store.Room
	LinkEnabled bool `json:"linkEnabled"`
	// LinkLost is set when the stored hash no longer matches what the daemon
	// key derives (the key was replaced): rotate to issue a new link.
	LinkLost       bool   `json:"linkLost,omitempty"`
	GuestURL       string `json:"guestUrl,omitempty"`
	GuestPath      string `json:"guestPath,omitempty"`
	GuestURLRemote bool   `json:"guestUrlRemote"`
	Live           bool   `json:"live"`
	LiveSessionID  string `json:"liveSessionId,omitempty"`
	HostInCall     bool   `json:"hostInCall"`
	// Producer is the room's producer link [SL-P1-11] (secret in its URL).
	Producer signaling.ProducerLinkResponse `json:"producer"`
}

type guestBaseFn func() (string, bool)

func (s *Server) roomView(room *store.Room, base guestBaseFn) RoomView {
	v := RoomView{Room: *room, LinkEnabled: room.LinkEnabled()}
	if v.LinkEnabled {
		secret := s.roomSecret(room.ID, room.InviteNonce)
		if hashToken(secret) != room.InviteSecretHash {
			v.LinkLost = true
		} else {
			v.GuestPath = "/join?" + url.Values{"room": {room.Slug}, "token": {secret}}.Encode()
			b, remote := base()
			v.GuestURL, v.GuestURLRemote = b+v.GuestPath, remote
		}
	}
	if live, err := s.Store.LiveRoomSession(room.ID); err == nil {
		v.Live, v.LiveSessionID = true, live.ID
		v.HostInCall = s.Hub != nil && s.Hub.HostConnected(live.ID)
	}
	v.Producer = s.producerLinkView(store.RoomProducerScope(room.ID), url.Values{"room": {room.Slug}}, base)
	v.Producer.RoomID = room.ID
	return v
}

// memoBase computes the guest base at most once per request (Tailscale
// discovery shells out).
func (s *Server) memoBase(r *http.Request) guestBaseFn {
	var done bool
	var base string
	var remote bool
	return func() (string, bool) {
		if !done {
			base, remote = s.guestBase(r)
			done = true
		}
		return base, remote
	}
}

func cleanName(name string, max int) (string, bool) {
	name = strings.TrimSpace(strings.ToValidUTF8(name, ""))
	if name == "" || utf8.RuneCountInString(name) > max {
		return "", false
	}
	for _, r := range name {
		// Cf covers bidi overrides/isolates that could reorder UI text.
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", false
		}
	}
	return name, true
}

func clampHostName(name string) string {
	return signaling.SanitizeDisplayText(name, maxHostNameRunes)
}

// writeSecretJSON writes a response that may carry link secrets or host
// tokens: never cached.
func writeSecretJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, v)
}

// roomSettingsPatch is the writable subset of RoomSettings.
type roomSettingsPatch struct {
	AutoAdmit   *bool                  `json:"autoAdmit"`
	LockOnStart *bool                  `json:"lockOnStart"`
	Recording   *store.SessionSettings `json:"recording"`
}

func (p *roomSettingsPatch) apply(dst *store.RoomSettings) error {
	if p == nil {
		return nil
	}
	if p.AutoAdmit != nil {
		dst.AutoAdmit = *p.AutoAdmit
	}
	if p.LockOnStart != nil {
		dst.LockOnStart = *p.LockOnStart
	}
	if p.Recording != nil {
		rec := store.DefaultSessionSettings()
		in := p.Recording
		if in.VideoBitrateBps < 0 || in.TargetFPS < 0 || in.AudioSampleRate < 0 || in.AudioBitDepth < 0 || in.XcorrThreshold < 0 || in.XcorrWindowMs < 0 {
			return errors.New("recording settings must not be negative")
		}
		if in.VideoBitrateBps > 0 {
			rec.VideoBitrateBps = in.VideoBitrateBps
		}
		if in.VideoCodec != "" {
			rec.VideoCodec = in.VideoCodec
		}
		if in.TargetFPS > 0 {
			rec.TargetFPS = in.TargetFPS
		}
		if in.AudioSampleRate > 0 {
			rec.AudioSampleRate = in.AudioSampleRate
		}
		if in.AudioBitDepth > 0 {
			rec.AudioBitDepth = in.AudioBitDepth
		}
		if in.XcorrThreshold > 0 {
			rec.XcorrThreshold = in.XcorrThreshold
		}
		if in.XcorrWindowMs > 0 {
			rec.XcorrWindowMs = in.XcorrWindowMs
		}
		if in.Engine != "" {
			rec.Engine = in.Engine
		}
		if in.RecordingMicMode != "" {
			if !signaling.ValidRecordingMicMode(in.RecordingMicMode) {
				return errors.New("recording_mic_mode must be auto, raw or clean")
			}
			rec.RecordingMicMode = in.RecordingMicMode
		}
		dst.Recording = &rec
	}
	return nil
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return false
	}
	return true
}

// handleRooms serves /api/v1/rooms and /api/v1/rooms/... .
func (s *Server) handleRooms(w http.ResponseWriter, r *http.Request) {
	rel := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/rooms"), "/")
	// The lobby is the only guest-facing room endpoint (reachable through
	// Serve/Funnel); everything else is host management.
	if rel == "lobby" {
		s.handleRoomLobby(w, r)
		return
	}
	if !s.adminAllowed(r) {
		adminDenied(w, r, "Managing rooms")
		return
	}
	if rel == "" {
		switch r.Method {
		case http.MethodGet:
			rooms, err := s.Store.ListRooms(r.URL.Query().Get("archived") == "1")
			if err != nil {
				slog.Error("list rooms failed", "err", err)
				http.Error(w, "cannot list rooms", http.StatusInternalServerError)
				return
			}
			base := s.memoBase(r)
			out := make([]RoomView, 0, len(rooms))
			for i := range rooms {
				out = append(out, s.roomView(&rooms[i], base))
			}
			writeSecretJSON(w, http.StatusOK, map[string]any{"rooms": out})
		case http.MethodPost:
			s.createRoom(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	id, action, _ := strings.Cut(rel, "/")
	room, err := s.Store.GetRoom(id)
	if err != nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}
	switch {
	case action == "" && r.Method == http.MethodGet:
		writeSecretJSON(w, http.StatusOK, s.roomView(room, s.memoBase(r)))
	case action == "" && r.Method == http.MethodPatch:
		s.updateRoom(w, r, room)
	case action == "" && r.Method == http.MethodDelete:
		if _, err := s.Store.LiveRoomSession(room.ID); err == nil {
			http.Error(w, "end the room's meeting before deleting it", http.StatusConflict)
			return
		}
		if err := s.Store.DeleteRoom(room.ID); err != nil {
			http.Error(w, "cannot delete room", http.StatusInternalServerError)
			return
		}
		slog.Info("Room deleted", "roomId", room.ID)
		w.WriteHeader(http.StatusNoContent)
	case action == "rotate" && r.Method == http.MethodPost:
		nonce, hash, err := s.newRoomInvite(room.ID)
		if err == nil {
			err = s.Store.SetRoomInvite(room.ID, nonce, hash)
		}
		if err != nil {
			http.Error(w, "cannot rotate the guest link", http.StatusInternalServerError)
			return
		}
		slog.Info("Room guest link rotated", "roomId", room.ID)
		s.denyWaitingUnderRoomLink(room, room.InviteNonce)
		s.roomLinkChanged(room.ID)
		room, _ = s.Store.GetRoom(room.ID)
		writeSecretJSON(w, http.StatusOK, s.roomView(room, s.memoBase(r)))
	case action == "revoke" && r.Method == http.MethodPost:
		if err := s.Store.SetRoomInvite(room.ID, "", ""); err != nil {
			http.Error(w, "cannot revoke the guest link", http.StatusInternalServerError)
			return
		}
		slog.Info("Room guest link revoked", "roomId", room.ID)
		s.denyWaitingUnderRoomLink(room, room.InviteNonce)
		s.roomLinkChanged(room.ID)
		room, _ = s.Store.GetRoom(room.ID)
		writeSecretJSON(w, http.StatusOK, s.roomView(room, s.memoBase(r)))
	case action == "start" && r.Method == http.MethodPost:
		s.startRoomMeeting(w, r, room)
	case action == "branding" || action == "branding/logo":
		s.handleRoomBranding(w, r, room, strings.TrimPrefix(strings.TrimPrefix(action, "branding"), "/"))
	case action == "titles":
		s.handleRoomTitles(w, r, room)
	case strings.HasPrefix(action, "producer/"):
		s.handleRoomProducer(w, r, room, strings.TrimPrefix(action, "producer/"))
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (s *Server) createRoom(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string             `json:"name"`
		Settings *roomSettingsPatch `json:"settings"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	name, ok := cleanName(req.Name, maxRoomNameRunes)
	if !ok {
		http.Error(w, errInvalidName.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	idBytes := make([]byte, 6)
	if _, err := rand.Read(idBytes); err != nil {
		http.Error(w, "cannot create room", http.StatusInternalServerError)
		return
	}
	room := &store.Room{ID: "room_" + hex.EncodeToString(idBytes), Name: name, CreatedAt: now, UpdatedAt: now}
	if err := req.Settings.apply(&room.Settings); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	nonce, hash, err := s.newRoomInvite(room.ID)
	if err != nil {
		http.Error(w, "cannot create room", http.StatusInternalServerError)
		return
	}
	room.InviteNonce, room.InviteSecretHash = nonce, hash
	if err := s.Store.CreateRoom(room); err != nil {
		slog.Error("create room failed", "err", err)
		http.Error(w, "cannot create room", http.StatusInternalServerError)
		return
	}
	slog.Info("Room created", "roomId", room.ID, "slug", room.Slug)
	writeSecretJSON(w, http.StatusCreated, s.roomView(room, s.memoBase(r)))
}

func (s *Server) updateRoom(w http.ResponseWriter, r *http.Request, room *store.Room) {
	var req struct {
		Name     *string            `json:"name"`
		Settings *roomSettingsPatch `json:"settings"`
		Archived *bool              `json:"archived"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Name != nil {
		name, ok := cleanName(*req.Name, maxRoomNameRunes)
		if !ok {
			http.Error(w, errInvalidName.Error(), http.StatusBadRequest)
			return
		}
		room.Name = name
	}
	if err := req.Settings.apply(&room.Settings); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Archived != nil {
		if *req.Archived && !room.Archived {
			if _, err := s.Store.LiveRoomSession(room.ID); err == nil {
				http.Error(w, "end the room's meeting before archiving it", http.StatusConflict)
				return
			}
		}
		room.Archived = *req.Archived
	}
	if err := s.Store.UpdateRoom(room); err != nil {
		http.Error(w, "cannot update room", http.StatusInternalServerError)
		return
	}
	writeSecretJSON(w, http.StatusOK, s.roomView(room, s.memoBase(r)))
}

// RoomStartResponse opens (or rejoins) a room's meeting as host.
type RoomStartResponse struct {
	SessionID string `json:"sessionId"`
	Title     string `json:"title"`
	RoomID    string `json:"roomId"`
	HostToken string `json:"hostToken"`
	HostURL   string `json:"hostUrl"`
	// Resumed is true when the room already had a meeting open.
	Resumed bool `json:"resumed"`
}

func (s *Server) startRoomMeeting(w http.ResponseWriter, r *http.Request, room *store.Room) {
	var req struct {
		HostName string `json:"hostName"`
	}
	if r.ContentLength != 0 && !decodeBody(w, r, &req) {
		return
	}
	if room.Archived {
		http.Error(w, "restore this room before starting a meeting in it", http.StatusConflict)
		return
	}
	s.roomMu.Lock()
	defer s.roomMu.Unlock()
	resp := RoomStartResponse{RoomID: room.ID, Title: room.Name}
	if live, err := s.Store.LiveRoomSession(room.ID); err == nil {
		resp.SessionID, resp.Resumed = live.ID, true
	} else if !errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "cannot read the room's meeting", http.StatusInternalServerError)
		return
	} else {
		preset := `{"preset":"1080p30"}`
		if room.Settings.Recording != nil {
			raw, _ := json.Marshal(room.Settings.Recording)
			preset = string(raw)
		}
		// Meetings in a room are joined with the room link; this per-meeting
		// invite stays unguessable and unused unless the host mints one.
		tok := make([]byte, 32)
		if _, err := rand.Read(tok); err != nil {
			http.Error(w, "cannot start meeting", http.StatusInternalServerError)
			return
		}
		sess := &store.Session{
			ID:              uuid.New().String()[:8],
			Title:           room.Name,
			CreatedAt:       time.Now().UTC(),
			PresetJSON:      preset,
			InviteTokenHash: hashToken(hex.EncodeToString(tok)),
			AutoAdmit:       room.Settings.AutoAdmit,
			RoomID:          room.ID,
		}
		if err := s.Store.CreateSession(sess); err != nil {
			slog.Error("create room meeting failed", "roomId", room.ID, "err", err)
			http.Error(w, "cannot start meeting", http.StatusInternalServerError)
			return
		}
		if room.Settings.LockOnStart && s.Hub != nil {
			s.Hub.SetRoomLock(sess.ID, true)
		}
		resp.SessionID = sess.ID
		slog.Info("Room meeting started", "roomId", room.ID, "sessionId", sess.ID)
	}
	if name := clampHostName(req.HostName); name != "" && name != room.Settings.HostName {
		room.Settings.HostName = name
		if err := s.Store.UpdateRoom(room); err != nil {
			slog.Warn("remember room host name failed", "roomId", room.ID, "err", err)
		}
	}
	resp.HostToken = signaling.MintHostToken(resp.SessionID, 24*time.Hour, s.Config.SecretKey)
	resp.HostURL = fmt.Sprintf("http://%s/?%s", r.Host, url.Values{"session": {resp.SessionID}, "token": {resp.HostToken}}.Encode())
	writeSecretJSON(w, http.StatusOK, resp)
}

// RoomLobby is what a guest holding a room link may know: the room's name,
// who hosts it, and whether a meeting is ready to take join requests.
type RoomLobby struct {
	Name      string `json:"name"`
	HostName  string `json:"hostName,omitempty"`
	Live      bool   `json:"live"`
	SessionID string `json:"sessionId,omitempty"`
	// Role is "producer" when the link is the room's producer link [SL-P1-11].
	Role string `json:"role,omitempty"`
}

// handleRoomLobby (GET /api/v1/rooms/lobby?room=<slug>, Authorization: Bearer
// <room link secret>) is guest-facing. Every failure is the same 401 so the
// endpoint does not reveal which rooms exist.
func (s *Server) handleRoomLobby(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	room, err := s.Store.GetRoomBySlug(r.URL.Query().Get("room"))
	role := ""
	valid := err == nil && !room.Archived && room.LinkEnabled() && tokenHashMatches(token, room.InviteSecretHash)
	if !valid && err == nil && !room.Archived {
		// The room's producer link waits in the same lobby [SL-P1-11].
		if _, hash, perr := s.Store.ProducerLink(store.RoomProducerScope(room.ID)); perr == nil && tokenHashMatches(token, hash) {
			valid, role = true, signaling.RoleProducer
		}
	}
	if !valid {
		http.Error(w, "this room link is not valid any more; ask the host for a new one", http.StatusUnauthorized)
		return
	}
	lobby := RoomLobby{Name: room.Name, HostName: room.Settings.HostName, Role: role}
	// Guests go to admission only once the host is actually in the call, so
	// their join request has someone to answer it.
	if live, err := s.Store.LiveRoomSession(room.ID); err == nil && s.Hub != nil && s.Hub.HostConnected(live.ID) {
		lobby.Live, lobby.SessionID = true, live.ID
	}
	writeSecretJSON(w, http.StatusOK, lobby)
}

// handleSessionEnd (POST /api/v1/sessions/{id}/end, host only) ends a meeting.
func (s *Server) handleSessionEnd(w http.ResponseWriter, r *http.Request, sessID string) {
	if !s.requireHost(w, r, sessID) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sess, err := s.Store.GetSession(sessID)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	if s.Hub != nil && s.Hub.IsRecording(sessID) {
		http.Error(w, "stop recording before ending the meeting", http.StatusConflict)
		return
	}
	if sess.EndedAt == nil {
		if err := s.Store.EndSession(sessID, time.Now()); err != nil && !errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "cannot end the meeting", http.StatusInternalServerError)
			return
		}
		if s.Hub != nil {
			s.Hub.EndSession(sessID, signaling.SessionEndByHost)
		}
	}
	writeSecretJSON(w, http.StatusOK, map[string]any{"ended": true, "roomId": sess.RoomID})
}

// HomeSession is a recent meeting on the host home screen. HostToken is a
// short-lived host credential (management responses only) for rejoining a
// live meeting or downloading its exports.
type HomeSession struct {
	store.SessionSummary
	Live      bool   `json:"live"`
	HostToken string `json:"hostToken"`
}

// handleHome (GET /api/v1/home, management only) feeds the call-first home.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(r) {
		adminDenied(w, r, "The home screen")
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rooms, err := s.Store.ListRooms(false)
	if err != nil {
		http.Error(w, "cannot list rooms", http.StatusInternalServerError)
		return
	}
	base := s.memoBase(r)
	views := make([]RoomView, 0, len(rooms))
	for i := range rooms {
		views = append(views, s.roomView(&rooms[i], base))
	}
	recent, err := s.Store.ListRecentSessions(recentSessions)
	if err != nil {
		http.Error(w, "cannot list recent meetings", http.StatusInternalServerError)
		return
	}
	sessions := make([]HomeSession, 0, len(recent))
	for _, sum := range recent {
		sum.InviteTokenHash = ""
		hs := HomeSession{SessionSummary: sum, HostToken: signaling.MintHostToken(sum.ID, homeTokenTTL, s.Config.SecretKey)}
		hs.Live = sum.EndedAt == nil && s.Hub != nil && s.Hub.HostConnected(sum.ID)
		sessions = append(sessions, hs)
	}
	_, remote := base()
	writeSecretJSON(w, http.StatusOK, map[string]any{"rooms": views, "recent": sessions, "guestLinksRemote": remote})
}
