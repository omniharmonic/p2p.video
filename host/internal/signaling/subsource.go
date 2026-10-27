package signaling

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Phone as an extra camera (PRD F3) [SL-P1-9].
//
// An admitted participant asks for a short-lived, single-use capability for
// a phone. The phone opens /phone.html with it, connects with role
// "subsource" and is admitted at once as an extra source of that participant
// (no second admission prompt: the capability was minted for an admitted
// participant, and the phone has no other rights). The phone's own signed
// participant grant (its upload token) lets it reconnect after a dropped
// socket; the capability itself works once.

// RoleSubsource is a phone (or other device) recording for a parent participant.
const RoleSubsource = "subsource"

// SubsourceCapTTL bounds how long a phone link works if it is never opened.
const SubsourceCapTTL = 10 * time.Minute

// SubsourceNameSuffix is appended to the parent's display name.
const SubsourceNameSuffix = " · Phone"

// MaxSubsourcesPerParticipant bounds the phones one participant can attach.
const MaxSubsourcesPerParticipant = 2

var (
	ErrSubsourceCapInvalid = errors.New("phone link is invalid or expired")
	ErrSubsourceCapUsed    = errors.New("phone link was already used")
	ErrSubsourceParent     = errors.New("the participant this phone belongs to is not in the call")
	ErrSubsourceLimit      = errors.New("too many phones for this participant")
)

// subsourceCapDomain separates the capability MAC from other tokens signed
// with the same secret, so no other token can be replayed as a capability.
const subsourceCapDomain = "studiolink-subsource-cap|"

// SubsourceClaims are the verified fields of a phone capability.
type SubsourceClaims struct {
	SessionID string
	ParentID  string
	Nonce     string
	ExpiresAt time.Time
}

// MintSubsourceCap signs a capability for a phone of parentID in sessionID.
// Format: base64("phone:session:parent:nonce:exp").base64(hmac(domain+body)).
// It has five fields, so it never verifies as an upload or host token.
func MintSubsourceCap(sessionID, parentID string, ttl time.Duration, secretKey []byte, now time.Time) (string, time.Time, error) {
	if sessionID == "" || parentID == "" || strings.Contains(sessionID, ":") || strings.Contains(parentID, ":") {
		return "", time.Time{}, ErrSubsourceCapInvalid
	}
	n := make([]byte, 16)
	if _, err := rand.Read(n); err != nil {
		return "", time.Time{}, fmt.Errorf("phone link entropy: %w", err)
	}
	exp := now.Add(ttl)
	body := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("phone:%s:%s:%s:%d", sessionID, parentID, hex.EncodeToString(n), exp.Unix())))
	return body + "." + base64.RawURLEncoding.EncodeToString(subsourceMAC(body, secretKey)), exp, nil
}

func subsourceMAC(body string, secretKey []byte) []byte {
	mac := hmac.New(sha256.New, secretKey)
	mac.Write([]byte(subsourceCapDomain + body))
	return mac.Sum(nil)
}

// VerifySubsourceCap checks the signature and expiry (not single use).
func VerifySubsourceCap(token string, secretKey []byte, now time.Time) (*SubsourceClaims, error) {
	body, sig, ok := strings.Cut(token, ".")
	if !ok || body == "" {
		return nil, ErrSubsourceCapInvalid
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, subsourceMAC(body, secretKey)) {
		return nil, ErrSubsourceCapInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, ErrSubsourceCapInvalid
	}
	f := strings.Split(string(raw), ":")
	if len(f) != 5 || f[0] != "phone" || f[1] == "" || f[2] == "" || len(f[3]) != 32 {
		return nil, ErrSubsourceCapInvalid
	}
	exp, err := strconv.ParseInt(f[4], 10, 64)
	if err != nil || now.Unix() > exp {
		return nil, ErrSubsourceCapInvalid
	}
	return &SubsourceClaims{SessionID: f[1], ParentID: f[2], Nonce: f[3], ExpiresAt: time.Unix(exp, 0)}, nil
}

// admittedParentLocked returns the parent's admitted client when it may own a
// phone (host or guest, not itself a phone). Caller holds h.mu.
func (h *Hub) admittedParentLocked(sessionID, parentID string) *Client {
	p := h.sessionGroups[sessionID][parentID]
	if p == nil || p.getStatus() != "admitted" || (p.Role != "host" && p.Role != "guest") {
		return nil
	}
	return p
}

func (h *Hub) subsourceCountLocked(sessionID, parentID string) int {
	n := 0
	for _, c := range h.sessionGroups[sessionID] {
		if c.Role == RoleSubsource && c.ParentParticipantID == parentID && c.getStatus() == "admitted" {
			n++
		}
	}
	return n
}

// MintSubsourceCapability mints a phone link for the participant that holds
// grant (its signed participant/upload token). Only an admitted host or guest
// that is connected to sessionID right now gets one.
func (h *Hub) MintSubsourceCapability(sessionID, grant string) (string, time.Time, error) {
	claims, err := VerifyUploadToken(grant, h.Config.SecretKey)
	if err != nil || claims.SessionID != sessionID {
		return "", time.Time{}, ErrSubsourceParent
	}
	h.mu.RLock()
	parent := h.admittedParentLocked(sessionID, claims.ParticipantID)
	count := h.subsourceCountLocked(sessionID, claims.ParticipantID)
	h.mu.RUnlock()
	if parent == nil {
		return "", time.Time{}, ErrSubsourceParent
	}
	if count >= MaxSubsourcesPerParticipant {
		return "", time.Time{}, ErrSubsourceLimit
	}
	return MintSubsourceCap(sessionID, claims.ParticipantID, SubsourceCapTTL, h.Config.SecretKey, h.clock())
}

// ConsumeSubsourceCap spends a phone capability for sessionID and returns the
// parent participant. A capability works once, only while unexpired, and only
// while its parent is still admitted.
func (h *Hub) ConsumeSubsourceCap(sessionID, token string) (string, error) {
	now := h.clock()
	claims, err := VerifySubsourceCap(token, h.Config.SecretKey, now)
	if err != nil || claims.SessionID != sessionID {
		return "", ErrSubsourceCapInvalid
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.spentCaps == nil {
		h.spentCaps = make(map[string]time.Time)
	}
	for nonce, exp := range h.spentCaps {
		if now.After(exp) {
			delete(h.spentCaps, nonce)
		}
	}
	if _, used := h.spentCaps[claims.Nonce]; used {
		return "", ErrSubsourceCapUsed
	}
	if h.admittedParentLocked(sessionID, claims.ParentID) == nil {
		return "", ErrSubsourceParent
	}
	if h.subsourceCountLocked(sessionID, claims.ParentID) >= MaxSubsourcesPerParticipant {
		return "", ErrSubsourceLimit
	}
	h.spentCaps[claims.Nonce] = claims.ExpiresAt
	return claims.ParentID, nil
}

// SubsourceResume authorizes a phone's reconnecting socket by its own signed
// participant grant: the grant must belong to a phone of sessionID that was
// never removed. It returns the parent participant.
func (h *Hub) SubsourceResume(sessionID, grant string) (string, bool) {
	claims, err := VerifyUploadToken(grant, h.Config.SecretKey)
	if err != nil || claims.SessionID != sessionID {
		return "", false
	}
	part, err := h.Store.GetParticipant(claims.ParticipantID)
	if err != nil || part == nil || part.SessionID != sessionID || part.Role != RoleSubsource || part.Status == "denied" || part.ParentParticipantID == "" {
		return "", false
	}
	return part.ParentParticipantID, true
}

// subsourceAllowed lists what a phone may send: media, clock, its own
// recorder/connection reports. Chat, hands, reactions, screen share and every
// host action are refused server-side.
var subsourceAllowed = map[string]bool{
	TypeHello: true, TypeClockPing: true, TypeSdpOffer: true, TypeSdpAnswer: true, TypeIceCandidate: true,
	TypeRecorderStatus: true, TypeParticipantState: true, TypeNetStats: true, TypePreflight: true,
}

// subsourceName is the phone's server-assigned display name.
func (h *Hub) subsourceName(sessionID, parentID string) string {
	h.mu.RLock()
	parent := h.sessionGroups[sessionID][parentID]
	h.mu.RUnlock()
	name := ""
	if parent != nil {
		parent.mu.RLock()
		name = parent.Name
		parent.mu.RUnlock()
	}
	if name == "" {
		if part, err := h.Store.GetParticipant(parentID); err == nil && part != nil {
			name = part.Name
		}
	}
	if name == "" {
		name = "Participant"
	}
	return clampName(name) + SubsourceNameSuffix
}

// publishOnly is implemented by the SFU: a phone sends its camera but never
// receives the call (saves the phone's battery and downlink).
type publishOnly interface {
	SetPublishOnly(sessionID, participantID string, on bool)
}

func (h *Hub) markPublishOnly(c *Client) {
	if po, ok := h.WebRTC.(publishOnly); ok && c.Role == RoleSubsource {
		po.SetPublishOnly(c.SessionID, c.ParticipantID, true)
	}
}

// subsourcesOfLocked lists a participant's phones. Caller holds h.mu.
func (h *Hub) subsourcesOfLocked(sessionID, parentID string) []string {
	var ids []string
	for id, c := range h.sessionGroups[sessionID] {
		if c.Role == RoleSubsource && c.ParentParticipantID == parentID {
			ids = append(ids, id)
		}
	}
	return ids
}
