package signaling

// Dashboard telemetry [SL-TECH-1]: the periodic host/producer source-health
// snapshot and the pre-flight reports it carries.

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/store"
)

func (h *Hub) dashboardLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
			h.emitDashboardTelemetry()
		}
	}
}

func (h *Hub) emitDashboardTelemetry() {
	h.mu.RLock()
	sessionIDs := make([]string, 0, len(h.sessionGroups))
	for sid := range h.sessionGroups {
		sessionIDs = append(sessionIDs, sid)
	}
	h.mu.RUnlock()

	for _, sid := range sessionIDs {
		h.mu.RLock()
		// Arrival order: the admission banner shows the first waiting person,
		// and map order would reshuffle it every second [SL-QA-1].
		type member struct {
			c   *Client
			seq uint64
		}
		grp := make([]member, 0, len(h.sessionGroups[sid]))
		for _, c := range h.sessionGroups[sid] {
			grp = append(grp, member{c, c.registeredSeq})
		}
		var activeTk *store.Take
		if tk := h.activeTake[sid]; tk != nil {
			cp := *tk
			activeTk = &cp
		}
		refreshAt := h.preflightRefresh[sid]
		sess, _ := h.Store.GetSession(sid)
		h.mu.RUnlock()

		if sess == nil || len(grp) == 0 {
			continue
		}

		var parts []DashboardParticipantState
		// The host and admitted producers see the dashboard [SL-P1-11].
		var viewers []*Client

		sort.Slice(grp, func(i, j int) bool {
			if grp[i].seq != grp[j].seq {
				return grp[i].seq < grp[j].seq
			}
			return grp[i].c.ParticipantID < grp[j].c.ParticipantID
		})
		for _, m := range grp {
			c := m.c
			if isAssistant(c) {
				viewers = append(viewers, c)
			}
			c.mu.RLock()
			tracksCopy := make(map[string]RecorderStatusPayload)
			for k, v := range c.Tracks {
				tracksCopy[k] = v
			}
			parts = append(parts, DashboardParticipantState{
				ParticipantID: c.ParticipantID,
				Name:          c.Name,
				Role:          c.Role,
				Status:        c.Status,
				ClockOffsetUs: c.ClockOffsetUs,
				RTTUs:         c.RTTUs,
				Tracks:        tracksCopy,
				Preflight:     copyPreflight(c.preflight),
				NetStats:      copyNetStats(c.netStats),
			})
			c.mu.RUnlock()
		}

		currentTakeNum := 0
		isRecording := false
		if activeTk != nil {
			currentTakeNum = activeTk.Number
			isRecording = true
		}

		dash := DashboardPayload{
			SessionID:            sid,
			Title:                sess.Title,
			CurrentTake:          currentTakeNum,
			IsRecording:          isRecording,
			PreflightRefreshAtMs: refreshAt,
			Participants:         parts,
		}
		dashRaw, _ := json.Marshal(dash)
		dashEnv := &Envelope{Type: TypeDashboard, Payload: dashRaw}

		for _, v := range viewers {
			v.SendEnvelope(dashEnv)
		}
	}
}

// storePreflight keeps a pending or admitted participant's latest pre-flight
// checklist for the host dashboard [SL-P0-5]. Identity comes from the
// authenticated connection, never from the payload.
func (h *Hub) storePreflight(c *Client, raw json.RawMessage) {
	if status := c.getStatus(); status != "admitted" && status != "pending" {
		return
	}
	var p PreflightPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return
	}
	clean := SanitizePreflight(p)
	c.mu.Lock()
	clean.ParticipantID = c.ParticipantID
	clean.Name = c.Name
	clean.ReceivedAtMs = time.Now().UnixMilli()
	c.preflight = &clean
	sessionID := c.SessionID
	c.mu.Unlock()
	// A refresh precedes a take, which a producer may also start [SL-P1-11].
	if !p.Refresh || !isAssistant(c) {
		return
	}
	refresh, _ := json.Marshal(PreflightPayload{Refresh: true, Items: []PreflightItem{}})
	env := &Envelope{Type: TypePreflight, Payload: refresh}
	h.mu.Lock()
	h.preflightRefresh[sessionID] = clean.ReceivedAtMs
	others := make([]*Client, 0, len(h.sessionGroups[sessionID]))
	for _, other := range h.sessionGroups[sessionID] {
		if other != c {
			others = append(others, other)
		}
	}
	h.mu.Unlock()
	for _, other := range others {
		if other.getStatus() == "admitted" {
			other.SendEnvelope(env)
		}
	}
}

func copyPreflight(p *PreflightPayload) *PreflightPayload {
	if p == nil {
		return nil
	}
	cp := *p
	cp.Items = append([]PreflightItem(nil), p.Items...)
	return &cp
}
