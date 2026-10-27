package signaling

import (
	"encoding/json"
	"log/slog"
	"time"
)

// sessionEndGrace lets session_end reach clients before their sockets close.
const sessionEndGrace = 1500 * time.Millisecond

// HostConnected reports whether an admitted host socket is in the session. A
// room's guest lobby waits for this before sending guests to admission.
func (h *Hub) HostConnected(sessionID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.sessionGroups[sessionID] {
		if c.Role == "host" && c.getStatus() == "admitted" {
			return true
		}
	}
	return false
}

// IsRecording reports whether the session has a take in progress.
func (h *Hub) IsRecording(sessionID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.activeTake[sessionID] != nil
}

// SetRoomLock sets the call's lock before anyone joins (a room's
// lock-on-start default). Admitted participants are never affected.
func (h *Hub) SetRoomLock(sessionID string, locked bool) {
	h.mu.Lock()
	h.roomLocked(sessionID).locked = locked
	h.mu.Unlock()
	h.broadcastRoster(sessionID)
	h.broadcastRoomState(sessionID)
}

// EndSession tells every connected client (admitted and waiting) that the
// meeting is over, drops their call media and closes their sockets after a
// short grace. The caller has already marked the session ended in the store,
// so reconnects are refused at the WebSocket upgrade.
func (h *Hub) EndSession(sessionID, reason string) {
	h.mu.Lock()
	clients := make([]*Client, 0, len(h.sessionGroups[sessionID]))
	for _, c := range h.sessionGroups[sessionID] {
		clients = append(clients, c)
	}
	delete(h.rooms, sessionID)
	h.mu.Unlock()
	if h.Captions != nil {
		h.Captions.SessionEnded(sessionID)
	}
	raw, _ := json.Marshal(SessionEndPayload{Reason: reason})
	for _, c := range clients {
		c.SendEnvelope(&Envelope{Type: TypeSessionEnd, Payload: raw})
		if h.WebRTC != nil {
			h.WebRTC.OnParticipantLeave(sessionID, c.ParticipantID)
		}
		h.revoked(c.ParticipantID)
		time.AfterFunc(sessionEndGrace, c.close)
	}
	slog.Info("Session ended", "sessionId", sessionID, "clients", len(clients), "reason", reason)
}
