package signaling

import (
	"encoding/json"
	"log/slog"
	"unicode/utf8"
)

// Screen share (ROADMAP P1). The room keeps at most one presenter. A client
// declares the MediaStreamTrack IDs of its share with source_add, waits for
// the roster to show it presenting, then adds the tracks to its existing
// PeerConnection and renegotiates. Nothing here touches recording: the
// sharer records its own screen locally like any other source.

// validScreenTrackIDs checks client-supplied track IDs: 1..MaxScreenTrackIDs
// distinct, non-empty, bounded strings.
func validScreenTrackIDs(ids []string) bool {
	if len(ids) == 0 || len(ids) > MaxScreenTrackIDs {
		return false
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || !utf8.ValidString(id) || utf8.RuneCountInString(id) > MaxTrackIDRunes || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func (h *Hub) handleSourceAdd(c *Client, raw json.RawMessage) {
	if c.getStatus() != "admitted" {
		return
	}
	var p SourceAddPayload
	if json.Unmarshal(raw, &p) != nil || p.Kind != SourceKindScreen || !validScreenTrackIDs(p.TrackIDs) {
		h.sendActionError(c, TypeSourceAdd, ErrCodeInvalid, "That source can't be shared.")
		return
	}
	now := h.clock()
	h.mu.Lock()
	r := h.roomLocked(c.SessionID)
	if !r.allowLocked("source:"+c.ParticipantID, ModerationRateLimit, ModerationWindow, now) {
		h.mu.Unlock()
		h.sendActionError(c, TypeSourceAdd, ErrCodeRateLimited, "Slow down.")
		return
	}
	if owner := r.screenOwner; owner != "" && owner != c.ParticipantID {
		name := "Someone"
		if other := h.sessionGroups[c.SessionID][owner]; other != nil {
			other.mu.RLock()
			if other.Name != "" {
				name = other.Name
			}
			other.mu.RUnlock()
		}
		h.mu.Unlock()
		h.sendActionError(c, TypeSourceAdd, ErrCodeConflict, name+" is already presenting. Ask them or the host to stop first.")
		return
	}
	r.screenOwner = c.ParticipantID
	h.mu.Unlock()
	if h.WebRTC != nil {
		h.WebRTC.SetScreenTracks(c.SessionID, c.ParticipantID, p.TrackIDs)
	}
	slog.Info("Screen share started", "sessionId", c.SessionID, "participantId", c.ParticipantID)
	h.broadcastRoster(c.SessionID)
}

func (h *Hub) handleSourceRemove(c *Client, raw json.RawMessage) {
	if c.getStatus() != "admitted" {
		return
	}
	var p SourceRemovePayload
	if json.Unmarshal(raw, &p) != nil || p.Kind != SourceKindScreen {
		return
	}
	if h.clearScreenShare(c.SessionID, c.ParticipantID, true) {
		h.broadcastRoster(c.SessionID)
	}
}

// handleScreenStop lets the host end someone else's share. The hub
// unpublishes it in the SFU first, so receivers lose it even if the sharer's
// client ignores the request, then tells the sharer to stop capturing.
func (h *Hub) handleScreenStop(c *Client, raw json.RawMessage) {
	if !h.requireHost(c, TypeScreenStop) {
		return
	}
	var p ScreenStopPayload
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	target := h.admittedPeer(c, p.ParticipantID)
	if target == nil || !h.clearScreenShare(c.SessionID, target.ParticipantID, true) {
		h.sendActionError(c, TypeScreenStop, ErrCodeNotFound, "That participant isn't presenting.")
		return
	}
	c.mu.RLock()
	by := c.Name
	c.mu.RUnlock()
	b, _ := json.Marshal(ScreenStopPayload{ParticipantID: target.ParticipantID, By: by})
	target.SendEnvelope(&Envelope{Type: TypeScreenStop, Payload: b})
	slog.Info("Host stopped screen share", "sessionId", c.SessionID, "participantId", target.ParticipantID)
	h.broadcastRoster(c.SessionID)
}

// clearScreenShare ends participantID's share if it is the presenter and
// reports whether it was. unpublish also removes the SFU publications (not
// needed when the participant's peer is already gone).
func (h *Hub) clearScreenShare(sessionID, participantID string, unpublish bool) bool {
	h.mu.Lock()
	r := h.rooms[sessionID]
	was := r != nil && r.screenOwner == participantID
	if was {
		r.screenOwner = ""
	}
	h.mu.Unlock()
	if was && unpublish && h.WebRTC != nil {
		h.WebRTC.SetScreenTracks(sessionID, participantID, nil)
	}
	return was
}

// ScreenOwner returns the presenting participant of a session, if any.
func (h *Hub) ScreenOwner(sessionID string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if r := h.rooms[sessionID]; r != nil {
		return r.screenOwner
	}
	return ""
}
