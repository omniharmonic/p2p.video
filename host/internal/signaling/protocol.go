package signaling

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Message types
const (
	TypeHello          = "hello"
	TypeAdmission      = "admission"
	TypeJoinRequest    = "join_request"
	TypeAdmit          = "admit"
	TypeDeny           = "deny"
	TypeSdpOffer       = "sdp_offer"
	TypeSdpAnswer      = "sdp_answer"
	TypeIceCandidate   = "ice_candidate"
	TypeSourceAdd      = "source_add"
	TypeSourceRemove   = "source_remove"
	TypeClockPing      = "clock_ping"
	TypeClockPong      = "clock_pong"
	TypeRecordStart    = "record_start"
	TypeRecordStop     = "record_stop"
	TypeChirp          = "chirp"
	TypeRecorderStatus = "recorder_status"
	TypeDashboard      = "dashboard"
	TypeSessionEnd     = "session_end"
	// TypeParticipantState relays call-path mic/camera state to the session.
	TypeParticipantState = "participant_state"

	// In-call collaboration (ROADMAP P1). None of these touch recording or
	// upload state; host-only actions are enforced by role in the hub.
	TypeChat        = "chat"         // C→S {text}; S→C ChatMessage
	TypeChatHistory = "chat_history" // S→C on admission/resume
	TypeHand        = "hand"         // C→S {raised}; host may lower another's hand
	TypeReaction    = "reaction"     // C→S {emoji}; S→C stamped with sender
	TypeRoster      = "roster"       // S→C admitted participants + room lock
	TypeRoomState   = "room_state"   // S→C (incl. waiting room): lock + recording
	TypeMuteRequest = "mute_request" // H→S {participantId}; S→C target
	TypeRemove      = "remove"       // H→S {participantId}; revokes like deny
	TypeRoomLock    = "room_lock"    // H→S {locked}
	TypeActionError = "action_error" // S→C a rejected collaboration action

	// Screen share (ROADMAP P1). The sharer declares its screen tracks with
	// source_add before publishing them on its existing PeerConnection, and
	// withdraws them with source_remove. The host may stop anyone's share.
	TypeScreenStop = "screen_stop" // H→S {participantId}; S→C target {participantId, by}
)

// Screen share. One share per session at a time. The SFU forwards a declared
// screen track with stream ID "<participantId>" + ScreenStreamSuffix, so
// receivers can tell it from the camera (stream ID "<participantId>").
const (
	SourceKindScreen   = "screen"
	ScreenStreamSuffix = "_screen"
	MaxScreenTrackIDs  = 2 // video + optional system/tab audio
	MaxTrackIDRunes    = 128
)

// Collaboration limits. Chat text is untrusted and rendered as text only.
const (
	ChatMaxRunes        = 2000
	ChatHistoryLimit    = 200
	ChatRateLimit       = 10
	ChatRateWindow      = 10 * time.Second
	ReactionRateLimit   = 5
	ReactionRateWindow  = 5 * time.Second
	ModerationRateLimit = 30
	ModerationWindow    = 10 * time.Second
)

// AllowedReactions is the fixed reaction allow-list.
var AllowedReactions = []string{"👍", "❤️", "😂", "😮", "👏", "🎉"}

// Admission denial reasons (AdmissionPayload.Reason).
const (
	ReasonDeclined    = "declined"
	ReasonRemoved     = "removed"
	ReasonRoomLocked  = "room_locked"
	ReasonUnavailable = "unavailable"
	// ReasonBusy: too many people are already waiting, or join attempts are
	// arriving too fast; try again shortly [SL-SEC-1].
	ReasonBusy = "busy"
)

// Action error codes (ActionErrorPayload.Code).
const (
	ErrCodeForbidden   = "forbidden"
	ErrCodeRateLimited = "rate_limited"
	ErrCodeInvalid     = "invalid"
	ErrCodeNotFound    = "not_found"
	ErrCodeConflict    = "conflict" // e.g. someone else is already presenting
)

// Envelope is the base frame for all signaling messages.
type Envelope struct {
	Type          string          `json:"t"`
	ID            string          `json:"id,omitempty"`
	ParticipantID string          `json:"participantId,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
}

// HelloPayload is sent by the client upon connecting.
type HelloPayload struct {
	ResumeToken         string          `json:"resumeToken,omitempty"`
	Name                string          `json:"name"`
	Role                string          `json:"role"` // "host", "guest", "subsource", "producer"; the server decides from the credential
	ParentParticipantID string          `json:"parentParticipantId,omitempty"`
	Capabilities        map[string]bool `json:"capabilities,omitempty"`
	Devices             []DeviceHint    `json:"devices,omitempty"`
}

type DeviceHint struct {
	DeviceID string `json:"deviceId"`
	Kind     string `json:"kind"` // "audioinput", "videoinput"
	Label    string `json:"label"`
}

// AdmissionPayload informs the client of admission result.
type AdmissionPayload struct {
	ActiveTakeID  string      `json:"activeTakeId,omitempty"`
	Role          string      `json:"role,omitempty"`
	Status        string      `json:"status"`           // "admitted", "pending", "denied"
	Reason        string      `json:"reason,omitempty"` // denied only: a Reason* code
	ParticipantID string      `json:"participantId"`
	UploadToken   string      `json:"uploadToken,omitempty"`
	IceServers    []ICEServer `json:"iceServers,omitempty"`
	// Phones (role "subsource") only [SL-P1-9]: who the phone records for, and
	// its server-assigned display name ("<Name> · Phone").
	ParentParticipantID string `json:"parentParticipantId,omitempty"`
	Name                string `json:"name,omitempty"`
}

type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// SdpPayload carries WebRTC offer/answer SDP. A client offer with
// Renegotiate set updates its existing PeerConnection (to add or remove a
// screen share); without it, an offer creates or replaces the participant's
// peer (initial join and media recovery).
type SdpPayload struct {
	SDP         string `json:"sdp"`
	Type        string `json:"type"` // "offer", "answer"
	Renegotiate bool   `json:"renegotiate,omitempty"`
}

// IceCandidatePayload carries ICE candidate JSON.
type IceCandidatePayload struct {
	Candidate     string  `json:"candidate"`
	SDPMid        *string `json:"sdpMid,omitempty"`
	SDPMLineIndex *uint16 `json:"sdpMLineIndex,omitempty"`
}

// SourceAddPayload registers a media track source. Only kind "screen" is
// accepted today: TrackIDs are the MediaStreamTrack IDs (SDP msid track IDs)
// the client is about to publish for the share.
type SourceAddPayload struct {
	SourceID string   `json:"sourceId"`
	Kind     string   `json:"kind"` // "camera", "mic", "screen"
	Label    string   `json:"label"`
	TrackIDs []string `json:"trackIds,omitempty"`
}

// SourceRemovePayload withdraws a source the client declared with source_add.
type SourceRemovePayload struct {
	SourceID string `json:"sourceId"`
	Kind     string `json:"kind"`
}

// ScreenStopPayload: the host asks the hub to stop ParticipantID's screen
// share. The hub unpublishes it and forwards {participantId, by} to the sharer.
type ScreenStopPayload struct {
	ParticipantID string `json:"participantId"`
	By            string `json:"by,omitempty"`
}

// ClockPingPayload initiates an NTP-style clock sync roundtrip.
type ClockPingPayload struct {
	T0 int64 `json:"t0"` // client local monotonic / perfTime in microseconds
}

// ClockPongPayload responds with server monotonic timestamps.
type ClockPongPayload struct {
	T0 int64 `json:"t0"` // client t0 echoed
	T1 int64 `json:"t1"` // server arrival in monotonic microseconds
	T2 int64 `json:"t2"` // server departure in monotonic microseconds
}

// RecordStartPayload commands clients to arm and begin recording at startAtHostUs.
type RecordStartPayload struct {
	TakeID        string `json:"takeId"`
	TakeNumber    int    `json:"takeNumber"`
	StartAtHostUs int64  `json:"startAtHostUs"`
	Preset        string `json:"preset"`
}

// RecordStopPayload commands clients to stop recording at stopAtHostUs.
type RecordStopPayload struct {
	TakeID       string `json:"takeId"`
	StopAtHostUs int64  `json:"stopAtHostUs"`
}

// ChirpPayload triggers a synchronous alignment sync chirp tone on all clients.
type ChirpPayload struct {
	AtHostUs  int64 `json:"atHostUs"`
	FreqStart int   `json:"freqStart"`
	FreqEnd   int   `json:"freqEnd"`
	DurMs     int   `json:"durMs"`
}

// RecorderStatusPayload conveys live chunk upload metrics from clients.
type RecorderStatusPayload struct {
	TrackID       string `json:"trackId"`
	State         string `json:"state"` // "recording", "uploading", "done", "error"
	BufferedBytes int64  `json:"bufferedBytes"`
	UploadedBytes int64  `json:"uploadedBytes"`
	DroppedFrames int    `json:"droppedFrames"`
	LastSeq       int    `json:"lastSeq"`
}

// Recorder states a client may report; anything else is dropped.
var recorderStates = map[string]bool{"recording": true, "uploading": true, "done": true, "error": true}

// SanitizeRecorderStatus whitelists State and clamps counters. The payload is
// relayed to the host dashboard, so a guest must not be able to put arbitrary
// strings or values there.
func SanitizeRecorderStatus(p RecorderStatusPayload) (RecorderStatusPayload, bool) {
	if !recorderStates[p.State] || p.TrackID == "" || len(p.TrackID) > 128 {
		return RecorderStatusPayload{}, false
	}
	p.BufferedBytes = clampInt64(p.BufferedBytes, 0, 1<<50)
	p.UploadedBytes = clampInt64(p.UploadedBytes, 0, 1<<50)
	p.DroppedFrames = clampInt(p.DroppedFrames, 0, 1<<30)
	p.LastSeq = clampInt(p.LastSeq, -1, 1<<30)
	return p, true
}

// DashboardParticipantState mirrors participant telemetry for host monitoring.
type DashboardParticipantState struct {
	ParticipantID string                           `json:"participantId"`
	Name          string                           `json:"name"`
	Role          string                           `json:"role"`
	Status        string                           `json:"status"`
	ClockOffsetUs int64                            `json:"clockOffsetUs"`
	RTTUs         int64                            `json:"rttUs"`
	Tracks        map[string]RecorderStatusPayload `json:"tracks"`
	// Preflight is the participant's latest pre-flight checklist [SL-P0-5].
	Preflight *PreflightPayload `json:"preflight,omitempty"`
	// NetStats is the participant's latest connection quality and upload
	// backlog report [SL-P0-8].
	NetStats *NetStatsPayload `json:"netStats,omitempty"`
}

// ParticipantStatePayload carries call-path mic/camera state. Clients send
// only MicMuted/CamOff; the hub overwrites ParticipantID and Name from the
// authenticated connection before relaying to admitted session members.
type ParticipantStatePayload struct {
	ParticipantID string `json:"participantId,omitempty"`
	Name          string `json:"name,omitempty"`
	MicMuted      bool   `json:"micMuted"`
	CamOff        bool   `json:"camOff"`
}

// ChatSendPayload is what a client sends; identity and time are server-assigned.
type ChatSendPayload struct {
	Text string `json:"text"`
}

// ChatMessage is a server-stamped chat line relayed to admitted participants.
type ChatMessage struct {
	ID            string `json:"id"`
	ParticipantID string `json:"participantId"`
	Name          string `json:"name"`
	Role          string `json:"role"`
	Text          string `json:"text"`
	TS            int64  `json:"ts"` // host wall clock, Unix milliseconds
}

// ChatHistoryPayload replays the session's recent chat to a (re)admitted client.
type ChatHistoryPayload struct {
	Messages []ChatMessage `json:"messages"`
}

// HandPayload raises or lowers a hand. ParticipantID names another participant
// only when the host lowers their hand; otherwise it is ignored.
type HandPayload struct {
	ParticipantID string `json:"participantId,omitempty"`
	Raised        bool   `json:"raised"`
}

// ReactionPayload is an ephemeral emoji reaction from the allow-list. Clients
// send only Emoji; the hub stamps the sender's identity.
type ReactionPayload struct {
	ParticipantID string `json:"participantId,omitempty"`
	Name          string `json:"name,omitempty"`
	Emoji         string `json:"emoji"`
}

// RosterEntry is one admitted participant as everyone in the call sees it.
type RosterEntry struct {
	ParticipantID string `json:"participantId"`
	Name          string `json:"name"`
	Role          string `json:"role"`
	MicMuted      bool   `json:"micMuted"`
	CamOff        bool   `json:"camOff"`
	HandRaised    bool   `json:"handRaised"`
	HandRaisedAt  int64  `json:"handRaisedAt,omitempty"` // Unix ms; orders the hand queue
	ScreenSharing bool   `json:"screenSharing,omitempty"`
	// ParentParticipantID marks a phone (role "subsource") of that participant.
	ParentParticipantID string `json:"parentParticipantId,omitempty"`
}

// RosterPayload is the full, server-authoritative list of admitted participants.
type RosterPayload struct {
	Participants []RosterEntry `json:"participants"`
	Locked       bool          `json:"locked"`
}

// RoomStatePayload is also sent to the waiting room, so it carries no
// participant data: only whether the room is locked and being recorded.
type RoomStatePayload struct {
	Locked     bool `json:"locked"`
	Recording  bool `json:"recording"`
	TakeNumber int  `json:"takeNumber,omitempty"`
	// RecordingMicMode is the session's recording mic default (auto, raw or
	// clean) [SL-P1-10]; re-sent when the host changes it.
	RecordingMicMode string `json:"recordingMicMode,omitempty"`
}

// SessionEndPayload (S→C, including the waiting room) says the meeting is
// over: the session no longer accepts connections. Recording uploads already
// granted continue over HTTP. A persistent room can open a new meeting later
// on the same guest link [SL-P1-5].
type SessionEndPayload struct {
	Reason string `json:"reason"` // a SessionEnd* code
}

// Session end reasons (SessionEndPayload.Reason).
const (
	SessionEndByHost = "ended_by_host"
)

// MuteRequestPayload asks a participant to mute their call microphone.
type MuteRequestPayload struct {
	ParticipantID string `json:"participantId"`
	By            string `json:"by,omitempty"`
}

// RemovePayload asks the hub to remove (and revoke) an admitted participant.
type RemovePayload struct {
	ParticipantID string `json:"participantId"`
}

// RoomLockPayload locks or unlocks the room to new join requests.
type RoomLockPayload struct {
	Locked bool `json:"locked"`
}

// ActionErrorPayload reports a rejected collaboration action to its sender.
type ActionErrorPayload struct {
	Action  string `json:"action"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// DashboardPayload aggregates current session metrics.
type DashboardPayload struct {
	SessionID    string                      `json:"sessionId"`
	Title        string                      `json:"title"`
	CurrentTake  int                         `json:"currentTake"`
	IsRecording  bool                        `json:"isRecording"`
	Participants []DashboardParticipantState `json:"participants"`
	// PreflightRefreshAtMs is when the host last asked for fresh pre-flight
	// reports (hub clock, Unix ms); reports received at or after it are fresh.
	PreflightRefreshAtMs int64 `json:"preflightRefreshAtMs,omitempty"`
}

// ParticipantClaims represents verified auth claims for uploads.
type ParticipantClaims struct {
	ParticipantID string
	SessionID     string
	ExpiresAt     int64
}

// MintUploadToken signs an auth token for upload endpoints.
// Format: base64(participantId:sessionId:expiresAt).base64(hmac)
func MintUploadToken(participantID, sessionID string, ttl time.Duration, secretKey []byte) string {
	exp := time.Now().Add(ttl).Unix()
	body := fmt.Sprintf("%s:%s:%d", participantID, sessionID, exp)
	bodyB64 := base64.RawURLEncoding.EncodeToString([]byte(body))

	mac := hmac.New(sha256.New, secretKey)
	mac.Write([]byte(bodyB64))
	sigB64 := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return fmt.Sprintf("%s.%s", bodyB64, sigB64)
}

// VerifyUploadToken verifies an auth token and extracts claims.
func VerifyUploadToken(tokenStr string, secretKey []byte) (*ParticipantClaims, error) {
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 2 {
		return nil, errors.New("invalid token format")
	}

	bodyB64, sigB64 := parts[0], parts[1]

	mac := hmac.New(sha256.New, secretKey)
	mac.Write([]byte(bodyB64))
	expectedSig := mac.Sum(nil)

	actualSig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil || !hmac.Equal(actualSig, expectedSig) {
		return nil, errors.New("invalid token signature")
	}

	bodyBytes, err := base64.RawURLEncoding.DecodeString(bodyB64)
	if err != nil {
		return nil, errors.New("corrupted token body")
	}

	fields := strings.Split(string(bodyBytes), ":")
	if len(fields) != 3 || fields[0] == "host" || fields[0] == "" || fields[1] == "" {
		return nil, errors.New("invalid token fields")
	}

	exp, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return nil, errors.New("invalid expiration")
	}

	if time.Now().Unix() > exp {
		return nil, errors.New("token expired")
	}

	return &ParticipantClaims{
		ParticipantID: fields[0],
		SessionID:     fields[1],
		ExpiresAt:     exp,
	}, nil
}

// AdmitPayload allows the host to admit a pending participant.
type AdmitPayload struct {
	ParticipantID string `json:"participantId"`
}

// DenyPayload allows the host to deny or kick a participant.
type DenyPayload struct {
	ParticipantID string `json:"participantId"`
	Reason        string `json:"reason,omitempty"`
}

// MintHostToken generates a cryptographically signed host token scoped to a session.
// Format: base64(host:sessionId:expiresAt).base64(hmac)
func MintHostToken(sessionID string, ttl time.Duration, secretKey []byte) string {
	exp := time.Now().Add(ttl).Unix()
	body := fmt.Sprintf("host:%s:%d", sessionID, exp)
	bodyB64 := base64.RawURLEncoding.EncodeToString([]byte(body))

	mac := hmac.New(sha256.New, secretKey)
	mac.Write([]byte(bodyB64))
	sigB64 := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return fmt.Sprintf("%s.%s", bodyB64, sigB64)
}

// VerifyHostToken checks if a token is a valid host token for the specified session.
func VerifyHostToken(tokenStr, sessionID string, secretKey []byte) bool {
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 2 {
		return false
	}

	bodyB64, sigB64 := parts[0], parts[1]

	mac := hmac.New(sha256.New, secretKey)
	mac.Write([]byte(bodyB64))
	expectedSig := mac.Sum(nil)

	actualSig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil || !hmac.Equal(actualSig, expectedSig) {
		return false
	}

	bodyBytes, err := base64.RawURLEncoding.DecodeString(bodyB64)
	if err != nil {
		return false
	}

	fields := strings.Split(string(bodyBytes), ":")
	if len(fields) != 3 {
		return false
	}

	if fields[0] != "host" || fields[1] != sessionID {
		return false
	}

	exp, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}

	return true
}

// ---- Pre-flight "ready to record" [SL-P0-5] ----
// Kept in its own block so concurrent protocol additions merge cleanly.

// TypePreflight carries a participant's pre-flight checklist. Clients send it
// while pending or admitted; the hub stamps identity, sanitizes it, keeps the
// latest per connection and includes it in the host dashboard.
const TypePreflight = "preflight"

// Pre-flight item statuses. "info" is shown but never counts toward Overall.
const (
	PreflightOK   = "ok"
	PreflightWarn = "warn"
	PreflightFail = "fail"
	PreflightInfo = "info"
)

// Limits applied by SanitizePreflight so a client cannot bloat the dashboard.
const (
	PreflightMaxItems     = 16
	PreflightMaxIDLen     = 32
	PreflightMaxLabelLen  = 48
	PreflightMaxDetailLen = 200
)

// PreflightItem is one check: storage, opfs, persist, camera, mic,
// resolution, headphones, path or power.
type PreflightItem struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// PreflightPayload is a participant's checklist. ParticipantID, Name,
// Overall and ReceivedAtMs are set by the hub; Overall is the worst of
// ok/warn/fail.
//
// Refresh: a host sends it (with its own items) right before a take; the hub
// relays {refresh:true} to the session's admitted participants, which re-run
// their checks and report, so the host confirms against a fresh snapshot.
// ReceivedAtMs (hub clock, Unix ms) lets the host tell fresh reports apart.
type PreflightPayload struct {
	ParticipantID string          `json:"participantId,omitempty"`
	Name          string          `json:"name,omitempty"`
	Overall       string          `json:"overall,omitempty"`
	Items         []PreflightItem `json:"items"`
	Refresh       bool            `json:"refresh,omitempty"`
	ReceivedAtMs  int64           `json:"receivedAtMs,omitempty"`
	// RecordingMic is the participant's recording microphone mode as opened
	// [SL-P1-10]; the host shows it per participant.
	RecordingMic *RecordingMicState `json:"recordingMic,omitempty"`
}

// SanitizePreflight drops items with unknown statuses or empty IDs, truncates
// strings, caps the item count and recomputes Overall. Identity fields are
// cleared for the caller to stamp from the authenticated connection.
func SanitizePreflight(p PreflightPayload) PreflightPayload {
	rank := map[string]int{PreflightInfo: 0, PreflightOK: 0, PreflightWarn: 1, PreflightFail: 2}
	out := PreflightPayload{Items: make([]PreflightItem, 0, min(len(p.Items), PreflightMaxItems))}
	worst := 0
	for _, it := range p.Items {
		if len(out.Items) == PreflightMaxItems {
			break
		}
		r, ok := rank[it.Status]
		if !ok || it.ID == "" {
			continue
		}
		out.Items = append(out.Items, PreflightItem{
			ID:     truncateRunes(it.ID, PreflightMaxIDLen),
			Label:  truncateRunes(it.Label, PreflightMaxLabelLen),
			Status: it.Status,
			Detail: truncateRunes(it.Detail, PreflightMaxDetailLen),
		})
		worst = max(worst, r)
	}
	out.Overall = [...]string{PreflightOK, PreflightWarn, PreflightFail}[worst]
	out.RecordingMic = SanitizeRecordingMic(p.RecordingMic)
	return out
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ---- Network quality and upload backlog [SL-P0-8] ----
// Kept in its own block so concurrent protocol additions merge cleanly.

// TypeNetStats carries a participant's call connection quality and upload
// backlog, computed by the client from RTCPeerConnection.getStats() every
// NetStatsIntervalMs. The hub sanitizes it, stamps identity, keeps the latest
// per connection for the host dashboard and relays it to the admitted members
// of the session, who show it as signal bars on tiles and in the people panel.
const TypeNetStats = "net_stats"

// NetStatsIntervalMs is the client reporting period; the hub drops reports
// that arrive faster than NetStatsMinIntervalMs from one connection.
const (
	NetStatsIntervalMs    = 2000
	NetStatsMinIntervalMs = 500
)

// Connection quality levels (NetStatsPayload.Quality).
const (
	NetQualityGood    = "good"
	NetQualityFair    = "fair"
	NetQualityPoor    = "poor"
	NetQualityUnknown = "unknown"
)

// Selected ICE path classes (NetStatsPayload.Path): host/srflx outside the
// tailnet is "direct", 100.64.0.0/10 or fd7a:115c:a1e0::/48 is "tailnet".
const (
	NetPathDirect  = "direct"
	NetPathTailnet = "tailnet"
	NetPathRelay   = "relay"
	NetPathNone    = "none"
	NetPathUnknown = "unknown"
)

// Upload scheduling modes (NetUploadStats.Mode), see docs/ARCHITECTURE.md §4.4.
const (
	UploadModeIdle      = "idle"      // nothing queued, no take
	UploadModeLive      = "live"      // take recording, call healthy
	UploadModeThrottled = "throttled" // take recording, call bandwidth-limited or poor
	UploadModeDrain     = "drain"     // no take recording: catch up at full speed
)

// Limits applied by SanitizeNetStats.
const (
	NetStatsMaxReasons   = 4
	NetStatsMaxReasonLen = 48
	NetStatsMaxProtoLen  = 8
	NetStatsMaxLimitLen  = 16
)

// NetUploadStats is the participant's progressive-upload backlog: bytes
// recorded locally (in OPFS) that the host has not acknowledged yet.
type NetUploadStats struct {
	PendingBytes int64  `json:"pendingBytes"`
	RateBps      int64  `json:"rateBps"` // recent acknowledged upload rate, bits/s
	Mode         string `json:"mode"`
	Concurrency  int    `json:"concurrency"`
	// RateLimitBps is the token-bucket rate while throttled (0 = unpaced).
	RateLimitBps int64 `json:"rateLimitBps,omitempty"`
}

// NetStatsPayload is one participant's connection report. ParticipantID,
// Name and ReceivedAtMs are set by the hub from the authenticated connection.
type NetStatsPayload struct {
	ParticipantID string `json:"participantId,omitempty"`
	Name          string `json:"name,omitempty"`
	Quality       string `json:"quality"`
	Path          string `json:"path,omitempty"`
	Protocol      string `json:"protocol,omitempty"` // "udp" or "tcp" (relay: TURN transport)
	RTTMs         int    `json:"rttMs,omitempty"`
	// AvailableOutgoingKbps is the sender bandwidth estimate (transport-wide).
	AvailableOutgoingKbps int `json:"availableOutgoingKbps,omitempty"`
	// OutboundLossPct comes from RTCP receiver reports for our published
	// tracks; InboundLossPct and JitterMs from what we receive.
	OutboundLossPct   float64 `json:"outboundLossPct,omitempty"`
	InboundLossPct    float64 `json:"inboundLossPct,omitempty"`
	JitterMs          int     `json:"jitterMs,omitempty"`
	QualityLimitation string  `json:"qualityLimitation,omitempty"` // camera encoder: none|bandwidth|cpu|other
	FramesDroppedPct  float64 `json:"framesDroppedPct,omitempty"`
	// CapTier is the applied camera sender tier (0 = 720p/1.5 Mbps ... 4 =
	// 180p/150 kbps) and SendMaxKbps its bitrate cap. Recording is unaffected.
	CapTier      int             `json:"capTier"`
	SendMaxKbps  int             `json:"sendMaxKbps,omitempty"`
	Reasons      []string        `json:"reasons,omitempty"`
	Upload       *NetUploadStats `json:"upload,omitempty"`
	ReceivedAtMs int64           `json:"receivedAtMs,omitempty"`
}

// SanitizeNetStats validates enums, clamps numbers and truncates strings so
// a client cannot bloat or corrupt the dashboard. Identity fields are cleared
// for the caller to stamp from the authenticated connection.
func SanitizeNetStats(p NetStatsPayload) NetStatsPayload {
	out := NetStatsPayload{
		Quality:               NetQualityUnknown,
		RTTMs:                 clampInt(p.RTTMs, 0, 60_000),
		AvailableOutgoingKbps: clampInt(p.AvailableOutgoingKbps, 0, 10_000_000),
		OutboundLossPct:       clampFloat(p.OutboundLossPct, 0, 100),
		InboundLossPct:        clampFloat(p.InboundLossPct, 0, 100),
		JitterMs:              clampInt(p.JitterMs, 0, 60_000),
		FramesDroppedPct:      clampFloat(p.FramesDroppedPct, 0, 100),
		CapTier:               clampInt(p.CapTier, 0, 16),
		SendMaxKbps:           clampInt(p.SendMaxKbps, 0, 1_000_000),
		Protocol:              truncateRunes(p.Protocol, NetStatsMaxProtoLen),
		QualityLimitation:     truncateRunes(p.QualityLimitation, NetStatsMaxLimitLen),
	}
	switch p.Quality {
	case NetQualityGood, NetQualityFair, NetQualityPoor, NetQualityUnknown:
		out.Quality = p.Quality
	}
	switch p.Path {
	case NetPathDirect, NetPathTailnet, NetPathRelay, NetPathNone, NetPathUnknown:
		out.Path = p.Path
	}
	for _, r := range p.Reasons {
		if len(out.Reasons) == NetStatsMaxReasons {
			break
		}
		if r != "" {
			out.Reasons = append(out.Reasons, truncateRunes(r, NetStatsMaxReasonLen))
		}
	}
	if u := p.Upload; u != nil {
		mode := UploadModeIdle
		switch u.Mode {
		case UploadModeIdle, UploadModeLive, UploadModeThrottled, UploadModeDrain:
			mode = u.Mode
		}
		out.Upload = &NetUploadStats{
			PendingBytes: clampInt64(u.PendingBytes, 0, 1<<50),
			RateBps:      clampInt64(u.RateBps, 0, 100_000_000_000),
			Mode:         mode,
			Concurrency:  clampInt(u.Concurrency, 0, 16),
			RateLimitBps: clampInt64(u.RateLimitBps, 0, 100_000_000_000),
		}
	}
	return out
}

func clampInt(v, lo, hi int) int { return max(lo, min(hi, v)) }

func clampInt64(v, lo, hi int64) int64 { return max(lo, min(hi, v)) }

func clampFloat(v, lo, hi float64) float64 {
	if v != v { // NaN
		return lo
	}
	return max(lo, min(hi, v))
}

// ---- Live captions [SL-P4-2] ----
// Kept in its own block so concurrent protocol additions merge cleanly.
// Captions are recognized on the host's Mac from the call audio the host's
// browser already receives (docs/integrations/CAPTIONS.md). They never touch
// recording, takes or exports.

const (
	// TypeCaption (S→C, admitted only) is one caption segment. A partial
	// segment is re-sent with the same ID until a final one replaces it.
	TypeCaption = "caption"
	// TypeCaptionState (S→C, admitted AND waiting room) says whether live
	// captions are on, so everyone knows their speech is being transcribed.
	TypeCaptionState = "caption_state"
	// TypeCaptionControl (H→S) turns captions on or off and sets the language.
	TypeCaptionControl = "caption_control"
)

// Caption limits applied by the daemon to untrusted recognizer output.
const (
	CaptionMaxRunes    = 500
	CaptionDefaultLang = "en-US"
)

// CaptionPayload is a recognized segment of one participant's speech. ID is
// stable across a segment's partial updates; Seq increases per session. Times
// are host wall clock, Unix milliseconds. ParticipantID and Name come from the
// daemon's admitted roster, never from the recognizer. A final segment with
// empty Text withdraws the partial with the same ID.
type CaptionPayload struct {
	ID            string `json:"id"`
	Seq           uint64 `json:"seq"`
	ParticipantID string `json:"participantId"`
	Name          string `json:"name"`
	Text          string `json:"text"`
	IsFinal       bool   `json:"isFinal"`
	StartMs       int64  `json:"startMs"`
	EndMs         int64  `json:"endMs"`
}

// CaptionStatePayload carries no participant data: it also reaches the
// waiting room. Provider is true while an on-device recognizer is connected.
type CaptionStatePayload struct {
	Enabled      bool   `json:"enabled"`
	Language     string `json:"language"`
	Provider     bool   `json:"provider"`
	ProviderName string `json:"providerName,omitempty"`
}

// CaptionControlPayload is the host's request. An empty Language keeps the
// current one.
type CaptionControlPayload struct {
	Enabled  bool   `json:"enabled"`
	Language string `json:"language,omitempty"`
}

// SubsourceLinkResponse answers POST /api/v1/sessions/{id}/subsource
// [SL-P1-9]: the phone page path with its single-use capability, an optional
// base URL a phone can reach (set when the requester is on the local
// dashboard) and when the link stops working (Unix ms).
type SubsourceLinkResponse struct {
	Path      string `json:"path"`
	BaseURL   string `json:"baseUrl,omitempty"`
	ExpiresAt int64  `json:"expiresAt"`
}

// ---- Recording microphone processing mode [SL-P1-10] ----
// Kept in its own block so concurrent protocol additions merge cleanly.

// Recording mic modes. A participant's own choice is auto, raw or clean; the
// session default (session settings recording_mic_mode, also part of a
// room's recording preset) replaces "auto" for everyone when it is raw or
// clean. "auto" resolves to raw when the participant says they wear
// headphones, to clean otherwise. Raw = no browser processing; clean = echo
// cancellation and noise suppression on, automatic gain off.
const (
	RecordingMicAuto  = "auto"
	RecordingMicRaw   = "raw"
	RecordingMicClean = "clean"
)

// Why a recording mic mode resolved the way it did (RecordingMicState.Reason).
const (
	RecordingMicReasonParticipant  = "participant"   // the participant chose raw or clean
	RecordingMicReasonSession      = "session"       // the host's session/room default
	RecordingMicReasonHeadphones   = "headphones"    // auto, wearing headphones
	RecordingMicReasonNoHeadphones = "no_headphones" // auto, no headphones
)

// ValidRecordingMicMode reports whether m is a mode a setting may hold.
func ValidRecordingMicMode(m string) bool {
	return m == RecordingMicAuto || m == RecordingMicRaw || m == RecordingMicClean
}

// RecordingMicState is a participant's recording microphone as its device
// actually opened it. The booleans are the recording track's measured
// getSettings(), so they can differ from Resolved where a browser shares
// processing between captures of one device (FollowsCall is then set).
// Carried in PreflightPayload (the host's per-participant view) and written
// to each recorded track's sidecar as micProcessing (manifest.json).
type RecordingMicState struct {
	Mode             string `json:"mode"`
	Resolved         string `json:"resolved"`
	Reason           string `json:"reason,omitempty"`
	Headphones       bool   `json:"headphones"`
	EchoCancellation bool   `json:"echoCancellation"`
	NoiseSuppression bool   `json:"noiseSuppression"`
	AutoGainControl  bool   `json:"autoGainControl"`
	FollowsCall      bool   `json:"followsCall,omitempty"`
}

// SanitizeRecordingMic whitelists the enums of an untrusted report; nil when
// the report is unusable.
func SanitizeRecordingMic(p *RecordingMicState) *RecordingMicState {
	if p == nil || !ValidRecordingMicMode(p.Mode) || (p.Resolved != RecordingMicRaw && p.Resolved != RecordingMicClean) {
		return nil
	}
	out := *p
	switch p.Reason {
	case RecordingMicReasonParticipant, RecordingMicReasonSession, RecordingMicReasonHeadphones, RecordingMicReasonNoHeadphones:
	default:
		out.Reason = ""
	}
	return &out
}

// ---- Producer role [SL-P1-11] ----
// Kept in its own block so concurrent protocol additions merge cleanly.

// Participant roles. A producer joins with the session's or room's separate
// producer link, needs the host's admission (never auto-admitted, never
// admitted by another producer), is heard and optionally seen in the call,
// and is never recorded: it gets no record_start, is not an expected
// recorder, cannot register tracks and is not captioned. It may assist the
// host: admit or deny waiting guests, start and stop takes (with the
// pre-flight refresh), mute guests and other producers, remove guests and
// see the host dashboard (source health and pre-flight). It cannot end the
// meeting, lock the room, manage links, livestreams or captions, stop a
// screen share, play the sync chirp, or act on the host; every host HTTP
// route needs a host token, which a producer never holds.
const (
	RoleHost     = "host"
	RoleGuest    = "guest"
	RoleProducer = "producer"
)

// ProducerLinkResponse answers GET /api/v1/sessions/{id}/producer and its
// rotate/revoke actions (host management only). URL carries the link
// secret: never logged. Enabled is false after revoke (or before the first
// rotate).
type ProducerLinkResponse struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url,omitempty"`
	Path    string `json:"path,omitempty"`
	// Remote is true when URL is an HTTPS address a remote producer can open.
	Remote bool `json:"remote"`
	// RoomID is set when the meeting's producer link is its room's link.
	RoomID string `json:"roomId,omitempty"`
}

// ---- Teleprompter [SL-P5-1] ----
// Kept in its own block so concurrent protocol additions merge cleanly.
//
// One script per session, plain text with "# " / "## " headings. A meeting
// opened from a room keeps the room's script (SQLite); an instant meeting's
// script lives in daemon memory only. The script is untrusted text: clients
// render it with textContent and the daemon never logs it.
//
// Who may do what (enforced in the hub):
//   - visibility (who sees the script) is set by the host only:
//     "host" (only the host), "crew" (host + producers), "everyone".
//   - the host edits; an admitted producer edits unless visibility is
//     "host" (the script is then private to the host and never sent to it).
//   - guests and phones never edit; guests receive the text only with
//     visibility "everyone" (otherwise an empty, hidden state).
//   - only the host sends prompter_scroll; it is relayed to everyone else
//     who may see the script, who follow it when they choose "Follow host".
const (
	TypePrompter       = "prompter"        // S→C PrompterPayload (admitted)
	TypePrompterEdit   = "prompter_edit"   // H/P→S PrompterEditPayload
	TypePrompterScroll = "prompter_scroll" // H→S→C PrompterScrollPayload
)

// Teleprompter visibilities (PrompterPayload.Visibility).
const (
	PrompterHost     = "host"
	PrompterCrew     = "crew"
	PrompterEveryone = "everyone"
)

// Teleprompter limits. The script cap is in bytes (UTF-8) and applies to
// the whole text; edits and scroll messages are rate limited per sender.
const (
	PrompterMaxBytes        = 64 * 1024
	PrompterScrollRateLimit = 20
	PrompterScrollWindow    = 2 * time.Second
	PrompterEditRateLimit   = 20
	PrompterEditWindow      = 10 * time.Second
	PrompterMaxSpeed        = 10
)

// ValidPrompterVisibility reports whether v is a known visibility.
func ValidPrompterVisibility(v string) bool {
	return v == PrompterHost || v == PrompterCrew || v == PrompterEveryone
}

// PrompterPayload is the session's script as one recipient may see it.
// Hidden is true when the recipient may not see the script: Text is then
// empty. Revision increases with every accepted edit; UpdatedBy is the
// editor's display name.
type PrompterPayload struct {
	Text       string `json:"text"`
	Visibility string `json:"visibility"`
	Revision   uint64 `json:"revision"`
	UpdatedBy  string `json:"updatedBy,omitempty"`
	Hidden     bool   `json:"hidden,omitempty"`
	// Scroll is the host's last shared scroll state, if it shares one.
	Scroll *PrompterScrollPayload `json:"scroll,omitempty"`
}

// PrompterEditPayload replaces the script text and/or (host only) the
// visibility. A nil Text keeps the current text.
type PrompterEditPayload struct {
	Text       *string `json:"text,omitempty"`
	Visibility string  `json:"visibility,omitempty"`
}

// PrompterScrollPayload is the host's reader position: Position is the
// fraction of the script scrolled (0..1), Playing whether auto-scroll runs
// and Speed its 1..PrompterMaxSpeed setting. Shared=false stops sharing.
type PrompterScrollPayload struct {
	Position float64 `json:"position"`
	Playing  bool    `json:"playing"`
	Speed    float64 `json:"speed"`
	Shared   bool    `json:"shared"`
}

// ---- Branding and lower thirds [SL-P5-2] ----
// Kept in its own block so concurrent protocol additions merge cleanly.
//
// Brand settings (show name, accent, logo, tile labels, livestream lower
// thirds) are host-managed over HTTP (/api/v1/sessions/{id}/branding,
// /api/v1/rooms/{id}/branding; host management only, never through Funnel).
// The logo is PNG or JPEG only (no SVG), at most BrandLogoMaxBytes, checked
// by magic bytes and a decoded header, and served from our own origin to
// the host. Lower-third text per participant is edited by the host or a
// producer over the WebSocket and exported in manifest.json.
const (
	TypeBranding   = "branding"    // S→C BrandingPayload (admitted)
	TypeLowerThird = "lower_third" // H/P→S LowerThirdPayload
)

// Branding limits.
const (
	BrandLogoMaxBytes       = 1 << 20
	BrandLogoMaxDimension   = 4096
	BrandShowNameMaxRunes   = 80
	LowerThirdNameMaxRunes  = 80
	LowerThirdTitleMaxRunes = 120
)

// Livestream lower-third modes (BrandingPayload.LowerThirdsMode).
const (
	LowerThirdsSpeaker = "speaker" // when someone becomes the active speaker, and on demand
	LowerThirdsManual  = "manual"  // on demand only
	LowerThirdsOff     = "off"
)

// DefaultBrandAccent is the accent used until the host picks one.
const DefaultBrandAccent = "#bfdbe8"

// LowerThirdPayload sets a participant's lower third. Empty Name means the
// participant's display name; both empty clears it. Sanitized like names.
type LowerThirdPayload struct {
	ParticipantID string `json:"participantId"`
	Name          string `json:"name,omitempty"`
	Title         string `json:"title,omitempty"`
}

// BrandingPayload is the session's brand as every admitted participant sees
// it. LogoVersion is empty without a logo; the logo itself is fetched by the
// host only (host management route). LowerThirds lists participants with
// lower-third text; everyone else defaults to their display name.
type BrandingPayload struct {
	ShowName        string              `json:"showName"`
	Accent          string              `json:"accent"`
	TileLabels      bool                `json:"tileLabels"`
	LowerThirdsMode string              `json:"lowerThirdsMode"`
	LogoVersion     string              `json:"logoVersion,omitempty"`
	LowerThirds     []LowerThirdPayload `json:"lowerThirds"`
	// RememberTitles is true in a room's meeting: lower thirds set there are
	// remembered by display name for the room's next episode [SL-P5-4].
	RememberTitles bool `json:"rememberTitles,omitempty"`
}

// BrandSettings is the body of GET/PUT .../branding. Logo is read-only
// here (POST/DELETE .../branding/logo change it).
type BrandSettings struct {
	ShowName        string     `json:"showName"`
	Accent          string     `json:"accent"`
	TileLabels      bool       `json:"tileLabels"`
	LowerThirdsMode string     `json:"lowerThirdsMode"`
	Logo            *BrandLogo `json:"logo,omitempty"`
}

// BrandLogo describes a stored logo. Version changes with its content.
type BrandLogo struct {
	Type    string `json:"type"`
	Size    int    `json:"size"`
	Version string `json:"version"`
}

// ValidAccent reports whether s is a #rrggbb colour.
func ValidAccent(s string) bool {
	if len(s) != 7 || s[0] != '#' {
		return false
	}
	for _, c := range s[1:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// ValidLowerThirdsMode reports whether m is a known mode.
func ValidLowerThirdsMode(m string) bool {
	return m == LowerThirdsSpeaker || m == LowerThirdsManual || m == LowerThirdsOff
}
