package signaling

import "encoding/json"

// handleNetStats keeps an admitted participant's latest connection report for
// the host dashboard and relays it to the other admitted members of the
// session [SL-P0-8]. Identity comes from the authenticated connection, never
// from the payload; reports faster than NetStatsMinIntervalMs are dropped.
func (h *Hub) handleNetStats(c *Client, raw json.RawMessage) {
	if c.getStatus() != "admitted" {
		return
	}
	var p NetStatsPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return
	}
	clean := SanitizeNetStats(p)
	now := h.clock()
	c.mu.Lock()
	if prev := c.netStats; prev != nil && now.UnixMilli()-prev.ReceivedAtMs < NetStatsMinIntervalMs {
		c.mu.Unlock()
		return
	}
	clean.ParticipantID = c.ParticipantID
	clean.Name = c.Name
	clean.ReceivedAtMs = now.UnixMilli()
	c.netStats = &clean
	sessionID := c.SessionID
	c.mu.Unlock()

	out, _ := json.Marshal(clean)
	env := &Envelope{Type: TypeNetStats, ParticipantID: clean.ParticipantID, Payload: out}
	h.mu.RLock()
	others := make([]*Client, 0, len(h.sessionGroups[sessionID]))
	for _, other := range h.sessionGroups[sessionID] {
		if other != c {
			others = append(others, other)
		}
	}
	h.mu.RUnlock()
	for _, other := range others {
		if other.getStatus() == "admitted" {
			other.SendEnvelope(env)
		}
	}
}

func copyNetStats(p *NetStatsPayload) *NetStatsPayload {
	if p == nil {
		return nil
	}
	cp := *p
	cp.Reasons = append([]string(nil), p.Reasons...)
	if p.Upload != nil {
		u := *p.Upload
		cp.Upload = &u
	}
	return &cp
}
