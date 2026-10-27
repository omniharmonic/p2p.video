package signaling

// Admission [SL-TECH-1]: hello, resume, admit and deny. The hub decides
// every participant's status; clients never set their own.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/store"
)

// AdmitParticipant admits a pending participant into the session.
func (h *Hub) AdmitParticipant(sessionID, participantID string) {
	h.mu.Lock()
	grp, ok := h.sessionGroups[sessionID]
	if !ok {
		h.mu.Unlock()
		return
	}
	target, ok := grp[participantID]
	if !ok {
		h.mu.Unlock()
		return
	}
	target.setStatus("admitted")
	h.mu.Unlock()

	_ = h.Store.UpdateParticipantStatus(participantID, "admitted")

	token := MintUploadToken(target.ParticipantID, target.SessionID, 12*time.Hour, h.Config.SecretKey)
	iceServers := BuildICEServers(h.Config, target.ParticipantID, time.Now())
	adm := AdmissionPayload{
		ActiveTakeID:  h.currentTakeID(sessionID),
		Status:        "admitted",
		Role:          target.Role,
		ParticipantID: target.ParticipantID,
		UploadToken:   token,
		IceServers:    iceServers,
	}
	raw, _ := json.Marshal(adm)
	target.SendEnvelope(&Envelope{Type: TypeAdmission, Payload: raw})
	h.afterAdmission(target)

	slog.Info("Host admitted participant", "sessionId", sessionID, "participantId", participantID)
}

// DenyParticipant rejects or removes a participant from the session. The
// host's free-text reason is logged only; the participant gets a reason code.
func (h *Hub) DenyParticipant(sessionID, participantID, reason string) {
	code := ReasonDeclined
	h.mu.RLock()
	if t := h.sessionGroups[sessionID][participantID]; t != nil && t.getStatus() == "admitted" {
		code = ReasonRemoved
	}
	h.mu.RUnlock()
	slog.Info("Host denied participant", "sessionId", sessionID, "participantId", participantID, "reason", reason)
	h.denyParticipant(sessionID, participantID, code)
}

// denyParticipant marks a participant denied in the store, which revokes its
// upload grant, TURN relay access and resume grant, tells it why, drops its
// call media and its raised hand, and refreshes everyone's roster.
func (h *Hub) denyParticipant(sessionID, participantID, code string) {
	h.mu.Lock()
	grp, ok := h.sessionGroups[sessionID]
	if !ok {
		h.mu.Unlock()
		return
	}
	target, ok := grp[participantID]
	if !ok {
		h.mu.Unlock()
		return
	}
	target.setStatus("denied")
	if r := h.rooms[sessionID]; r != nil {
		delete(r.hands, participantID)
		if r.screenOwner == participantID {
			r.screenOwner = "" // OnParticipantLeave below unpublishes it
		}
	}
	// A removed participant's phones go with it.
	children := h.subsourcesOfLocked(sessionID, participantID)
	h.mu.Unlock()

	_ = h.Store.UpdateParticipantStatus(participantID, "denied")
	h.revoked(participantID)

	adm := AdmissionPayload{
		Status:        "denied",
		Reason:        code,
		ParticipantID: target.ParticipantID,
	}
	raw, _ := json.Marshal(adm)
	target.SendEnvelope(&Envelope{Type: TypeAdmission, Payload: raw})

	if h.WebRTC != nil {
		h.WebRTC.OnParticipantLeave(sessionID, participantID)
	}
	h.broadcastRoster(sessionID)
	slog.Info("Participant denied", "sessionId", sessionID, "participantId", participantID, "reason", code)
	for _, child := range children {
		h.denyParticipant(sessionID, child, code)
	}
}

func (h *Hub) handleHello(c *Client, p *HelloPayload) {
	c.mu.Lock()
	if c.initialized {
		c.mu.Unlock()
		return
	}
	c.initialized = true
	// Denied before hello (its link was rotated away): stay denied.
	denied := c.Status == "denied"
	c.mu.Unlock()
	if denied {
		return
	}
	name := clampName(p.Name)
	if c.Role == RoleSubsource {
		// A phone is named after its participant, never by itself.
		name = h.subsourceName(c.SessionID, c.ParentParticipantID)
	}
	c.mu.Lock()
	c.Name = name
	c.mu.Unlock()
	// A signed participant grant restores identity; a caller-supplied ID never does.
	if p.ResumeToken != "" {
		if err := h.resumeClient(c, p.ResumeToken); err != nil {
			h.deny(c, ReasonUnavailable)
			return
		}
	}
	// Server-derived role: NEVER allow a client to claim host if not authenticated as host

	sess, err := h.Store.GetSession(c.SessionID)
	if err != nil {
		h.deny(c, ReasonUnavailable)
		return
	}
	// A locked room refuses new join requests; admitted participants still
	// resume with their signed grant, and the host is never locked out.
	// A phone's capability was minted for an admitted participant: it joins a
	// locked room like its parent did.
	if c.Role != "host" && c.Role != RoleSubsource && !c.resumed && h.IsRoomLocked(c.SessionID) {
		h.deny(c, ReasonRoomLocked)
		return
	}

	// A producer always waits for the host, even when the room auto-admits
	// guests: its link grants assist powers [SL-P1-11].
	shouldAdmit := (sess.AutoAdmit && c.Role != RoleProducer) || c.Role == RoleHost || c.resumed || c.Role == RoleSubsource
	// Bound the waiting room before any database row exists, so a flood of
	// join requests can neither bury the host nor grow the database.
	if !shouldAdmit && h.pendingCount(c.SessionID, c) >= MaxPendingPerSession {
		slog.Warn("Join request refused: waiting room full", "sessionId", c.SessionID, "limit", MaxPendingPerSession)
		h.denyAndClose(c, ReasonBusy)
		return
	}
	if shouldAdmit {
		c.setStatus("admitted")
	} else {
		c.setStatus("pending")
	}

	part, _ := h.Store.GetParticipant(c.ParticipantID)
	if part == nil {
		_ = h.Store.CreateParticipant(&store.Participant{
			ID:                  c.ParticipantID,
			SessionID:           c.SessionID,
			Name:                c.Name,
			Role:                c.Role,
			ParentParticipantID: c.ParentParticipantID,
			Status:              c.getStatus(),
			JoinedAt:            time.Now().UTC(),
		})
	} else {
		_ = h.Store.UpdateParticipantStatus(c.ParticipantID, c.getStatus())
	}

	if shouldAdmit {
		token := MintUploadToken(c.ParticipantID, c.SessionID, 12*time.Hour, h.Config.SecretKey)

		// ICE servers (policy STUN + participant-bound TURN); admitted only.
		iceServers := BuildICEServers(h.Config, c.ParticipantID, time.Now())

		adm := AdmissionPayload{
			ActiveTakeID:  h.currentTakeID(c.SessionID),
			Status:        "admitted",
			Role:          c.Role,
			ParticipantID: c.ParticipantID,
			UploadToken:   token,
			IceServers:    iceServers,
		}
		if c.Role == RoleSubsource {
			adm.ParentParticipantID, adm.Name = c.ParentParticipantID, c.Name
		}
		raw, _ := json.Marshal(adm)
		h.markPublishOnly(c)
		c.SendEnvelope(&Envelope{Type: TypeAdmission, Payload: raw})
		h.afterAdmission(c)
	} else {
		c.setStatus("pending")
		_ = h.Store.UpdateParticipantStatus(c.ParticipantID, "pending")

		adm := AdmissionPayload{
			Status:        "pending",
			ParticipantID: c.ParticipantID,
		}
		raw, _ := json.Marshal(adm)
		c.SendEnvelope(&Envelope{Type: TypeAdmission, Payload: raw})
		// Tell the waiting room whether the session is being recorded
		// and whether live captions are on.
		h.sendRoomState(c)
		h.sendCaptionState(c)

		// Notify whoever may admit this request: the host always, admitted
		// producers for guests only [SL-P1-11].
		reqRaw, _ := json.Marshal(map[string]string{
			"participantId": c.ParticipantID,
			"name":          c.Name,
			"role":          c.Role,
		})
		env := &Envelope{Type: TypeJoinRequest, Payload: reqRaw}
		if c.Role == RoleGuest {
			h.sendToAssistants(c.SessionID, env)
		} else {
			h.SendToHost(c.SessionID, env)
		}
	}
}

func (h *Hub) deny(c *Client, reason string) {
	c.setStatus("denied")
	adm := AdmissionPayload{
		Status:        "denied",
		Reason:        reason,
		ParticipantID: c.ParticipantID,
	}
	raw, _ := json.Marshal(adm)
	c.SendEnvelope(&Envelope{Type: TypeAdmission, Payload: raw})
}

// resumeClient binds a new socket to a previously admitted participant. The
// in-memory grant is never a host credential and cannot cross sessions or roles.
func (h *Hub) resumeClient(c *Client, token string) error {
	claims, err := VerifyUploadToken(token, h.Config.SecretKey)
	if err != nil || claims.SessionID != c.SessionID {
		return fmt.Errorf("invalid resume grant")
	}
	part, err := h.Store.GetParticipant(claims.ParticipantID)
	if err != nil || part == nil || part.SessionID != c.SessionID || part.Role != c.Role || part.Status == "denied" {
		return fmt.Errorf("resume identity unavailable")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if old := h.clients[claims.ParticipantID]; old != nil && old != c {
		old.close()
		if h.WebRTC != nil {
			h.WebRTC.OnParticipantLeave(old.SessionID, old.ParticipantID)
		}
	}
	delete(h.clients, c.ParticipantID)
	if grp := h.sessionGroups[c.SessionID]; grp != nil {
		delete(grp, c.ParticipantID)
	}
	// A resumed client re-declares any screen share it still has before it
	// re-offers media; until then it is not presenting.
	if r := h.rooms[c.SessionID]; r != nil && r.screenOwner == claims.ParticipantID {
		r.screenOwner = ""
		if h.WebRTC != nil {
			h.WebRTC.SetScreenTracks(c.SessionID, claims.ParticipantID, nil)
		}
	}
	c.ParticipantID = claims.ParticipantID
	c.resumed = true
	h.clients[c.ParticipantID] = c
	h.sessionGroups[c.SessionID][c.ParticipantID] = c
	return nil
}
