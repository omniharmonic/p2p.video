package signaling

import (
	"encoding/json"
	"log/slog"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/omniharmonic/studiolink/host/internal/store"
)

// Production extras [SL-P5-1, SL-P5-2]: the teleprompter and branding /
// lower thirds. The permission rules are documented in protocol.go. Nothing
// here touches recording, uploads or post-processing; the script text is
// never logged.

// prompterState is a session's teleprompter. A room's script is loaded from
// and saved to SQLite; an instant meeting's lives only here. Guarded by h.mu.
type prompterState struct {
	text       string
	visibility string
	revision   uint64
	updatedBy  string
	roomScope  string // "room:<id>" when persisted, else ""
	scroll     *PrompterScrollPayload
}

// prompterSees reports whether c may read a script with visibility vis.
func prompterSees(c *Client, vis string) bool {
	if c.getStatus() != "admitted" {
		return false
	}
	switch c.Role {
	case RoleHost:
		return true
	case RoleProducer:
		return vis == PrompterCrew || vis == PrompterEveryone
	case RoleGuest:
		return vis == PrompterEveryone
	}
	return false
}

// prompterEdits reports whether c may edit a script with visibility vis.
func prompterEdits(c *Client, vis string) bool {
	if c.getStatus() != "admitted" {
		return false
	}
	return c.Role == RoleHost || (c.Role == RoleProducer && vis != PrompterHost)
}

// loadPrompter returns the session's teleprompter, loading a room's saved
// script on first use.
func (h *Hub) loadPrompter(sessionID string) *prompterState {
	h.mu.RLock()
	if r := h.rooms[sessionID]; r != nil && r.prompter != nil {
		p := r.prompter
		h.mu.RUnlock()
		return p
	}
	h.mu.RUnlock()
	st := &prompterState{visibility: PrompterHost}
	if h.Store != nil {
		if sess, err := h.Store.GetSession(sessionID); err == nil && sess.RoomID != "" {
			st.roomScope = store.ProductionScope(sess)
			if prod, err := h.Store.GetProduction(st.roomScope); err == nil {
				st.text = prod.Script
				if ValidPrompterVisibility(prod.ScriptVisibility) {
					st.visibility = prod.ScriptVisibility
				}
			} else {
				slog.Warn("Teleprompter script unavailable", "sessionId", sessionID, "err", err)
			}
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.roomLocked(sessionID)
	if r.prompter == nil {
		r.prompter = st
	}
	return r.prompter
}

// prompterFor is the script as c may see it. Caller holds no locks.
func (h *Hub) prompterFor(c *Client, st prompterState) PrompterPayload {
	out := PrompterPayload{Visibility: st.visibility, Revision: st.revision, UpdatedBy: st.updatedBy}
	if !prompterSees(c, st.visibility) {
		out.Hidden = true
		return out
	}
	out.Text = st.text
	if st.scroll != nil {
		sc := *st.scroll
		out.Scroll = &sc
	}
	return out
}

func (h *Hub) prompterSnapshot(sessionID string) prompterState {
	st := h.loadPrompter(sessionID)
	h.mu.RLock()
	defer h.mu.RUnlock()
	cp := *st
	return cp
}

// productionClients is everyone admitted in the session except phones.
func (h *Hub) productionClients(sessionID string) []*Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*Client, 0, len(h.sessionGroups[sessionID]))
	for _, c := range h.sessionGroups[sessionID] {
		if c.Role != RoleSubsource && c.getStatus() == "admitted" {
			out = append(out, c)
		}
	}
	return out
}

func (h *Hub) sendPrompter(c *Client, st prompterState) {
	raw, _ := json.Marshal(h.prompterFor(c, st))
	c.SendEnvelope(&Envelope{Type: TypePrompter, Payload: raw})
}

func (h *Hub) broadcastPrompter(sessionID string) {
	st := h.prompterSnapshot(sessionID)
	for _, c := range h.productionClients(sessionID) {
		h.sendPrompter(c, st)
	}
}

// sendProduction brings a newly admitted client's teleprompter and branding
// up to date. Phones get neither.
func (h *Hub) sendProduction(c *Client) {
	if c.Role == RoleSubsource || c.getStatus() != "admitted" {
		return
	}
	h.sendPrompter(c, h.prompterSnapshot(c.SessionID))
	// In a room, a returning person gets the lower third the room remembers
	// for their display name [SL-P5-4]; everyone then sees it.
	if h.applyRoomLowerThird(c) {
		h.BrandingChanged(c.SessionID)
		return
	}
	if b, ok := h.brandingPayload(c.SessionID); ok {
		raw, _ := json.Marshal(b)
		c.SendEnvelope(&Envelope{Type: TypeBranding, Payload: raw})
	}
}

// normalizeScript validates untrusted script text: valid UTF-8, CRLF → LF,
// NUL removed, at most PrompterMaxBytes.
func normalizeScript(text string) (string, bool) {
	if !utf8.ValidString(text) {
		return "", false
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\x00", "")
	return text, len(text) <= PrompterMaxBytes
}

func (h *Hub) handlePrompterEdit(c *Client, raw json.RawMessage) {
	if c.getStatus() != "admitted" {
		return
	}
	st := h.prompterSnapshot(c.SessionID)
	if !prompterEdits(c, st.visibility) {
		slog.Warn("Rejected teleprompter edit", "participantId", c.ParticipantID, "role", c.Role)
		msg := "Only the host or a producer can edit the script."
		if c.Role == RoleProducer {
			msg = "The host keeps the script private."
		}
		h.sendActionError(c, TypePrompterEdit, ErrCodeForbidden, msg)
		return
	}
	var p PrompterEditPayload
	if json.Unmarshal(raw, &p) != nil {
		h.sendActionError(c, TypePrompterEdit, ErrCodeInvalid, "That script change couldn't be read.")
		return
	}
	if p.Visibility != "" {
		if c.Role != RoleHost {
			h.sendActionError(c, TypePrompterEdit, ErrCodeForbidden, "Only the host chooses who sees the script.")
			return
		}
		if !ValidPrompterVisibility(p.Visibility) {
			h.sendActionError(c, TypePrompterEdit, ErrCodeInvalid, "Unknown script visibility.")
			return
		}
	}
	var text string
	if p.Text != nil {
		var ok bool
		if text, ok = normalizeScript(*p.Text); !ok {
			h.sendActionError(c, TypePrompterEdit, ErrCodeInvalid, "The script is too long (64 KB at most).")
			return
		}
	}
	now := h.clock()
	c.mu.RLock()
	name := c.Name
	c.mu.RUnlock()
	h.mu.Lock()
	r := h.roomLocked(c.SessionID)
	if !r.allowLocked("prompter:"+c.ParticipantID, PrompterEditRateLimit, PrompterEditWindow, now) {
		h.mu.Unlock()
		h.sendActionError(c, TypePrompterEdit, ErrCodeRateLimited, "Saving too often. Wait a moment.")
		return
	}
	ps := r.prompter // loaded by prompterSnapshot above
	if ps == nil {
		h.mu.Unlock() // the meeting ended meanwhile
		return
	}
	if p.Text != nil {
		ps.text = text
	}
	if p.Visibility != "" {
		ps.visibility = p.Visibility
	}
	ps.revision++
	ps.updatedBy = name
	scope, saveText, saveVis, rev := ps.roomScope, ps.text, ps.visibility, ps.revision
	h.mu.Unlock()
	if scope != "" && h.Store != nil {
		if err := h.Store.SetScript(scope, saveText, saveVis); err != nil {
			slog.Error("Saving the teleprompter script failed", "sessionId", c.SessionID, "err", err)
		}
	}
	// Size and revision only: the script itself is never logged.
	slog.Info("Teleprompter updated", "sessionId", c.SessionID, "participantId", c.ParticipantID, "revision", rev, "bytes", len(saveText), "visibility", saveVis)
	h.broadcastPrompter(c.SessionID)
}

func finite(v, lo, hi float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return lo
	}
	return clampFloat(v, lo, hi)
}

func (h *Hub) handlePrompterScroll(c *Client, raw json.RawMessage) {
	if !h.requireHost(c, TypePrompterScroll) {
		return
	}
	var p PrompterScrollPayload
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	p = PrompterScrollPayload{Position: finite(p.Position, 0, 1), Playing: p.Playing, Speed: finite(p.Speed, 0, PrompterMaxSpeed), Shared: p.Shared}
	st := h.loadPrompter(c.SessionID)
	now := h.clock()
	h.mu.Lock()
	if !h.roomLocked(c.SessionID).allowLocked("prompter_scroll:"+c.ParticipantID, PrompterScrollRateLimit, PrompterScrollWindow, now) {
		h.mu.Unlock()
		return // a dropped scroll update is harmless; the next one catches up
	}
	if p.Shared {
		sc := p
		st.scroll = &sc
	} else {
		st.scroll = nil
	}
	vis := st.visibility
	h.mu.Unlock()
	out, _ := json.Marshal(p)
	env := &Envelope{Type: TypePrompterScroll, Payload: out}
	for _, other := range h.productionClients(c.SessionID) {
		if other != c && prompterSees(other, vis) {
			other.SendEnvelope(env)
		}
	}
}

// brandingPayload is the session's brand and lower thirds for every
// admitted participant.
func (h *Hub) brandingPayload(sessionID string) (BrandingPayload, bool) {
	out := BrandingPayload{Accent: DefaultBrandAccent, LowerThirdsMode: LowerThirdsSpeaker, LowerThirds: []LowerThirdPayload{}}
	if h.Store == nil {
		return out, false
	}
	sess, err := h.Store.GetSession(sessionID)
	if err != nil {
		return out, false
	}
	prod, err := h.Store.GetProduction(store.ProductionScope(sess))
	if err != nil {
		slog.Warn("Branding unavailable", "sessionId", sessionID, "err", err)
		return out, false
	}
	b := prod.Brand
	out.ShowName, out.TileLabels = b.ShowName, b.TileLabels
	if ValidAccent(b.Accent) {
		out.Accent = strings.ToLower(b.Accent)
	}
	if ValidLowerThirdsMode(b.LowerThirds) {
		out.LowerThirdsMode = b.LowerThirds
	}
	if prod.LogoSHA != "" {
		out.LogoVersion = prod.LogoSHA[:min(16, len(prod.LogoSHA))]
	}
	out.RememberTitles = sess.RoomID != ""
	lts, err := h.Store.ListLowerThirds(sessionID)
	if err != nil {
		slog.Warn("Lower thirds unavailable", "sessionId", sessionID, "err", err)
	}
	for _, lt := range lts {
		out.LowerThirds = append(out.LowerThirds, LowerThirdPayload{ParticipantID: lt.ParticipantID, Name: lt.Name, Title: lt.Title})
	}
	return out, true
}

// BrandingChanged sends the session's brand to everyone admitted (the HTTP
// branding routes call it after a change).
func (h *Hub) BrandingChanged(sessionID string) {
	b, ok := h.brandingPayload(sessionID)
	if !ok {
		return
	}
	raw, _ := json.Marshal(b)
	env := &Envelope{Type: TypeBranding, Payload: raw}
	for _, c := range h.productionClients(sessionID) {
		c.SendEnvelope(env)
	}
}

func (h *Hub) handleLowerThird(c *Client, raw json.RawMessage) {
	if !h.requireAssist(c, TypeLowerThird) {
		return
	}
	var p LowerThirdPayload
	if json.Unmarshal(raw, &p) != nil || p.ParticipantID == "" {
		h.sendActionError(c, TypeLowerThird, ErrCodeInvalid, "That lower third couldn't be read.")
		return
	}
	now := h.clock()
	h.mu.Lock()
	ok := h.roomLocked(c.SessionID).allowLocked("lower_third:"+c.ParticipantID, ModerationRateLimit, ModerationWindow, now)
	h.mu.Unlock()
	if !ok {
		h.sendActionError(c, TypeLowerThird, ErrCodeRateLimited, "Slow down.")
		return
	}
	part, err := h.Store.GetParticipant(p.ParticipantID)
	if err != nil || part == nil || part.SessionID != c.SessionID || part.Role == RoleSubsource {
		h.sendActionError(c, TypeLowerThird, ErrCodeNotFound, "That participant isn't in this meeting.")
		return
	}
	lt := store.LowerThird{
		ParticipantID: part.ID,
		Name:          SanitizeDisplayText(p.Name, LowerThirdNameMaxRunes),
		Title:         SanitizeDisplayText(p.Title, LowerThirdTitleMaxRunes),
	}
	if lt.Name == part.Name {
		lt.Name = "" // the default: follows the participant's display name
	}
	if err := h.Store.SetLowerThird(c.SessionID, lt); err != nil {
		slog.Error("Saving a lower third failed", "sessionId", c.SessionID, "err", err)
		h.sendActionError(c, TypeLowerThird, ErrCodeInvalid, "The lower third couldn't be saved.")
		return
	}
	// A room remembers it for the person's next episode (and forgets it
	// when both fields are cleared) [SL-P5-4].
	if sess, err := h.Store.GetSession(c.SessionID); err == nil && sess.RoomID != "" {
		if err := h.Store.RememberRoomLowerThird(sess.RoomID, part.Name, lt.Name, lt.Title); err != nil {
			slog.Warn("Remembering a lower third for the room failed", "sessionId", c.SessionID, "err", err)
		}
	}
	slog.Info("Lower third updated", "sessionId", c.SessionID, "participantId", part.ID, "by", c.ParticipantID)
	h.BrandingChanged(c.SessionID)
}

// applyRoomLowerThird copies the lower third c's room remembers for c's
// display name into this meeting, unless c already has one. It reports
// whether the meeting's lower thirds changed.
func (h *Hub) applyRoomLowerThird(c *Client) bool {
	if h.Store == nil || c.Role == RoleSubsource {
		return false
	}
	sess, err := h.Store.GetSession(c.SessionID)
	if err != nil || sess.RoomID == "" {
		return false
	}
	part, err := h.Store.GetParticipant(c.ParticipantID)
	if err != nil || part == nil || part.SessionID != c.SessionID {
		return false
	}
	applied, err := h.Store.ApplyRoomLowerThird(sess.RoomID, sess.ID, part.ID, part.Name)
	if err != nil {
		slog.Warn("Applying a remembered lower third failed", "sessionId", c.SessionID, "err", err)
		return false
	}
	if applied {
		slog.Info("Remembered lower third applied", "sessionId", c.SessionID, "participantId", part.ID)
	}
	return applied
}
