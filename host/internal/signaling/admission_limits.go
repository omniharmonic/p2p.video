package signaling

import (
	"log/slog"
	"time"
)

// Join-request abuse limits [SL-SEC-1]. Invite and room links can reach the
// public internet (Funnel), so a leaked link must not let anyone flood the
// host with join requests or the database with participant rows. Rate limits
// per remote address and per session are applied before the WebSocket
// upgrade (httpserver); these are the hub's own bounds.
const (
	// MaxPendingPerSession is how many join requests may wait for the host
	// at once. Further requests are denied (ReasonBusy) without a database row.
	MaxPendingPerSession = 20
	// deniedCloseGrace lets a denial reach the client before its socket closes.
	deniedCloseGrace = 2 * time.Second
)

// HelloTimeout is how long a new socket may stay registered without sending
// hello before it is closed. A variable only so tests can shorten it.
var HelloTimeout = 10 * time.Second

// HelloReceived reports whether the client has sent its hello.
func (c *Client) HelloReceived() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.initialized
}

// pendingCount counts join requests waiting for the host in a session,
// excluding except. Pre-hello sockets do not count: they are bounded by
// HelloTimeout and the upgrade rate limits instead.
func (h *Hub) pendingCount(sessionID string, except *Client) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := 0
	for _, c := range h.sessionGroups[sessionID] {
		if c != except && c.Role != "host" && c.HelloReceived() && c.getStatus() == "pending" {
			n++
		}
	}
	return n
}

// denyAndClose refuses a join attempt and closes its socket after a grace.
func (h *Hub) denyAndClose(c *Client, reason string) {
	h.deny(c, reason)
	time.AfterFunc(deniedCloseGrace, c.close)
}

// DenyPendingWithCredential denies every not-yet-admitted socket in a session
// that joined with credential (a room link nonce or an invite hash, see
// Client.JoinCredential) and closes it. Call it when that credential is
// rotated or revoked so people already waiting under the old link do not get
// admitted anyway [SL-SEC-1]. Admitted participants are unaffected.
func (h *Hub) DenyPendingWithCredential(sessionID, credential string) int {
	if credential == "" {
		return 0
	}
	h.mu.RLock()
	var targets []*Client
	for _, c := range h.sessionGroups[sessionID] {
		if c.Role != "host" && c.JoinCredential == credential && c.getStatus() != "admitted" {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range targets {
		h.denyParticipant(sessionID, c.ParticipantID, ReasonUnavailable)
		time.AfterFunc(deniedCloseGrace, c.close)
	}
	if len(targets) > 0 {
		slog.Info("Denied waiting participants after their link was replaced", "sessionId", sessionID, "count", len(targets))
	}
	return len(targets)
}

// revoked tells the relay (and anything else holding per-participant grants)
// that a participant lost access, so live TURN allocations close now rather
// than at their next refresh.
func (h *Hub) revoked(participantID string) {
	if h.OnParticipantRevoked != nil {
		h.OnParticipantRevoked(participantID)
	}
}
