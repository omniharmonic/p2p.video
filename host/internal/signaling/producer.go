package signaling

import (
	"encoding/json"
	"log/slog"

	"github.com/omniharmonic/studiolink/host/internal/store"
)

// Producer role and session-wide recording mic default [SL-P1-11, SL-P1-10].
// The permission rules live here so the matrix is in one place; the protocol
// block in protocol.go documents them.

// isAssistant reports whether c may use the host-assist actions: the host,
// or an admitted producer.
func isAssistant(c *Client) bool {
	return (c.Role == RoleHost || c.Role == RoleProducer) && c.getStatus() == "admitted"
}

// requireAssist enforces host-or-producer actions server-side.
func (h *Hub) requireAssist(c *Client, action string) bool {
	if isAssistant(c) {
		return true
	}
	slog.Warn("Rejected host-assist action", "action", action, "participantId", c.ParticipantID, "role", c.Role)
	h.sendActionError(c, action, ErrCodeForbidden, "Only the host or a producer can do that.")
	return false
}

// mayAdmitOrDeny: the host decides on anyone; a producer only on guests who
// are waiting (never on another producer, never someone already admitted).
func (h *Hub) mayAdmitOrDeny(c *Client, action, targetID string) bool {
	if c.Role == RoleHost && c.getStatus() == "admitted" {
		return true
	}
	if !h.requireAssist(c, action) {
		return false
	}
	h.mu.RLock()
	target := h.sessionGroups[c.SessionID][targetID]
	h.mu.RUnlock()
	if target == nil || target.Role != RoleGuest || target.getStatus() != "pending" {
		slog.Warn("Rejected producer admission decision", "action", action, "participantId", c.ParticipantID)
		h.sendActionError(c, action, ErrCodeForbidden, "A producer can admit or decline waiting guests only.")
		return false
	}
	return true
}

// sendToAssistants sends env to the session's host and admitted producers.
func (h *Hub) sendToAssistants(sessionID string, env *Envelope) {
	h.mu.RLock()
	clients := make([]*Client, 0, len(h.sessionGroups[sessionID]))
	for _, c := range h.sessionGroups[sessionID] {
		clients = append(clients, c)
	}
	h.mu.RUnlock()
	for _, c := range clients {
		if c.Role == RoleHost || isAssistant(c) {
			c.SendEnvelope(env)
		}
	}
}

// recordingMicDefault is the session's recording mic default (auto when
// unset or unreadable).
func (h *Hub) recordingMicDefault(sessionID string) string {
	if h.Store == nil {
		return RecordingMicAuto
	}
	sess, err := h.Store.GetSession(sessionID)
	if err != nil || sess.PresetJSON == "" {
		return RecordingMicAuto
	}
	var settings store.SessionSettings
	if json.Unmarshal([]byte(sess.PresetJSON), &settings) != nil || !ValidRecordingMicMode(settings.RecordingMicMode) {
		return RecordingMicAuto
	}
	return settings.RecordingMicMode
}

// SessionSettingsChanged tells everyone in the session (admitted and
// waiting) the settings they act on, today the recording mic default.
func (h *Hub) SessionSettingsChanged(sessionID string) {
	h.broadcastRoomState(sessionID)
}
