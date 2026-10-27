package signaling

import (
	"encoding/json"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// room is the in-memory collaboration state of one session: chat history,
// raised hands, the lock and per-participant rate-limit windows. It lives for
// the daemon's lifetime (bounded by ChatHistoryLimit) and is guarded by Hub.mu.
// Nothing here is read by the recording or upload pipeline.
type room struct {
	locked     bool
	chat       []ChatMessage
	chatSeq    uint64
	hands      map[string]int64 // participantID -> raised at (Unix ms)
	lastHandAt int64
	windows    map[string][]time.Time // "<action>:<participantID>" -> recent events
	// screenOwner is the participant presenting a screen share, if any
	// (screenshare.go). One share per room.
	screenOwner string
	// prompter is the session's teleprompter (production.go) [SL-P5-1].
	prompter *prompterState
}

// roomLocked returns the session's room, creating it. Caller holds h.mu.
func (h *Hub) roomLocked(sessionID string) *room {
	r := h.rooms[sessionID]
	if r == nil {
		r = &room{hands: map[string]int64{}, windows: map[string][]time.Time{}}
		h.rooms[sessionID] = r
	}
	return r
}

func (h *Hub) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// allowLocked applies a sliding-window rate limit. Keys are per participant,
// not per connection, so reconnecting does not reset a limit. Caller holds h.mu.
func (r *room) allowLocked(key string, limit int, window time.Duration, now time.Time) bool {
	cutoff := now.Add(-window)
	recent := r.windows[key][:0]
	for _, t := range r.windows[key] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) >= limit {
		r.windows[key] = recent
		return false
	}
	r.windows[key] = append(recent, now)
	return true
}

// IsRoomLocked reports whether new join requests are refused for a session.
func (h *Hub) IsRoomLocked(sessionID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	r := h.rooms[sessionID]
	return r != nil && r.locked
}

func (h *Hub) sendActionError(c *Client, action, code, message string) {
	raw, _ := json.Marshal(ActionErrorPayload{Action: action, Code: code, Message: message})
	c.SendEnvelope(&Envelope{Type: TypeActionError, Payload: raw})
}

// requireHost enforces host-only collaboration actions server-side.
func (h *Hub) requireHost(c *Client, action string) bool {
	if c.Role == "host" && c.getStatus() == "admitted" {
		return true
	}
	slog.Warn("Rejected host-only action", "action", action, "participantId", c.ParticipantID, "role", c.Role)
	h.sendActionError(c, action, ErrCodeForbidden, "Only the host can do that.")
	return false
}

// admittedPeer returns an admitted participant of c's session other than c.
func (h *Hub) admittedPeer(c *Client, participantID string) *Client {
	h.mu.RLock()
	target := h.sessionGroups[c.SessionID][participantID]
	h.mu.RUnlock()
	if target == nil || target == c || target.getStatus() != "admitted" {
		return nil
	}
	return target
}

// MaxNameRunes bounds the display name a client announces in hello.
const MaxNameRunes = 80

// clampName sanitizes an untrusted display name (SanitizeDisplayText) and
// bounds its length.
func clampName(name string) string {
	return SanitizeDisplayText(name, MaxNameRunes)
}

// SanitizeDisplayText makes untrusted single-line text safe to show to other
// people: invalid UTF-8, control characters (Cc) and format characters (Cf:
// bidi overrides U+202A–202E and isolates U+2066–2069, zero-width joiners,
// BOM…) are removed, every run of whitespace becomes one space, the result is
// trimmed, and it is cut to maxRunes. Bidi overrides would otherwise let a
// guest's name visually reorder the host's UI text [SL-SEC-1].
func SanitizeDisplayText(s string, maxRunes int) string {
	s = strings.ToValidUTF8(s, "")
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			space = true
			continue
		case unicode.Is(unicode.Cc, r), unicode.Is(unicode.Cf, r):
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	out := b.String()
	if maxRunes > 0 && utf8.RuneCountInString(out) > maxRunes {
		out = strings.TrimSpace(string([]rune(out)[:maxRunes]))
	}
	return out
}

// normalizeChatText trims and validates untrusted chat text.
func normalizeChatText(text string) (string, bool) {
	if !utf8.ValidString(text) {
		return "", false
	}
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if text == "" || utf8.RuneCountInString(text) > ChatMaxRunes {
		return "", false
	}
	return text, true
}

func (h *Hub) handleChat(c *Client, raw json.RawMessage) {
	if c.getStatus() != "admitted" {
		return
	}
	var p ChatSendPayload
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	text, ok := normalizeChatText(p.Text)
	if !ok {
		h.sendActionError(c, TypeChat, ErrCodeInvalid, "Messages must be 1–"+strconv.Itoa(ChatMaxRunes)+" characters.")
		return
	}
	now := h.clock()
	c.mu.RLock()
	name, role := c.Name, c.Role
	c.mu.RUnlock()
	h.mu.Lock()
	r := h.roomLocked(c.SessionID)
	if !r.allowLocked("chat:"+c.ParticipantID, ChatRateLimit, ChatRateWindow, now) {
		h.mu.Unlock()
		h.sendActionError(c, TypeChat, ErrCodeRateLimited, "You're sending messages too quickly. Wait a moment and try again.")
		return
	}
	r.chatSeq++
	msg := ChatMessage{ID: "m" + strconv.FormatUint(r.chatSeq, 10), ParticipantID: c.ParticipantID, Name: name, Role: role, Text: text, TS: now.UnixMilli()}
	r.chat = append(r.chat, msg)
	if over := len(r.chat) - ChatHistoryLimit; over > 0 {
		r.chat = slices.Clone(r.chat[over:])
	}
	h.mu.Unlock()
	// Never log chat contents.
	out, _ := json.Marshal(msg)
	h.Broadcast(c.SessionID, &Envelope{Type: TypeChat, ParticipantID: c.ParticipantID, Payload: out})
}

// sendChatHistory replays recent chat to a newly admitted or resumed client.
func (h *Hub) sendChatHistory(c *Client) {
	h.mu.RLock()
	var msgs []ChatMessage
	if r := h.rooms[c.SessionID]; r != nil {
		msgs = slices.Clone(r.chat)
	}
	h.mu.RUnlock()
	if msgs == nil {
		msgs = []ChatMessage{}
	}
	raw, _ := json.Marshal(ChatHistoryPayload{Messages: msgs})
	c.SendEnvelope(&Envelope{Type: TypeChatHistory, Payload: raw})
}

func (h *Hub) handleHand(c *Client, raw json.RawMessage) {
	if c.getStatus() != "admitted" {
		return
	}
	var p HandPayload
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	target := c.ParticipantID
	if p.ParticipantID != "" && p.ParticipantID != c.ParticipantID {
		// Only the host may act on someone else's hand, and only to lower it.
		if !h.requireHost(c, TypeHand) {
			return
		}
		if p.Raised {
			h.sendActionError(c, TypeHand, ErrCodeInvalid, "You can only lower someone else's hand.")
			return
		}
		if h.admittedPeer(c, p.ParticipantID) == nil {
			h.sendActionError(c, TypeHand, ErrCodeNotFound, "That participant is no longer in the call.")
			return
		}
		target = p.ParticipantID
	}
	now := h.clock()
	h.mu.Lock()
	r := h.roomLocked(c.SessionID)
	if !r.allowLocked("hand:"+c.ParticipantID, ModerationRateLimit, ModerationWindow, now) {
		h.mu.Unlock()
		h.sendActionError(c, TypeHand, ErrCodeRateLimited, "Slow down.")
		return
	}
	_, already := r.hands[target]
	changed := false
	if p.Raised && !already {
		at := now.UnixMilli()
		if at <= r.lastHandAt {
			at = r.lastHandAt + 1
		}
		r.lastHandAt = at
		r.hands[target] = at
		changed = true
	} else if !p.Raised && already {
		delete(r.hands, target)
		changed = true
	}
	h.mu.Unlock()
	if changed {
		h.broadcastRoster(c.SessionID)
	}
}

func (h *Hub) handleReaction(c *Client, raw json.RawMessage) {
	if c.getStatus() != "admitted" {
		return
	}
	var p ReactionPayload
	if json.Unmarshal(raw, &p) != nil || !slices.Contains(AllowedReactions, p.Emoji) {
		h.sendActionError(c, TypeReaction, ErrCodeInvalid, "That reaction isn't available.")
		return
	}
	now := h.clock()
	h.mu.Lock()
	ok := h.roomLocked(c.SessionID).allowLocked("reaction:"+c.ParticipantID, ReactionRateLimit, ReactionRateWindow, now)
	h.mu.Unlock()
	if !ok {
		h.sendActionError(c, TypeReaction, ErrCodeRateLimited, "Too many reactions. Wait a moment.")
		return
	}
	c.mu.RLock()
	out := ReactionPayload{ParticipantID: c.ParticipantID, Name: c.Name, Emoji: p.Emoji}
	c.mu.RUnlock()
	b, _ := json.Marshal(out)
	h.Broadcast(c.SessionID, &Envelope{Type: TypeReaction, ParticipantID: c.ParticipantID, Payload: b})
}

// handleMuteRequest asks a participant's client to mute its call microphone.
// The client applies it; recording is not affected by this request.
func (h *Hub) handleMuteRequest(c *Client, raw json.RawMessage) {
	if !h.requireAssist(c, TypeMuteRequest) {
		return
	}
	var p MuteRequestPayload
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	target := h.admittedPeer(c, p.ParticipantID)
	if target == nil {
		h.sendActionError(c, TypeMuteRequest, ErrCodeNotFound, "That participant is no longer in the call.")
		return
	}
	// A producer mutes guests and other producers, never the host [SL-P1-11].
	if c.Role == RoleProducer && target.Role == RoleHost {
		h.sendActionError(c, TypeMuteRequest, ErrCodeForbidden, "A producer can't mute the host.")
		return
	}
	c.mu.RLock()
	by := c.Name
	c.mu.RUnlock()
	b, _ := json.Marshal(MuteRequestPayload{ParticipantID: target.ParticipantID, By: by})
	target.SendEnvelope(&Envelope{Type: TypeMuteRequest, Payload: b})
	slog.Info("Host requested participant mute", "sessionId", c.SessionID, "participantId", target.ParticipantID)
}

// handleRemove removes an admitted participant through the deny/revoke path:
// the store status becomes "denied", which revokes the upload grant, TURN
// relay access and session resume.
func (h *Hub) handleRemove(c *Client, raw json.RawMessage) {
	if !h.requireAssist(c, TypeRemove) {
		return
	}
	var p RemovePayload
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	target := h.admittedPeer(c, p.ParticipantID)
	if target == nil {
		h.sendActionError(c, TypeRemove, ErrCodeNotFound, "That participant is no longer in the call.")
		return
	}
	if target.Role == RoleHost {
		h.sendActionError(c, TypeRemove, ErrCodeForbidden, "The host can't be removed.")
		return
	}
	// A producer removes guests (and their phones), never another producer.
	if c.Role == RoleProducer && target.Role == RoleProducer {
		h.sendActionError(c, TypeRemove, ErrCodeForbidden, "Only the host can remove a producer.")
		return
	}
	h.denyParticipant(c.SessionID, target.ParticipantID, ReasonRemoved)
}

func (h *Hub) handleRoomLock(c *Client, raw json.RawMessage) {
	if !h.requireHost(c, TypeRoomLock) {
		return
	}
	var p RoomLockPayload
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	h.mu.Lock()
	h.roomLocked(c.SessionID).locked = p.Locked
	h.mu.Unlock()
	slog.Info("Room lock changed", "sessionId", c.SessionID, "locked", p.Locked)
	h.broadcastRoster(c.SessionID)
	h.broadcastRoomState(c.SessionID)
}

// rosterFor builds the admitted-participant list for a session.
func (h *Hub) rosterFor(sessionID string) RosterPayload {
	h.mu.RLock()
	clients := make([]*Client, 0, len(h.sessionGroups[sessionID]))
	for _, c := range h.sessionGroups[sessionID] {
		clients = append(clients, c)
	}
	var hands map[string]int64
	locked := false
	presenter := ""
	if r := h.rooms[sessionID]; r != nil {
		hands = make(map[string]int64, len(r.hands))
		for k, v := range r.hands {
			hands[k] = v
		}
		locked = r.locked
		presenter = r.screenOwner
	}
	h.mu.RUnlock()
	out := RosterPayload{Participants: []RosterEntry{}, Locked: locked}
	for _, c := range clients {
		c.mu.RLock()
		if c.Status == "admitted" {
			e := RosterEntry{ParticipantID: c.ParticipantID, Name: c.Name, Role: c.Role, ParentParticipantID: c.ParentParticipantID}
			if c.mediaState != nil {
				e.MicMuted, e.CamOff = c.mediaState.MicMuted, c.mediaState.CamOff
			}
			if at, ok := hands[c.ParticipantID]; ok {
				e.HandRaised, e.HandRaisedAt = true, at
			}
			e.ScreenSharing = presenter != "" && presenter == c.ParticipantID
			out.Participants = append(out.Participants, e)
		}
		c.mu.RUnlock()
	}
	sort.Slice(out.Participants, func(i, j int) bool {
		a, b := out.Participants[i], out.Participants[j]
		if (a.Role == "host") != (b.Role == "host") {
			return a.Role == "host"
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ParticipantID < b.ParticipantID
	})
	return out
}

// broadcastRoster sends the current roster to every admitted participant.
func (h *Hub) broadcastRoster(sessionID string) {
	raw, _ := json.Marshal(h.rosterFor(sessionID))
	h.Broadcast(sessionID, &Envelope{Type: TypeRoster, Payload: raw})
}

func (h *Hub) roomStateFor(sessionID string) RoomStatePayload {
	st := RoomStatePayload{RecordingMicMode: h.recordingMicDefault(sessionID)}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if r := h.rooms[sessionID]; r != nil {
		st.Locked = r.locked
	}
	if tk := h.activeTake[sessionID]; tk != nil {
		st.Recording, st.TakeNumber = true, tk.Number
	}
	return st
}

func (h *Hub) sendRoomState(c *Client) {
	raw, _ := json.Marshal(h.roomStateFor(c.SessionID))
	c.SendEnvelope(&Envelope{Type: TypeRoomState, Payload: raw})
}

// broadcastRoomState reaches admitted AND waiting participants, so a guest in
// the waiting room learns that the session is being recorded before admission.
func (h *Hub) broadcastRoomState(sessionID string) {
	raw, _ := json.Marshal(h.roomStateFor(sessionID))
	env := &Envelope{Type: TypeRoomState, Payload: raw}
	h.mu.RLock()
	clients := make([]*Client, 0, len(h.sessionGroups[sessionID]))
	for _, c := range h.sessionGroups[sessionID] {
		clients = append(clients, c)
	}
	h.mu.RUnlock()
	for _, c := range clients {
		if s := c.getStatus(); s == "admitted" || s == "pending" {
			c.SendEnvelope(env)
		}
	}
}

// afterAdmission brings a newly admitted or resumed client up to date.
func (h *Hub) afterAdmission(c *Client) {
	h.sendRoomState(c)
	h.sendCaptionState(c)
	h.sendChatHistory(c)
	h.sendProduction(c)
	h.broadcastRoster(c.SessionID)
}
