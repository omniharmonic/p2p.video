package signaling

// Message dispatch [SL-TECH-1]: HandleMessage routes each client envelope by
// type, and relays mic/camera state between participants.

import (
	"encoding/json"
	"log/slog"

	"github.com/omniharmonic/studiolink/host/internal/clock"
)

// HandleMessage parses and routes incoming messages from a client.
func (h *Hub) HandleMessage(c *Client, msg []byte) {
	var env Envelope
	if err := json.Unmarshal(msg, &env); err != nil {
		slog.Error("Invalid envelope JSON", "err", err)
		return
	}

	if c.Role == RoleSubsource && !subsourceAllowed[env.Type] {
		h.sendActionError(c, env.Type, ErrCodeForbidden, "A phone camera can't do that.")
		return
	}
	switch env.Type {
	case TypeHello:
		var p HelloPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return
		}
		h.handleHello(c, &p)

	case TypeClockPing:
		t1 := clock.MonotonicNowUs()
		var p ClockPingPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return
		}
		t2 := clock.MonotonicNowUs()
		// Respond with clock_pong
		pong := ClockPongPayload{
			T0: p.T0,
			T1: t1,
			T2: t2,
		}
		raw, _ := json.Marshal(pong)
		c.SendEnvelope(&Envelope{
			Type:          TypeClockPong,
			ParticipantID: c.ParticipantID,
			Payload:       raw,
		})

	case TypeSdpOffer:
		if c.getStatus() != "admitted" {
			slog.Warn("Ignoring SDP offer from unadmitted client", "participantId", c.ParticipantID, "status", c.getStatus())
			return
		}
		var p SdpPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return
		}
		if h.WebRTC != nil && p.Renegotiate {
			ans, err := h.WebRTC.HandleRenegotiation(c.SessionID, c.ParticipantID, p.SDP)
			if err != nil {
				// Glare is resolved by the client rolling back when the
				// server's outstanding offer reaches it; anything else is
				// recovered by the client's renegotiation timeout.
				slog.Info("Client renegotiation not applied", "participantId", c.ParticipantID, "err", err)
				return
			}
			ansRaw, _ := json.Marshal(SdpPayload{SDP: ans, Type: "answer", Renegotiate: true})
			c.SendEnvelope(&Envelope{Type: TypeSdpAnswer, ParticipantID: c.ParticipantID, Payload: ansRaw})
		} else if h.WebRTC != nil {
			ans, err := h.WebRTC.HandleOffer(c.SessionID, c.ParticipantID, p.SDP)
			if err != nil {
				slog.Error("Failed to handle SDP offer", "participantId", c.ParticipantID, "err", err)
				return
			}
			ansRaw, _ := json.Marshal(SdpPayload{SDP: ans, Type: "answer"})
			c.SendEnvelope(&Envelope{
				Type:          TypeSdpAnswer,
				ParticipantID: c.ParticipantID,
				Payload:       ansRaw,
			})
		}

	case TypeSdpAnswer:
		if c.getStatus() != "admitted" {
			return
		}
		var p SdpPayload
		if json.Unmarshal(env.Payload, &p) == nil && h.WebRTC != nil {
			if err := h.WebRTC.HandleAnswer(c.SessionID, c.ParticipantID, p.SDP); err != nil {
				slog.Warn("Invalid SDP answer", "err", err)
			}
		}
	case TypeIceCandidate:
		if c.getStatus() != "admitted" {
			slog.Warn("Ignoring ICE candidate from unadmitted client", "participantId", c.ParticipantID, "status", c.getStatus())
			return
		}
		var p IceCandidatePayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return
		}
		if h.WebRTC != nil {
			_ = h.WebRTC.HandleCandidate(c.SessionID, c.ParticipantID, p.Candidate, p.SDPMid, p.SDPMLineIndex)
		}

	case TypeAdmit:
		var p AdmitPayload
		if err := json.Unmarshal(env.Payload, &p); err == nil && p.ParticipantID != "" && h.mayAdmitOrDeny(c, TypeAdmit, p.ParticipantID) {
			h.AdmitParticipant(c.SessionID, p.ParticipantID)
		}

	case TypeDeny:
		var p DenyPayload
		if err := json.Unmarshal(env.Payload, &p); err == nil && p.ParticipantID != "" && h.mayAdmitOrDeny(c, TypeDeny, p.ParticipantID) {
			h.DenyParticipant(c.SessionID, p.ParticipantID, p.Reason)
		}

	case TypeRecorderStatus:
		if c.getStatus() != "admitted" {
			return
		}
		var p RecorderStatusPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return
		}
		p, ok := SanitizeRecorderStatus(p)
		if !ok {
			return
		}
		tr, err := h.Store.GetTrack(p.TrackID)
		if err != nil {
			return
		}
		src, err := h.Store.GetSource(tr.SourceID)
		if err != nil || src.ParticipantID != c.ParticipantID {
			return
		}
		c.mu.Lock()
		if c.Tracks == nil {
			c.Tracks = make(map[string]RecorderStatusPayload)
		}
		c.Tracks[p.TrackID] = p
		c.mu.Unlock()

	case TypeParticipantState:
		if c.getStatus() != "admitted" {
			return
		}
		var p ParticipantStatePayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return
		}
		h.relayParticipantState(c, p)
		h.broadcastRoster(c.SessionID)

	case TypeChat:
		h.handleChat(c, env.Payload)
	case TypeHand:
		h.handleHand(c, env.Payload)
	case TypeReaction:
		h.handleReaction(c, env.Payload)
	case TypeMuteRequest:
		h.handleMuteRequest(c, env.Payload)
	case TypeRemove:
		h.handleRemove(c, env.Payload)
	case TypeRoomLock:
		h.handleRoomLock(c, env.Payload)
	case TypeSourceAdd:
		h.handleSourceAdd(c, env.Payload)
	case TypeSourceRemove:
		h.handleSourceRemove(c, env.Payload)
	case TypeScreenStop:
		h.handleScreenStop(c, env.Payload)

	case TypePreflight:
		h.storePreflight(c, env.Payload)
	case TypeNetStats:
		h.handleNetStats(c, env.Payload)
	case TypeCaptionControl:
		h.handleCaptionControl(c, env.Payload)
	case TypePrompterEdit:
		h.handlePrompterEdit(c, env.Payload)
	case TypePrompterScroll:
		h.handlePrompterScroll(c, env.Payload)
	case TypeLowerThird:
		h.handleLowerThird(c, env.Payload)

	case TypeRecordStart:
		// The host, or an admitted producer assisting it [SL-P1-11].
		if h.requireAssist(c, TypeRecordStart) {
			h.startRecording(c.SessionID)
			h.broadcastRoomState(c.SessionID)
		}

	case TypeRecordStop:
		if h.requireAssist(c, TypeRecordStop) {
			h.stopRecording(c.SessionID)
			h.broadcastRoomState(c.SessionID)
		}

	case TypeChirp:
		if h.requireHost(c, TypeChirp) {
			var p ChirpPayload
			if err := json.Unmarshal(env.Payload, &p); err == nil {
				p.AtHostUs = clock.MonotonicNowUs() + 500_000 // 500ms delay
				raw, _ := json.Marshal(p)
				h.Broadcast(c.SessionID, &Envelope{Type: TypeChirp, Payload: raw})
			}
		}
	}
}

// relayParticipantState stamps the sender's server-side identity onto its
// mic/camera state and relays it to the admitted members of its session. A
// connection's first state message also returns everyone else's latest state,
// so late joiners and reconnects render indicators without polling.
func (h *Hub) relayParticipantState(c *Client, p ParticipantStatePayload) {
	c.mu.Lock()
	p.ParticipantID = c.ParticipantID
	p.Name = c.Name
	state := p
	c.mediaState = &state
	needsSnapshot := !c.mediaStateSynced
	c.mediaStateSynced = true
	sessionID := c.SessionID
	c.mu.Unlock()

	raw, _ := json.Marshal(p)
	h.Broadcast(sessionID, &Envelope{Type: TypeParticipantState, ParticipantID: p.ParticipantID, Payload: raw})
	if !needsSnapshot {
		return
	}
	h.mu.RLock()
	others := make([]*Client, 0, len(h.sessionGroups[sessionID]))
	for _, other := range h.sessionGroups[sessionID] {
		if other != c {
			others = append(others, other)
		}
	}
	h.mu.RUnlock()
	for _, other := range others {
		if other.getStatus() != "admitted" {
			continue
		}
		other.mu.RLock()
		var snapshot *ParticipantStatePayload
		if other.mediaState != nil {
			cp := *other.mediaState
			snapshot = &cp
		}
		other.mu.RUnlock()
		if snapshot != nil {
			raw, _ := json.Marshal(snapshot)
			c.SendEnvelope(&Envelope{Type: TypeParticipantState, ParticipantID: snapshot.ParticipantID, Payload: raw})
		}
	}
}
