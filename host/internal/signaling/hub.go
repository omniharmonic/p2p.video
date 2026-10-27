package signaling

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

// WebRTCHandler handles SDP and ICE negotiation with the SFU.
type WebRTCHandler interface {
	HandleOffer(sessionID, participantID string, sdp string) (answerSDP string, err error)
	HandleCandidate(sessionID, participantID string, candidate string, sdpMid *string, sdpMLineIndex *uint16) error
	HandleAnswer(sessionID, participantID, sdp string) error
	// HandleRenegotiation applies a client offer to the existing peer (screen
	// share add/remove). It fails without a reply on glare; see sfu.ErrGlare.
	HandleRenegotiation(sessionID, participantID, sdp string) (answerSDP string, err error)
	// SetScreenTracks declares (or, with no IDs, ends and unpublishes) a
	// participant's screen-share tracks.
	SetScreenTracks(sessionID, participantID string, trackIDs []string)
	SetOnOffer(fn func(sessionID, participantID, sdp string))
	OnParticipantLeave(sessionID, participantID string)
	SetOnCandidate(fn func(sessionID, participantID string, candidate string, sdpMid *string, sdpMLineIndex *uint16))
}

// Client represents a connected WebSocket client.
type Client struct {
	Hub           *Hub
	Done          chan struct{}
	closeOnce     sync.Once
	Conn          *websocket.Conn
	Send          chan []byte
	ParticipantID string
	SessionID     string
	Name          string
	Role          string
	// registeredSeq is the hub's Register order (dashboard ordering).
	registeredSeq uint64
	// ParentParticipantID is set for role "subsource" (a phone recording
	// for that participant) by the WebSocket handler, never by the client.
	ParentParticipantID string
	Status              string // "pending", "admitted", "denied"
	// JoinCredential identifies the guest credential this socket connected
	// with ("room:<nonce>" or "invite:<hash>"), so rotating or revoking it
	// can deny people still waiting under it. Empty for hosts. Set before
	// Register; never sent to clients or logged.
	JoinCredential string
	ClockOffsetUs  int64
	RTTUs          int64
	initialized    bool
	resumed        bool
	Tracks         map[string]RecorderStatusPayload
	// mediaState is the last call-path mic/camera state this connection sent;
	// mediaStateSynced records that it has received everyone else's.
	mediaState       *ParticipantStatePayload
	mediaStateSynced bool
	// preflight is the last sanitized pre-flight checklist [SL-P0-5].
	preflight *PreflightPayload
	// netStats is the last sanitized connection report [SL-P0-8].
	netStats *NetStatsPayload
	mu       sync.RWMutex
}

// Hub manages active sessions and connected clients.
type Hub struct {
	Store         *store.Store
	Config        *config.Config
	WebRTC        WebRTCHandler
	clients       map[string]*Client            // participantID -> client
	sessionGroups map[string]map[string]*Client // sessionID -> participantID -> client
	activeTake    map[string]*store.Take        // sessionID -> current Take
	rooms         map[string]*room              // sessionID -> chat/hands/lock (collab.go)
	now           func() time.Time              // test clock for rate limits; nil = time.Now
	mu            sync.RWMutex
	closeOnce     sync.Once
	stopCh        chan struct{}
	// preflightRefresh is when each session's host last requested fresh
	// pre-flight reports (Unix ms) [SL-P0-5].
	preflightRefresh map[string]int64

	// registered counts Register calls; each client keeps its number so the
	// dashboard lists people in arrival order, not map order [SL-QA-1].
	registered uint64

	// spentCaps holds used phone-capability nonces until they expire [SL-P1-9].
	spentCaps map[string]time.Time

	// Captions is the live-captions manager [SL-P4-2]; nil disables captions.
	Captions CaptionController

	// References records the host-received microphone of each participant
	// while a take runs (refrec, the ADR-007 alignment reference) [SL-P0-10];
	// nil disables it. Set before clients connect.
	References ReferenceRecorder

	// OnParticipantRevoked, when set, is called after a participant is denied,
	// removed or its session ends (the embedded TURN relay closes its live
	// allocations). Set before clients connect.
	OnParticipantRevoked func(participantID string)
}

// ReferenceRecorder captures alignment references for a session's take
// (implemented by refrec.Manager).
type ReferenceRecorder interface {
	StartTake(sessionID, refsDir string) error
	StopTake(sessionID string)
}

// NewHub creates a new signaling Hub.
func NewHub(st *store.Store, cfg *config.Config, rtc WebRTCHandler) *Hub {
	h := &Hub{
		Store:            st,
		Config:           cfg,
		WebRTC:           rtc,
		clients:          make(map[string]*Client),
		sessionGroups:    make(map[string]map[string]*Client),
		activeTake:       make(map[string]*store.Take),
		rooms:            make(map[string]*room),
		stopCh:           make(chan struct{}),
		preflightRefresh: make(map[string]int64),
	}
	if rtc != nil {
		rtc.SetOnOffer(func(sessionID, participantID, sdp string) {
			h.mu.RLock()
			c := h.clients[participantID]
			h.mu.RUnlock()
			if c != nil && c.SessionID == sessionID && c.getStatus() == "admitted" {
				raw, _ := json.Marshal(SdpPayload{SDP: sdp, Type: "offer"})
				c.SendEnvelope(&Envelope{Type: TypeSdpOffer, Payload: raw})
			}
		})
		rtc.SetOnCandidate(func(sessionID, participantID string, candidate string, sdpMid *string, sdpMLineIndex *uint16) {
			h.mu.RLock()
			client, ok := h.clients[participantID]
			h.mu.RUnlock()
			if ok && client != nil && client.SessionID == sessionID {
				raw, _ := json.Marshal(IceCandidatePayload{
					Candidate:     candidate,
					SDPMid:        sdpMid,
					SDPMLineIndex: sdpMLineIndex,
				})
				client.SendEnvelope(&Envelope{
					Type:          TypeIceCandidate,
					ParticipantID: participantID,
					Payload:       raw,
				})
			}
		})
	}
	go h.dashboardLoop()
	return h
}

// Close terminates the hub background routines.
func (h *Hub) Close() {
	h.closeOnce.Do(func() {
		close(h.stopCh)
		h.mu.RLock()
		defer h.mu.RUnlock()
		for _, c := range h.clients {
			c.close()
		}
	})
}

// Register adds a client to the hub.
func (h *Hub) Register(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if c.Done == nil {
		c.Done = make(chan struct{})
	}
	h.registered++
	c.registeredSeq = h.registered
	h.clients[c.ParticipantID] = c
	if _, ok := h.sessionGroups[c.SessionID]; !ok {
		h.sessionGroups[c.SessionID] = make(map[string]*Client)
	}
	h.sessionGroups[c.SessionID][c.ParticipantID] = c
	slog.Info("Signaling client registered", "participantId", c.ParticipantID, "sessionId", c.SessionID, "role", c.Role)
}

// Unregister cleans up a disconnected client.
func (h *Hub) Unregister(c *Client) {
	if h.unregister(c) {
		// The SFU already dropped the participant's publications.
		h.clearScreenShare(c.SessionID, c.ParticipantID, false)
		if c.getStatus() == "admitted" {
			h.broadcastRoster(c.SessionID)
		}
	}
}

// unregister removes c and reports whether it left the session group.
func (h *Hub) unregister(c *Client) bool {
	c.close()
	h.mu.Lock()
	defer h.mu.Unlock()

	if current, ok := h.clients[c.ParticipantID]; ok && current == c {
		delete(h.clients, c.ParticipantID)
	}
	if current := h.clients[c.ParticipantID]; current != nil && current != c {
		return false
	}
	if grp, ok := h.sessionGroups[c.SessionID]; ok {
		delete(grp, c.ParticipantID)
		if len(grp) == 0 {
			delete(h.sessionGroups, c.SessionID)
			delete(h.preflightRefresh, c.SessionID)
		}
	}
	if h.WebRTC != nil {
		h.WebRTC.OnParticipantLeave(c.SessionID, c.ParticipantID)
	}
	if c.getStatus() != "denied" {
		_ = h.Store.UpdateParticipantStatus(c.ParticipantID, "disconnected")
	}
	slog.Info("Signaling client unregistered", "participantId", c.ParticipantID)
	return true
}

// Send sends an envelope to a specific participant.
func (h *Hub) Send(participantID string, env *Envelope) {
	h.mu.RLock()
	c, ok := h.clients[participantID]
	h.mu.RUnlock()
	if ok {
		c.SendEnvelope(env)
	}
}

// Broadcast sends an envelope to all admitted clients in a session.
func (h *Hub) Broadcast(sessionID string, env *Envelope) {
	h.mu.RLock()
	grp, ok := h.sessionGroups[sessionID]
	if !ok {
		h.mu.RUnlock()
		return
	}
	clients := make([]*Client, 0, len(grp))
	for _, c := range grp {
		clients = append(clients, c)
	}
	h.mu.RUnlock()

	for _, c := range clients {
		if c.getStatus() == "admitted" {
			c.SendEnvelope(env)
		}
	}
}

// SendEnvelope encodes and enqueues a message for a client.
func (c *Client) SendEnvelope(env *Envelope) {
	select {
	case <-c.Done:
		return
	default:
	}
	data, err := json.Marshal(env)
	if err != nil {
		slog.Error("Failed to marshal envelope", "err", err)
		return
	}
	select {
	case c.Send <- data:
	default:
		slog.Warn("Client send queue full, dropping message", "participantId", c.ParticipantID)
	}
}

// SendToHost sends an envelope to the host participant of the session.
func (h *Hub) SendToHost(sessionID string, env *Envelope) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if grp, ok := h.sessionGroups[sessionID]; ok {
		for _, c := range grp {
			if c.Role == "host" {
				c.SendEnvelope(env)
			}
		}
	}
}

func (c *Client) getStatus() string       { c.mu.RLock(); defer c.mu.RUnlock(); return c.Status }
func (c *Client) setStatus(status string) { c.mu.Lock(); c.Status = status; c.mu.Unlock() }

func (c *Client) close() {
	c.closeOnce.Do(func() {
		if c.Done != nil {
			close(c.Done)
		}
		if c.Conn != nil {
			_ = c.Conn.Close()
		}
	})
}

// Counts reports live signaling objects for local diagnostics [SL-P0-6].
func (h *Hub) Counts() map[string]int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return map[string]int{"hubClients": len(h.clients), "hubSessions": len(h.sessionGroups), "hubActiveTakes": len(h.activeTake)}
}
