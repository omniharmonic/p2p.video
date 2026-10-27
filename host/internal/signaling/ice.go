package signaling

import (
	"time"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/turn"
)

// BuildICEServers returns the ICE servers for an ADMITTED participant
// (ARCHITECTURE §3.3). Callers must never use it for pending or denied
// participants: the TURN entry carries a participant-bound credential.
//
//   - public STUN, only when policy allows it (ADR-005);
//   - the embedded TURN relay, only when it is running (secret present) and at
//     least one URL is advertised, with REST credentials bound to participantID.
func BuildICEServers(cfg *config.Config, participantID string, now time.Time) []ICEServer {
	servers := []ICEServer{}
	if cfg == nil {
		return servers
	}
	if cfg.PublicSTUN && len(cfg.STUNURLs) > 0 {
		servers = append(servers, ICEServer{URLs: append([]string(nil), cfg.STUNURLs...)})
	}
	if len(cfg.TURNSecret) > 0 && len(cfg.TURNURLs) > 0 && participantID != "" {
		c := turn.Mint(cfg.TURNSecret, participantID, turn.DefaultTTL, now)
		servers = append(servers, ICEServer{URLs: append([]string(nil), cfg.TURNURLs...), Username: c.Username, Credential: c.Credential})
	}
	return servers
}

// ParticipantMayRelay reports whether a participant's TURN credentials should
// still be honoured: it must have been admitted and not since denied/removed.
// A transient disconnect keeps relay access (the socket may be resuming); the
// credential TTL still bounds it.
func (h *Hub) ParticipantMayRelay(participantID string) bool {
	if h == nil || h.Store == nil {
		return false
	}
	p, err := h.Store.GetParticipant(participantID)
	if err != nil || p == nil {
		return false
	}
	if p.Status != "admitted" && p.Status != "disconnected" {
		return false
	}
	// An ended meeting's participants are all "disconnected": not a resume.
	sess, err := h.Store.GetSession(p.SessionID)
	return err == nil && sess.EndedAt == nil
}
