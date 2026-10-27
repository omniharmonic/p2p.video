package signaling

import (
	"encoding/json"
	"log/slog"
)

// CaptionController is the live-captions manager (internal/captions) as the
// hub sees it [SL-P4-2]. Nil disables captions: control requests are refused
// and everyone is told captions are off.
type CaptionController interface {
	// Control applies the host's on/off and language request. It returns the
	// resulting state, or an error for an invalid request.
	Control(sessionID string, p CaptionControlPayload) (CaptionStatePayload, error)
	// State returns a session's current caption state.
	State(sessionID string) CaptionStatePayload
	// SessionEnded stops recognition for a session; its transcript is kept.
	SessionEnded(sessionID string)
}

// handleCaptionControl lets the host turn captions on or off for the room.
func (h *Hub) handleCaptionControl(c *Client, raw json.RawMessage) {
	if !h.requireHost(c, TypeCaptionControl) {
		return
	}
	var p CaptionControlPayload
	if json.Unmarshal(raw, &p) != nil {
		h.sendActionError(c, TypeCaptionControl, ErrCodeInvalid, "Invalid captions request.")
		return
	}
	if h.Captions == nil {
		h.sendActionError(c, TypeCaptionControl, ErrCodeInvalid, "Live captions are not available on this host.")
		return
	}
	if _, err := h.Captions.Control(c.SessionID, p); err != nil {
		h.sendActionError(c, TypeCaptionControl, ErrCodeInvalid, err.Error())
		return
	}
	slog.Info("Captions changed by host", "sessionId", c.SessionID, "enabled", p.Enabled)
	// The manager broadcasts the new state through BroadcastCaptionState.
}

func (h *Hub) captionState(sessionID string) CaptionStatePayload {
	if h.Captions == nil {
		return CaptionStatePayload{Language: CaptionDefaultLang}
	}
	return h.Captions.State(sessionID)
}

// sendCaptionState tells one (admitted or waiting) client whether captions are on.
func (h *Hub) sendCaptionState(c *Client) {
	raw, _ := json.Marshal(h.captionState(c.SessionID))
	c.SendEnvelope(&Envelope{Type: TypeCaptionState, Payload: raw})
}

// BroadcastCaptionState reaches admitted AND waiting participants, like the
// recording state: a guest learns captions are on before being admitted.
func (h *Hub) BroadcastCaptionState(sessionID string, p CaptionStatePayload) {
	raw, _ := json.Marshal(p)
	env := &Envelope{Type: TypeCaptionState, Payload: raw}
	for _, c := range h.sessionClients(sessionID) {
		if s := c.getStatus(); s == "admitted" || s == "pending" {
			c.SendEnvelope(env)
		}
	}
}

// BroadcastCaption sends a caption segment to admitted participants only.
func (h *Hub) BroadcastCaption(sessionID string, p CaptionPayload) {
	raw, _ := json.Marshal(p)
	h.Broadcast(sessionID, &Envelope{Type: TypeCaption, Payload: raw})
}

// AdmittedParticipant returns the display name of an admitted participant of
// the session. Captions are attributed only to admitted participants.
func (h *Hub) AdmittedParticipant(sessionID, participantID string) (string, bool) {
	h.mu.RLock()
	c := h.sessionGroups[sessionID][participantID]
	h.mu.RUnlock()
	// A producer is never captioned or transcribed [SL-P1-11].
	if c == nil || c.getStatus() != "admitted" || c.Role == RoleProducer {
		return "", false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Name, true
}

// CaptionSessions lists sessions with a connected socket, so the manager can
// refresh their caption state when a recognizer connects or leaves.
func (h *Hub) CaptionSessions() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(h.sessionGroups))
	for sid := range h.sessionGroups {
		out = append(out, sid)
	}
	return out
}

func (h *Hub) sessionClients(sessionID string) []*Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	clients := make([]*Client, 0, len(h.sessionGroups[sessionID]))
	for _, c := range h.sessionGroups[sessionID] {
		clients = append(clients, c)
	}
	return clients
}
