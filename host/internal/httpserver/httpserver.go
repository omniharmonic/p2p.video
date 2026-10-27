package httpserver

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/omniharmonic/studiolink/host/internal/buildinfo"
	"github.com/omniharmonic/studiolink/host/internal/captions"
	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/filespeech"
	"github.com/omniharmonic/studiolink/host/internal/integrations"
	"github.com/omniharmonic/studiolink/host/internal/livestream"
	"github.com/omniharmonic/studiolink/host/internal/postproc"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
	"github.com/omniharmonic/studiolink/host/internal/tailscale"
	"github.com/omniharmonic/studiolink/host/internal/upload"
)

// Server wires together HTTP routes, WebSocket hub, upload handler, and static web UI.
type Server struct {
	Config   *config.Config
	Store    *store.Store
	Hub      *signaling.Hub
	Upload   *upload.Handler
	Pipeline *postproc.Pipeline
	WebFS    fs.FS
	// Livestream is optional; nil disables the livestream routes (503).
	Livestream *livestream.Manager
	// RuntimeCounts adds object counts (SFU peers, hub clients) to the
	// loopback-only /api/v1/debug/runtime snapshot [SL-P0-6]. Optional.
	RuntimeCounts func() map[string]int
	// DesktopToken authorizes the native host app's loopback-only state
	// endpoint. Empty disables the endpoint (503). Never log it.
	DesktopToken string
	// Captions relays call audio to the on-device caption provider
	// [SL-P4-2]. Nil disables the caption routes (503).
	Captions *captions.Manager
	// Transcription brokers post-take file transcription to the Mac app's
	// on-device recognizer [SL-P4-3]. Nil disables its provider socket (503).
	Transcription *filespeech.Broker
	// roomMu serializes opening room meetings so a room never gets two.
	roomMu sync.Mutex
	// recordMu serializes solo take start/stop/discard [SL-P5-3].
	recordMu sync.Mutex
	// Integrations runs post-take transcript/vault/clip-editor handoffs
	// (SL-P4-1). Optional; nil answers 503.
	Integrations *integrations.Service
	// LaunchToken is this run's dashboard launch token (<data-dir>/.launch_token,
	// see admin.go). Empty disables launch sign-in; the desktop token still
	// works. Never log it.
	LaunchToken string

	adminOnce  sync.Once
	adminValue string
	// joinLimits rate-limits guest WebSocket upgrades (ratelimit.go).
	joinLimitsOnce sync.Once
	joinByAddr     *limiter
	joinBySession  *limiter
	// tsStatus caches `tailscale status` for /api/v1/tailscale/status.
	tsStatus tsStatusCache
}

// checkOrigin rejects browser requests from unrelated origins, including local sites.
func (s *Server) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Path != "" {
		return false
	}
	if u.Host == r.Host {
		return true
	}
	if s.Config != nil && s.Config.PublicURL != "" {
		pub, err := url.Parse(s.Config.PublicURL)
		return err == nil && u.Scheme == pub.Scheme && u.Host == pub.Host
	}
	return false
}

func requestToken(r *http.Request) string {
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		return strings.TrimPrefix(a, "Bearer ")
	}
	return r.URL.Query().Get("token")
}
func (s *Server) requireHost(w http.ResponseWriter, r *http.Request, sessionID string) bool {
	if !signaling.VerifyHostToken(requestToken(r), sessionID, s.Config.SecretKey) {
		http.Error(w, "host authorization required", 401)
		return false
	}
	if !s.managementAllowed(r) {
		http.Error(w, "host management is available only on the local dashboard", 403)
		return false
	}
	return true
}

// sessionAccess is what a request's credential may see of one session.
type sessionAccess int

const (
	accessNone sessionAccess = iota
	// accessInvite: an invite or room-link holder, maybe never admitted:
	// only what the waiting room needs.
	accessInvite
	// accessParticipant: a signed participant grant of an admitted (or
	// briefly disconnected) participant.
	accessParticipant
	// accessHost: a host token, from a position where host control is
	// allowed (managementAllowed: never Funnel, never another tailnet device
	// unless it is the configured owner).
	accessHost
)

func (s *Server) sessionAccessFor(r *http.Request, sessionID string) sessionAccess {
	token := requestToken(r)
	if token == "" {
		return accessNone
	}
	if signaling.VerifyHostToken(token, sessionID, s.Config.SecretKey) {
		if s.managementAllowed(r) {
			return accessHost
		}
		return accessNone
	}
	sess, err := s.Store.GetSession(sessionID)
	if err != nil || sess.EndedAt != nil {
		return accessNone
	}
	if claims, err := signaling.VerifyUploadToken(token, s.Config.SecretKey); err == nil {
		if claims.SessionID != sessionID {
			return accessNone
		}
		part, err := s.Store.GetParticipant(claims.ParticipantID)
		if err != nil || part.SessionID != sessionID || (part.Status != "admitted" && part.Status != "disconnected") {
			return accessNone
		}
		return accessParticipant
	}
	if s.guestTokenValid(sess, token) {
		return accessInvite
	}
	// A producer link sees what a guest invite sees before admission [SL-P1-11].
	if _, ok := s.producerCredential(sess, token); ok {
		return accessInvite
	}
	return accessNone
}

func (s *Server) requireSession(w http.ResponseWriter, r *http.Request, sessionID string) bool {
	if s.sessionAccessFor(r, sessionID) == accessNone {
		http.Error(w, "session authorization required", 401)
		return false
	}
	return true
}

// Host credential bootstrap is available only directly on loopback, never through Serve/Funnel.
func localManagement(r *http.Request) bool {
	host := r.URL.Hostname()
	if host == "" {
		u, _ := url.Parse("http://" + r.Host)
		host = u.Hostname()
	}
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(remote)
	return err == nil && ip != nil && ip.IsLoopback() && (host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())) && r.Header.Get("X-Forwarded-For") == "" && r.Header.Get("Forwarded") == "" && r.Header.Get("Tailscale-Funnel-Request") == ""
}

// Private Serve overwrites identity headers. Trust them only on a loopback
// proxy request to the explicitly configured HTTPS origin, with a tailnet client
// address. Default configuration remains local-only; Funnel is always rejected.
func (s *Server) managementAllowed(r *http.Request) bool {
	return localManagement(r) || s.ownerServeAllowed(r)
}

// ownerServeAllowed is the opt-in owner-over-private-Serve path
// (--owner-tailscale-login). It trusts Serve's identity headers on a loopback
// proxy request; a local process could forge them, which docs/SECURITY.md
// lists as a residual risk of enabling it.
func (s *Server) ownerServeAllowed(r *http.Request) bool {
	if s.Config == nil || s.Config.OwnerTailscaleLogin == "" || r.Header.Get("Tailscale-Funnel-Request") != "" {
		return false
	}
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(remote)
	if err != nil || ip == nil || !ip.IsLoopback() {
		return false
	}
	u, err := url.Parse(s.Config.PublicURL)
	if err != nil || u.Scheme != "https" || !strings.HasSuffix(u.Hostname(), ".ts.net") || u.Host != r.Host {
		return false
	}
	if r.Header.Get("Tailscale-User-Login") != s.Config.OwnerTailscaleLogin {
		return false
	}
	client := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Forwarded-For")))
	if client == nil {
		return false
	}
	_, v4, _ := net.ParseCIDR("100.64.0.0/10")
	_, v6, _ := net.ParseCIDR("fd7a:115c:a1e0::/48")
	return v4.Contains(client) || v6.Contains(client)
}

// Handler returns the configured http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/api/v1/doctor", s.handleDoctor)
	mux.HandleFunc("/api/v1/desktop/state", s.handleDesktopState)
	mux.HandleFunc("/api/v1/captions/provider", s.handleCaptionProvider)
	mux.HandleFunc("/api/v1/transcription/provider", s.handleTranscriptionProvider)
	mux.HandleFunc("/api/v1/tailscale/status", s.handleTailscaleStatus)
	mux.HandleFunc("/api/v1/sessions", s.handleSessions)
	mux.HandleFunc("/api/v1/sessions/", s.handleSessionDetail)
	mux.HandleFunc("/api/v1/rooms", s.handleRooms)
	mux.HandleFunc("/api/v1/rooms/", s.handleRooms)
	mux.HandleFunc("/api/v1/home", s.handleHome)
	// Async record links [SL-P5-3]: host management, and the guest side.
	mux.HandleFunc("/api/v1/recordlinks", s.handleRecordLinks)
	mux.HandleFunc("/api/v1/recordlinks/", s.handleRecordLinks)
	mux.HandleFunc("/api/v1/record/", s.handleRecordGuest)
	mux.HandleFunc("/record", s.serveRecordPage)
	// Calendar integration [SL-P6-1]: scheduling and invites (management),
	// the host's feed (this Mac and tailnet only, never Funnel) and the Mac
	// app's matching data (loopback + desktop token).
	mux.HandleFunc("/api/v1/schedule", s.handleSchedule)
	mux.HandleFunc("/api/v1/schedule/", s.handleSchedule)
	mux.HandleFunc("/api/v1/calendar/", s.handleCalendar)
	mux.HandleFunc("/api/v1/desktop/calendar", s.handleDesktopCalendar)
	mux.HandleFunc("/api/v1/ws", s.handleWebSocket)
	mux.HandleFunc("/api/v1/takes/", s.handleTakes)
	mux.HandleFunc("/api/v1/debug/runtime", s.handleDebugRuntime)

	// Static UI and SPA fallback
	if s.WebFS != nil {
		fileServer := http.FileServer(http.FS(s.WebFS))
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.NotFound(w, r)
				return
			}
			f, err := s.WebFS.Open(strings.TrimPrefix(r.URL.Path, "/"))
			if err == nil {
				_ = f.Close()
				fileServer.ServeHTTP(w, r)
				return
			}
			// Fallback to index.html for SPA routes
			r.URL.Path = "/"
			fileServer.ServeHTTP(w, r)
		})
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/" {
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><title>p2p.video</title></head><body><h1>p2p.video Daemon Active</h1><p>Running on Go / Pion SFU</p></body></html>`))
				return
			}
			http.NotFound(w, r)
		})
	}

	return s.corsMiddleware(mux)
}

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w.Header())
		origin := r.Header.Get("Origin")
		if origin != "" && !s.checkOrigin(r) {
			http.Error(w, "origin not allowed", 403)
			return
		}
		// Bound slow request bodies (slowloris over Funnel). Chunk uploads
		// extend this per request; WebSockets set their own deadlines.
		if !isWebSocketUpgrade(r) {
			_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(requestReadTimeout))
		}
		if s.handleLaunch(w, r) {
			return
		}
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Chunk-SHA256")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "product": buildinfo.Product, "buildId": buildinfo.ID})
}

type CreateSessionRequest struct {
	Title     string `json:"title"`
	AutoAdmit bool   `json:"autoAdmit"`
}

type CreateSessionResponse struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	InviteURL string `json:"inviteUrl"`
	Token     string `json:"token"`
	HostToken string `json:"hostToken"`
	HostURL   string `json:"hostUrl"`
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(r) {
		adminDenied(w, r, "Creating a meeting")
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req CreateSessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	if req.Title == "" {
		req.Title = fmt.Sprintf("Session %s", time.Now().Format("Jan 02 15:04"))
	}

	sessID := uuid.New().String()[:8]

	// Generate 32-byte secure invite token
	tokenBytes := make([]byte, 32)
	_, _ = rand.Read(tokenBytes)
	tokenHex := hex.EncodeToString(tokenBytes)

	// Hash token for database storage
	h := sha256.Sum256([]byte(tokenHex))
	tokenHash := hex.EncodeToString(h[:])

	sess := &store.Session{
		ID:              sessID,
		Title:           req.Title,
		CreatedAt:       time.Now().UTC(),
		PresetJSON:      `{"preset":"1080p30"}`,
		InviteTokenHash: tokenHash,
		AutoAdmit:       req.AutoAdmit,
	}

	if err := s.Store.CreateSession(sess); err != nil {
		http.Error(w, fmt.Sprintf("create session: %v", err), http.StatusInternalServerError)
		return
	}

	hostToken := signaling.MintHostToken(sessID, 24*time.Hour, s.Config.SecretKey)
	inviteURL := fmt.Sprintf("%s/join?session=%s&token=%s", s.Config.PublicURL, sessID, tokenHex)
	hostURL := fmt.Sprintf("http://%s/?session=%s&token=%s", r.Host, sessID, hostToken)

	resp := CreateSessionResponse{
		ID:        sessID,
		Title:     sess.Title,
		InviteURL: inviteURL,
		Token:     tokenHex,
		HostToken: hostToken,
		HostURL:   hostURL,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleSessionDetail(w http.ResponseWriter, r *http.Request) {
	relPath := strings.TrimPrefix(r.URL.Path, "/api/v1/sessions/")
	relPath = strings.TrimPrefix(relPath, "/")

	// Handle /api/v1/sessions/{id}/livestream[/ingest] (host management only)
	if id, sub, ok := strings.Cut(relPath, "/livestream"); ok && id != "" && !strings.Contains(id, "/") && (sub == "" || sub == "/" || sub == "/ingest") {
		s.handleLivestream(w, r, id, strings.Trim(sub, "/"))
		return
	}

	// Handle /api/v1/sessions/{id}/captions/{ingest,transcript} (host management only) [SL-P4-2]
	if id, sub, ok := strings.Cut(relPath, "/captions/"); ok && id != "" && !strings.Contains(id, "/") && !strings.Contains(sub, "/") {
		s.handleCaptionSession(w, r, id, sub)
		return
	}

	// Handle /api/v1/sessions/{id}/subsource (an admitted participant) [SL-P1-9]
	if id, ok := strings.CutSuffix(strings.TrimSuffix(relPath, "/"), "/subsource"); ok && id != "" && !strings.Contains(id, "/") {
		s.handleSubsourceLink(w, r, id)
		return
	}

	// Handle /api/v1/sessions/{id}/producer[/rotate|/revoke] (host only) [SL-P1-11]
	if id, sub, ok := strings.Cut(strings.TrimSuffix(relPath, "/"), "/producer"); ok && id != "" && !strings.Contains(id, "/") && (sub == "" || sub == "/rotate" || sub == "/revoke") {
		s.handleSessionProducer(w, r, id, strings.TrimPrefix(sub, "/"))
		return
	}

	// Handle /api/v1/sessions/{id}/branding[/logo] (host only) [SL-P5-2]
	if id, sub, ok := strings.Cut(strings.TrimSuffix(relPath, "/"), "/branding"); ok && id != "" && !strings.Contains(id, "/") && (sub == "" || sub == "/logo") {
		s.handleSessionBranding(w, r, id, strings.TrimPrefix(sub, "/"))
		return
	}

	// Handle /api/v1/sessions/{id}/end (host only) [SL-P1-5]
	if id, ok := strings.CutSuffix(strings.TrimSuffix(relPath, "/"), "/end"); ok && id != "" && !strings.Contains(id, "/") {
		s.handleSessionEnd(w, r, id)
		return
	}

	// Handle /api/v1/sessions/{id}/invite
	if strings.HasSuffix(relPath, "/invite") {
		sessID := strings.TrimSuffix(relPath, "/invite")
		sessID = strings.TrimSuffix(sessID, "/")
		s.handleSessionInvite(w, r, sessID)
		return
	}

	// Handle /api/v1/sessions/{id}/settings
	if strings.HasSuffix(relPath, "/settings") {
		sessID := strings.TrimSuffix(relPath, "/settings")
		sessID = strings.TrimSuffix(sessID, "/")
		s.handleSessionSettings(w, r, sessID)
		return
	}

	sessID := relPath
	if idx := strings.Index(sessID, "/"); idx != -1 {
		sessID = sessID[:idx]
	}

	access := s.sessionAccessFor(r, sessID)
	if access == accessNone {
		http.Error(w, "session authorization required", 401)
		return
	}
	sess, err := s.Store.GetSession(sessID)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	currentSettings := store.DefaultSessionSettings()
	if sess.PresetJSON != "" && sess.PresetJSON != "{}" {
		_ = json.Unmarshal([]byte(sess.PresetJSON), &currentSettings)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	// Each credential sees only what its client needs [SL-SEC-1]: the waiting
	// room needs the title and recording settings; an admitted participant
	// also needs everyone's display names; only the host sees takes and the
	// full roster. Session.InviteTokenHash is never serialized.
	view := map[string]any{
		"session":  map[string]any{"id": sess.ID, "title": sess.Title},
		"settings": currentSettings,
	}
	// The green room says "you're joining as producer" before admission [SL-P1-11].
	if access == accessInvite {
		if _, producer := s.producerCredential(sess, requestToken(r)); producer {
			view["joinRole"] = signaling.RoleProducer
		}
	}
	switch access {
	case accessHost:
		participants, _ := s.Store.ListParticipants(sessID)
		takes, _ := s.Store.ListTakesBySession(sessID)
		if takes == nil {
			takes = []store.Take{}
		}
		view["session"], view["participants"], view["takes"] = sess, participants, takes
	case accessParticipant:
		participants, _ := s.Store.ListParticipants(sessID)
		roster := make([]map[string]string, 0, len(participants))
		for _, p := range participants {
			if p.Status == "admitted" || p.Status == "disconnected" {
				roster = append(roster, map[string]string{"id": p.ID, "name": p.Name, "role": p.Role})
			}
		}
		view["participants"] = roster
	}
	_ = json.NewEncoder(w).Encode(view)
}

func (s *Server) handleSessionSettings(w http.ResponseWriter, r *http.Request, sessID string) {
	if r.Method == http.MethodGet {
		if !s.requireSession(w, r, sessID) {
			return
		}
	} else if !s.requireHost(w, r, sessID) {
		return
	}
	sess, err := s.Store.GetSession(sessID)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	currentSettings := store.DefaultSessionSettings()
	if sess.PresetJSON != "" && sess.PresetJSON != "{}" {
		_ = json.Unmarshal([]byte(sess.PresetJSON), &currentSettings)
	}

	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(currentSettings)
		return
	}

	if r.Method == http.MethodPatch || r.Method == http.MethodPut || r.Method == http.MethodPost {
		var incoming map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
			http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
			return
		}

		if v, ok := incoming["video_bitrate_bps"].(float64); ok && v > 0 {
			currentSettings.VideoBitrateBps = int(v)
		}
		if v, ok := incoming["video_codec"].(string); ok && v != "" {
			currentSettings.VideoCodec = v
		}
		if v, ok := incoming["target_fps"].(float64); ok && v > 0 {
			currentSettings.TargetFPS = v
		}
		if v, ok := incoming["audio_sample_rate"].(float64); ok && v > 0 {
			currentSettings.AudioSampleRate = int(v)
		}
		if v, ok := incoming["audio_bit_depth"].(float64); ok && v > 0 {
			currentSettings.AudioBitDepth = int(v)
		}
		if v, ok := incoming["xcorr_threshold"].(float64); ok && v > 0 {
			currentSettings.XcorrThreshold = v
		}
		if v, ok := incoming["xcorr_window_ms"].(float64); ok && v > 0 {
			currentSettings.XcorrWindowMs = int(v)
		}
		if v, ok := incoming["engine"].(string); ok && v != "" {
			currentSettings.Engine = v
		}
		if v, ok := incoming["auto_transcribe"].(bool); ok {
			currentSettings.AutoTranscribe = v
		}
		if v, ok := incoming["export_codec"].(string); ok {
			if v != "h264" && v != "prores_proxy" {
				http.Error(w, "export_codec must be h264 or prores_proxy", http.StatusBadRequest)
				return
			}
			currentSettings.ExportCodec = v
		}
		micChanged := false
		if v, ok := incoming["recording_mic_mode"].(string); ok {
			if !signaling.ValidRecordingMicMode(v) {
				http.Error(w, "recording_mic_mode must be auto, raw or clean", http.StatusBadRequest)
				return
			}
			micChanged = currentSettings.RecordingMicMode != v
			currentSettings.RecordingMicMode = v
		}

		updatedBytes, err := json.Marshal(currentSettings)
		if err != nil {
			http.Error(w, "failed to marshal settings", http.StatusInternalServerError)
			return
		}

		if err := s.Store.UpdateSessionSettings(sessID, string(updatedBytes)); err != nil {
			http.Error(w, fmt.Sprintf("failed to persist settings: %v", err), http.StatusInternalServerError)
			return
		}
		// Participants re-resolve their recording mic (applied between takes) [SL-P1-10].
		if micChanged && s.Hub != nil {
			s.Hub.SessionSettingsChanged(sessID)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(updatedBytes)
		return
	}

	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func (s *Server) handleSessionInvite(w http.ResponseWriter, r *http.Request, sessID string) {
	if !s.requireHost(w, r, sessID) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	token := r.URL.Query().Get("token")
	if token == "" {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			token = strings.TrimPrefix(auth, "Bearer ")
		}
	}

	if !signaling.VerifyHostToken(token, sessID, s.Config.SecretKey) {
		http.Error(w, "unauthorized: host token required", http.StatusUnauthorized)
		return
	}

	sess, err := s.Store.GetSession(sessID)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	baseURL := s.Config.PublicURL
	if discovered, e := tailscale.NewClient().DiscoverGuestURL(s.Config.HTTPPort); e == nil && (baseURL == "" || strings.HasPrefix(baseURL, "http://localhost:") || strings.HasPrefix(baseURL, "http://127.0.0.1:")) {
		baseURL = discovered
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Hostname() == "localhost" || (net.ParseIP(u.Hostname()) != nil && net.ParseIP(u.Hostname()).IsLoopback()) || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		http.Error(w, "Configure --public-url with your private Tailscale HTTPS address and Tailscale Serve before inviting a remote guest.", 422)
		return
	}
	tokenBytes := make([]byte, 32)
	_, _ = rand.Read(tokenBytes)
	tokenHex := hex.EncodeToString(tokenBytes)

	h := sha256.Sum256([]byte(tokenHex))
	tokenHash := hex.EncodeToString(h[:])

	if err := s.Store.UpdateSessionInviteTokenHash(sess.ID, tokenHash); err != nil {
		http.Error(w, fmt.Sprintf("update invite token: %v", err), http.StatusInternalServerError)
		return
	}
	// The previous invite stops working: so does waiting under it (L4).
	if s.Hub != nil {
		s.Hub.DenyPendingWithCredential(sess.ID, inviteCredential(sess.InviteTokenHash))
	}

	inviteURL := fmt.Sprintf("%s/join?session=%s&token=%s", strings.TrimRight(baseURL, "/"), sess.ID, tokenHex)
	resp := map[string]string{
		"sessionId": sess.ID,
		"inviteUrl": inviteURL,
		"token":     tokenHex,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(r) {
		adminDenied(w, r, "The setup check")
		return
	}
	tsClient := tailscale.NewClient()
	tsStatus, tsErr := tsClient.CheckStatus()

	hasFFmpeg := s.Pipeline != nil && s.Pipeline.FFmpeg != nil
	ffmpegPath := ""
	hasVideoToolbox := false
	if hasFFmpeg {
		ffmpegPath = s.Pipeline.FFmpeg.FFmpegPath
		hasVideoToolbox = s.Pipeline.FFmpeg.HasVideoToolbox
	}

	tsOnline := false
	dnsName := ""
	var ips []string
	tsErrStr := ""
	if tsStatus != nil {
		tsOnline = tsStatus.Self.Online
		dnsName = tsStatus.Self.DNSName
		ips = tsStatus.Self.TailscaleIPs
	}
	if tsErr != nil {
		tsErrStr = tsErr.Error()
	}

	guestURL, serveErr := tsClient.DiscoverGuestURL(s.Config.HTTPPort)
	resp := map[string]interface{}{
		"connection": map[string]interface{}{"guestUrl": guestURL, "privateServeReady": serveErr == nil, "port": s.Config.HTTPPort},
		"status":     "ok",
		"ffmpeg": map[string]interface{}{
			"installed":    hasFFmpeg,
			"path":         ffmpegPath,
			"videotoolbox": hasVideoToolbox,
		},
		"tailscale": map[string]interface{}{
			"online":   tsOnline,
			"dns_name": dnsName,
			"ips":      ips,
			"error":    tsErrStr,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleTailscaleStatus reports this machine's tailnet identity: host-only
// (it names the tailnet and its IPs) and cached, because each miss forks the
// tailscale CLI [SL-SEC-1].
func (s *Server) handleTailscaleStatus(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(r) {
		adminDenied(w, r, "Tailscale status")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(s.tsStatus.get(time.Now(), func() []byte {
		status, err := tailscale.NewClient().CheckStatus()
		var v map[string]any
		if err != nil {
			v = map[string]any{"online": false, "error": err.Error()}
		} else {
			v = map[string]any{"online": status.Self.Online, "dns_name": status.Self.DNSName, "ips": status.Self.TailscaleIPs}
		}
		raw, _ := json.Marshal(v)
		return append(raw, '\n')
	}))
}

// tsStatusTTL is how long a tailscale status answer is reused.
const tsStatusTTL = 5 * time.Second

// tsStatusCache memoizes one rendered status; concurrent misses share a fetch.
type tsStatusCache struct {
	mu      sync.Mutex
	at      time.Time
	body    []byte
	fetches int // tests
}

func (c *tsStatusCache) get(now time.Time, fetch func() []byte) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.body == nil || now.Sub(c.at) >= tsStatusTTL || now.Before(c.at) {
		c.body, c.at = fetch(), now
		c.fetches++
	}
	return c.body
}

func (s *Server) handleTakes(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && !strings.Contains(strings.TrimPrefix(r.URL.Path, "/api/v1/takes/"), "/") {
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/takes/")
		tk, err := s.Store.GetTake(id)
		if err != nil {
			http.Error(w, "take not found", 404)
			return
		}
		if !s.requireHost(w, r, tk.SessionID) {
			return
		}
		tracks, err := s.Store.ListTracksByTake(id)
		if err != nil {
			http.Error(w, "cannot list tracks", 500)
			return
		}
		rows := make([]map[string]interface{}, 0, len(tracks))
		for _, tr := range tracks {
			name, label := "Unknown", tr.SourceID
			if src, e := s.Store.GetSource(tr.SourceID); e == nil {
				label = src.Label
				if part, e := s.Store.GetParticipant(src.ParticipantID); e == nil {
					name = part.Name
					// A phone's tracks are listed under its participant [SL-P1-9].
					if part.Role == signaling.RoleSubsource && part.ParentParticipantID != "" {
						if parent, e := s.Store.GetParticipant(part.ParentParticipantID); e == nil {
							name, label = parent.Name, "Phone "+label
						}
					}
				}
			}
			rows = append(rows, map[string]interface{}{"trackId": tr.ID, "kind": tr.Kind, "status": tr.Status, "totalBytes": tr.TotalBytes, "lastSeq": tr.LastSeq, "sha256": tr.SHA256, "participantName": name, "sourceLabel": label})
		}
		missing, err := s.Store.ListMissingTakeParticipants(id)
		if err != nil {
			http.Error(w, "cannot verify take completeness", 500)
			return
		}
		// Originals are downloadable once every registered track is assembled
		// (quick delivery, SL-P0-9); the export status is memory-only.
		originalsReady := tk.Status != "recording" && len(missing) == 0 && len(tracks) > 0
		for _, tr := range tracks {
			if tr.Status != "assembled" && tr.Status != "processed" {
				originalsReady = false
			}
		}
		body := map[string]interface{}{"take": tk, "tracks": rows, "missingParticipants": missing, "originalsReady": originalsReady}
		if s.Pipeline != nil {
			if status, ok := s.Pipeline.ExportStatus(id); ok {
				body["postproc"] = status
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
		return
	}

	if s.handleTakeIntegrations(w, r) {
		return
	}

	if strings.HasSuffix(r.URL.Path, "/download") {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/takes/"), "/download")
		s.handleTakeDownload(w, r, id)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/originals") {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/takes/"), "/originals")
		s.handleOriginalsDownload(w, r, id)
		return
	}

	// If path matches chunk upload or complete: /api/v1/takes/{takeId}/tracks/{trackId}/...
	if strings.Contains(r.URL.Path, "/tracks/") || strings.HasSuffix(r.URL.Path, "/tracks") {
		s.Upload.ServeHTTP(w, r)
		return
	}

	// Trigger post-processing: POST /api/v1/takes/{takeId}/postprocess
	if strings.HasSuffix(r.URL.Path, "/postprocess") && r.Method == http.MethodPost {
		takeID := strings.TrimPrefix(r.URL.Path, "/api/v1/takes/")
		takeID = strings.TrimSuffix(takeID, "/postprocess")

		tk, err := s.Store.GetTake(takeID)
		if err != nil {
			http.Error(w, "take not found", 404)
			return
		}
		if !s.requireHost(w, r, tk.SessionID) {
			return
		}
		if s.Pipeline == nil {
			http.Error(w, "postprocessing unavailable", 503)
			return
		}
		if err := s.Pipeline.ProcessTake(takeID); err != nil {
			http.Error(w, fmt.Sprintf("post-processing failed: %v", err), http.StatusInternalServerError)
			return
		}
		if s.Integrations != nil {
			s.Integrations.TakeExported(takeID)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"complete"}`))
		return
	}

	http.NotFound(w, r)
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session")
	token := r.URL.Query().Get("token")
	// Never accept a caller-selected identity: it could hijack an admitted participant.
	participantID := "p_" + uuid.NewString()

	if sessionID == "" || token == "" {
		http.Error(w, "missing session or token query parameter", http.StatusUnauthorized)
		return
	}

	sess, err := s.Store.GetSession(sessionID)
	if err != nil || sess.EndedAt != nil {
		http.Error(w, "session unavailable", 401)
		return
	}
	// A record-link submission is a recording, never a call [SL-P5-3]: no
	// credential (host token included) opens a socket into it.
	if s.Store.IsSubmissionSession(sessionID) {
		http.Error(w, "this recording has no call to join", http.StatusForbidden)
		return
	}
	role := "guest"
	credential := ""
	parentID := ""

	// Check 1: Is token a valid Host token?
	if signaling.VerifyHostToken(token, sessionID, s.Config.SecretKey) {
		// Host control (admit/deny/record) is management: only from the local
		// dashboard or the configured owner over private Serve, the same rule
		// as requireHost. Never Funnel, never another tailnet device (L3).
		if !s.managementAllowed(r) {
			if r.Header.Get("Tailscale-Funnel-Request") != "" {
				http.Error(w, "host control is not available through Funnel", http.StatusForbidden)
				return
			}
			http.Error(w, "host control is available only on the local dashboard (or to the owner configured with --owner-tailscale-login)", http.StatusForbidden)
			return
		}
		role = "host"
	} else if parent, ok := s.Hub.SubsourceResume(sessionID, token); ok {
		// Check 2: a phone reconnecting with its own participant grant [SL-P1-9].
		role, parentID = signaling.RoleSubsource, parent
	} else if strings.HasPrefix(r.URL.Query().Get("role"), signaling.RoleSubsource) {
		// Check 3: a phone's single-use capability, spent here [SL-P1-9].
		if !s.allowJoin(r, sessionID) {
			w.Header().Set("Retry-After", "10")
			http.Error(w, "too many join attempts; wait a moment and scan the code again", http.StatusTooManyRequests)
			return
		}
		parent, err := s.Hub.ConsumeSubsourceCap(sessionID, token)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		role, parentID = signaling.RoleSubsource, parent
	} else {
		// Check 4: the session's guest invite, or its room's guest link; or
		// the producer link, which only changes the role [SL-P1-11].
		cred, ok := s.guestCredential(sess, token)
		if !ok {
			if cred, ok = s.producerCredential(sess, token); ok {
				role = signaling.RoleProducer
			}
		}
		if !ok {
			http.Error(w, "unauthorized: invalid or expired session token", http.StatusUnauthorized)
			return
		}
		// Join attempts are rate-limited per client address and per session
		// before the upgrade, so a leaked link cannot flood the host [SL-SEC-1].
		if !s.allowJoin(r, sessionID) {
			w.Header().Set("Retry-After", "10")
			http.Error(w, "too many join attempts; wait a moment and reopen the invitation", http.StatusTooManyRequests)
			return
		}
		credential = cred
	}

	upgrader := websocket.Upgrader{
		CheckOrigin: s.checkOrigin,
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("WebSocket upgrade failed", "err", err)
		return
	}

	initialStatus := "pending"
	if role == "host" {
		initialStatus = "admitted"
	}

	conn.SetReadLimit(1 << 20)
	// Detect dead network paths even when TCP does not deliver a close frame.
	_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(45 * time.Second)) })
	client := &signaling.Client{
		Hub:            s.Hub,
		Conn:           conn,
		Send:           make(chan []byte, 256),
		ParticipantID:  participantID,
		SessionID:      sessionID,
		Role:           role,
		Status:         initialStatus,
		JoinCredential: credential,
		// Only the server sets a phone's parent (from its capability or grant).
		ParentParticipantID: parentID,
	}

	s.Hub.Register(client)
	// A socket that never says hello does not stay registered (M1).
	time.AfterFunc(signaling.HelloTimeout, func() {
		if !client.HelloReceived() {
			slog.Info("Closing socket that sent no hello", "sessionId", sessionID)
			_ = conn.Close()
		}
	})

	// Pump writes
	go func() {
		defer conn.Close()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
					return
				}
			case <-client.Done:
				return
			case msg := <-client.Send:
				if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
					return
				}
			}
		}
	}()

	// Pump reads
	go func() {
		defer func() {
			s.Hub.Unregister(client)
			_ = conn.Close()
		}()
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				break
			}
			s.Hub.HandleMessage(client, msg)
		}
	}()
}
