// Mirrors host/internal/signaling/protocol.go

export const TypeHello = "hello";
export const TypeAdmission = "admission";
export const TypeJoinRequest = "join_request";
export const TypeAdmit = "admit";
export const TypeDeny = "deny";
export const TypeSdpOffer = "sdp_offer";
export const TypeSdpAnswer = "sdp_answer";
export const TypeIceCandidate = "ice_candidate";
export const TypeSourceAdd = "source_add";
export const TypeSourceRemove = "source_remove";
export const TypeClockPing = "clock_ping";
export const TypeClockPong = "clock_pong";
export const TypeRecordStart = "record_start";
export const TypeRecordStop = "record_stop";
export const TypeChirp = "chirp";
export const TypeRecorderStatus = "recorder_status";
export const TypeDashboard = "dashboard";
export const TypeSessionEnd = "session_end";
export const TypeParticipantState = "participant_state";

export interface Envelope<T = unknown> {
  t: string;
  id?: string;
  participantId?: string;
  payload?: T;
}

export interface HelloPayload {
  resumeToken?: string;
  name: string;
  role: "host" | "guest" | "subsource" | "producer";
  parentParticipantId?: string;
  capabilities?: Record<string, boolean>;
  devices?: Array<{ deviceId: string; kind: string; label: string }>;
}

export interface ICEServer {
  urls: string[];
  username?: string;
  credential?: string;
}

export interface AdmissionPayload {
  activeTakeId?: string;
  role?: "host" | "guest" | "subsource" | "producer";
  status: "admitted" | "pending" | "denied";
  reason?: DenialReason; // denied only
  participantId: string;
  uploadToken?: string;
  iceServers?: ICEServer[];
  /** Phones (role "subsource") only [SL-P1-9]: who the phone records for. */
  parentParticipantId?: string;
  /** Phones only: the server-assigned display name ("<Name> · Phone"). */
  name?: string;
}

/** Phone camera (PRD F3) [SL-P1-9]. */
export const RoleSubsource = "subsource";
export const SubsourceCapTTLMs = 10 * 60 * 1000;
/** POST /api/v1/sessions/{id}/subsource with the participant's upload token. */
export interface SubsourceLinkResponse {
  /** "/phone.html?session=…&token=<single-use capability>" */
  path: string;
  /** Set when the requester is on the local dashboard: a base a phone can reach. */
  baseUrl?: string;
  /** Unix ms when the link stops working if unused. */
  expiresAt: number;
}

/**
 * A client offer with renegotiate=true updates its existing PeerConnection
 * (screen share add/remove); without it the offer creates or replaces the
 * participant's peer (join and media recovery).
 */
export interface SdpPayload {
  sdp: string;
  type: "offer" | "answer";
  renegotiate?: boolean;
}

export interface AdmitPayload {
  participantId: string;
}

export interface DenyPayload {
  participantId: string;
  reason?: string;
}

export interface IceCandidatePayload {
  candidate: string;
  sdpMid?: string;
  sdpMLineIndex?: number;
}

export interface ClockPingPayload {
  t0: number; // client local perfTime in microseconds
}

export interface ClockPongPayload {
  t0: number;
  t1: number; // host arrival in microseconds
  t2: number; // host departure in microseconds
}

export interface RecordStartPayload {
  takeId: string;
  takeNumber: number;
  startAtHostUs: number;
  preset: string;
}

export interface RecordStopPayload {
  takeId: string;
  stopAtHostUs: number;
}

export interface RecorderStatusPayload {
  trackId: string;
  state: "recording" | "uploading" | "done" | "error";
  bufferedBytes: number;
  uploadedBytes: number;
  droppedFrames: number;
  lastSeq: number;
}

export interface DashboardParticipantState {
  participantId: string;
  name: string;
  role: string;
  status: string;
  clockOffsetUs: number;
  rttUs: number;
  tracks: Record<string, RecorderStatusPayload>;
  /** Latest pre-flight checklist [SL-P0-5]. */
  preflight?: PreflightPayload;
  /** Latest connection quality + upload backlog report [SL-P0-8]. */
  netStats?: NetStatsPayload;
}

// Call-path mic/camera state. Clients send micMuted/camOff; the host stamps
// participantId/name from the authenticated connection and relays it to the
// admitted participants of the session.
export interface ParticipantStatePayload {
  participantId?: string;
  name?: string;
  micMuted: boolean;
  camOff: boolean;
}

// In-call collaboration (ROADMAP P1). Host-only actions are enforced by role
// on the host; hiding them in the UI is a convenience, not the control.
export const TypeChat = "chat";
export const TypeChatHistory = "chat_history";
export const TypeHand = "hand";
export const TypeReaction = "reaction";
export const TypeRoster = "roster";
export const TypeRoomState = "room_state";
export const TypeMuteRequest = "mute_request";
export const TypeRemove = "remove";
export const TypeRoomLock = "room_lock";
export const TypeActionError = "action_error";

export const ChatMaxChars = 2000;
export const AllowedReactions = ["👍", "❤️", "😂", "😮", "👏", "🎉"] as const;
export type ReactionEmoji = (typeof AllowedReactions)[number];

export type DenialReason = "declined" | "removed" | "room_locked" | "unavailable" | "busy";

/** C→S: identity and time are assigned by the host. */
export interface ChatSendPayload {
  text: string;
}

/** S→C: a server-stamped chat line. `text` is untrusted: render as text. */
export interface ChatMessage {
  id: string;
  participantId: string;
  name: string;
  role: string;
  text: string;
  ts: number; // host wall clock, Unix ms
}

export interface ChatHistoryPayload {
  messages: ChatMessage[];
}

/** participantId only when the host lowers someone else's hand. */
export interface HandPayload {
  participantId?: string;
  raised: boolean;
}

export interface ReactionPayload {
  participantId?: string;
  name?: string;
  emoji: ReactionEmoji;
}

export interface RosterEntry {
  participantId: string;
  name: string;
  role: string;
  micMuted: boolean;
  camOff: boolean;
  handRaised: boolean;
  handRaisedAt?: number; // Unix ms; orders the hand queue
  screenSharing?: boolean;
  /** Marks a phone (role "subsource") of that participant [SL-P1-9]. */
  parentParticipantId?: string;
}

export interface RosterPayload {
  participants: RosterEntry[];
  locked: boolean;
}

/** Also sent to the waiting room, so it carries no participant data. */
export interface RoomStatePayload {
  locked: boolean;
  recording: boolean;
  takeNumber?: number;
  /** The session's recording mic default [SL-P1-10]. */
  recordingMicMode?: RecordingMicMode;
}

/** S→C (incl. waiting room): the host ended the meeting [SL-P1-5]. */
export interface SessionEndPayload {
  reason: "ended_by_host";
}

export interface MuteRequestPayload {
  participantId: string;
  by?: string;
}

export interface RemovePayload {
  participantId: string;
}

export interface RoomLockPayload {
  locked: boolean;
}

export interface ActionErrorPayload {
  action: string;
  code: "forbidden" | "rate_limited" | "invalid" | "not_found" | "conflict";
  message: string;
}

// Screen share (ROADMAP P1). One presenter per room; the host can stop it.
// The SFU forwards a declared screen as stream ID participantId + suffix.
export const TypeScreenStop = "screen_stop";
export const SourceKindScreen = "screen";
export const ScreenStreamSuffix = "_screen";
export const MaxScreenTrackIDs = 2;

/** C→S before publishing the share's tracks (MediaStreamTrack IDs). */
export interface SourceAddPayload {
  sourceId: string;
  kind: "camera" | "mic" | "screen";
  label: string;
  trackIds?: string[];
}

export interface SourceRemovePayload {
  sourceId: string;
  kind: string;
}

/** H→S {participantId}; S→C to the presenter {participantId, by}. */
export interface ScreenStopPayload {
  participantId: string;
  by?: string;
}

export interface DashboardPayload {
  sessionId: string;
  title: string;
  currentTake: number;
  isRecording: boolean;
  participants: DashboardParticipantState[];
  /** Hub time (Unix ms) of the host's latest pre-flight refresh request [SL-P0-5]. */
  preflightRefreshAtMs?: number;
}

// ---- Pre-flight "ready to record" [SL-P0-5] ----
// Mirrors the separate block in protocol.go.
export const TypePreflight = "preflight";

export type PreflightStatus = "ok" | "warn" | "fail" | "info";

export interface PreflightItem {
  id: string;
  label: string;
  status: PreflightStatus;
  detail?: string;
}

// Clients send items only; the host stamps participantId/name/receivedAtMs and
// recomputes overall (worst of ok/warn/fail; "info" never counts). A host's
// refresh:true asks admitted participants to re-check and report now.
export interface PreflightPayload {
  participantId?: string;
  name?: string;
  overall?: "ok" | "warn" | "fail";
  items: PreflightItem[];
  refresh?: boolean;
  receivedAtMs?: number;
  /** The participant's recording mic as opened [SL-P1-10]. */
  recordingMic?: RecordingMicState;
}

// ---- Network quality and upload backlog [SL-P0-8] ----
// Mirrors the separate block in protocol.go.
export const TypeNetStats = "net_stats";
export const NetStatsIntervalMs = 2000;

export type NetQuality = "good" | "fair" | "poor" | "unknown";
export type NetPath = "direct" | "tailnet" | "relay" | "none" | "unknown";
export type UploadMode = "idle" | "live" | "throttled" | "drain";

export interface NetUploadStats {
  /** Bytes recorded locally (OPFS) not yet acknowledged by the host. */
  pendingBytes: number;
  /** Recent acknowledged upload rate, bits/s. */
  rateBps: number;
  mode: UploadMode;
  concurrency: number;
  /** Token-bucket rate while throttled, bits/s (absent = unpaced). */
  rateLimitBps?: number;
}

// Clients send everything but identity; the hub stamps participantId, name
// and receivedAtMs and relays the report to the other admitted participants.
export interface NetStatsPayload {
  participantId?: string;
  name?: string;
  quality: NetQuality;
  path?: NetPath;
  protocol?: string;
  rttMs?: number;
  availableOutgoingKbps?: number;
  outboundLossPct?: number;
  inboundLossPct?: number;
  jitterMs?: number;
  qualityLimitation?: string;
  framesDroppedPct?: number;
  /** Applied camera sender tier (0 = 720p/1.5 Mbps … 4 = 180p/150 kbps). */
  capTier: number;
  sendMaxKbps?: number;
  reasons?: string[];
  upload?: NetUploadStats;
  receivedAtMs?: number;
}

// ---- Live captions [SL-P4-2] ----
// Mirrors the separate block in protocol.go. Recognized on the host's Mac
// (docs/integrations/CAPTIONS.md); never part of recording or exports.
export const TypeCaption = "caption";
export const TypeCaptionState = "caption_state";
export const TypeCaptionControl = "caption_control";
export const CaptionMaxRunes = 500;
export const CaptionDefaultLang = "en-US";

/** S→C (admitted only). Partials repeat `id` until a final replaces them; a
 * final with empty text withdraws the partial. Times: host Unix ms. */
export interface CaptionPayload {
  id: string;
  seq: number;
  participantId: string;
  name: string;
  text: string;
  isFinal: boolean;
  startMs: number;
  endMs: number;
}

/** S→C, also to the waiting room: are captions on, and is a recognizer connected. */
export interface CaptionStatePayload {
  enabled: boolean;
  language: string;
  provider: boolean;
  providerName?: string;
}

/** H→S. An empty language keeps the current one. */
export interface CaptionControlPayload {
  enabled: boolean;
  language?: string;
}

// ---- Recording microphone processing mode [SL-P1-10] ----
// Mirrors the separate block in protocol.go.
export type RecordingMicMode = "auto" | "raw" | "clean";
export type ResolvedMicMode = "raw" | "clean";
export type RecordingMicReason = "participant" | "session" | "headphones" | "no_headphones";
export const RecordingMicModes: readonly RecordingMicMode[] = ["auto", "raw", "clean"];

/** The recording mic as opened; the booleans are the track's measured getSettings(). */
export interface RecordingMicState {
  mode: RecordingMicMode;
  resolved: ResolvedMicMode;
  reason?: RecordingMicReason;
  headphones: boolean;
  echoCancellation: boolean;
  noiseSuppression: boolean;
  autoGainControl: boolean;
  /** The browser applied the call's processing to the recording too. */
  followsCall?: boolean;
}

// ---- Producer role [SL-P1-11] ----
// Mirrors the separate block in protocol.go. A producer is admitted by the
// host, heard (camera optional), never recorded or captioned, and may admit
// or decline waiting guests, start/stop takes, mute guests and producers,
// remove guests and see source health. Enforced on the hub.
export const RoleHost = "host";
export const RoleGuest = "guest";
export const RoleProducer = "producer";

/** GET /api/v1/sessions/{id}/producer and POST …/producer/{rotate,revoke} (host only). */
export interface ProducerLinkResponse {
  enabled: boolean;
  url?: string;
  path?: string;
  remote: boolean;
  roomId?: string;
}

// ---- Teleprompter [SL-P5-1] ----
// Mirrors the separate block in protocol.go. Visibility is set by the host
// only; the host edits, a producer edits unless the script is "host"
// (private); guests never edit and get the text only with "everyone". Only
// the host sends prompter_scroll. The script is untrusted: render with
// textContent only.
export const TypePrompter = "prompter";
export const TypePrompterEdit = "prompter_edit";
export const TypePrompterScroll = "prompter_scroll";
export type PrompterVisibility = "host" | "crew" | "everyone";
export const PrompterVisibilities: readonly PrompterVisibility[] = ["host", "crew", "everyone"];
export const PrompterMaxBytes = 64 * 1024;
export const PrompterMaxSpeed = 10;

export interface PrompterScrollPayload {
  /** Fraction of the script scrolled, 0..1. */
  position: number;
  playing: boolean;
  /** 1..PrompterMaxSpeed. */
  speed: number;
  /** false: the host stopped sharing its position. */
  shared: boolean;
}

/** S→C: the script as this participant may see it. */
export interface PrompterPayload {
  text: string;
  visibility: PrompterVisibility;
  revision: number;
  updatedBy?: string;
  /** This participant may not see the script (text is empty). */
  hidden?: boolean;
  scroll?: PrompterScrollPayload;
}

/** H/P→S: new text and/or (host only) visibility. */
export interface PrompterEditPayload {
  text?: string;
  visibility?: PrompterVisibility;
}

// ---- Branding and lower thirds [SL-P5-2] ----
// Mirrors the separate block in protocol.go. Brand settings and the logo
// are host management HTTP routes; lower-third text is edited by the host
// or a producer over the WebSocket.
export const TypeBranding = "branding";
export const TypeLowerThird = "lower_third";
export const BrandLogoMaxBytes = 1 << 20;
export const BrandShowNameMaxRunes = 80;
export const LowerThirdNameMaxRunes = 80;
export const LowerThirdTitleMaxRunes = 120;
export const DefaultBrandAccent = "#bfdbe8";
export type LowerThirdsMode = "speaker" | "manual" | "off";

export interface LowerThirdPayload {
  participantId: string;
  /** Empty: the participant's display name. */
  name?: string;
  title?: string;
}

/** S→C to every admitted participant. */
export interface BrandingPayload {
  showName: string;
  accent: string;
  tileLabels: boolean;
  lowerThirdsMode: LowerThirdsMode;
  /** Empty without a logo; the host fetches it from …/branding/logo. */
  logoVersion?: string;
  lowerThirds: LowerThirdPayload[];
  /** True in a room's meeting: lower thirds are remembered by display name for the next episode [SL-P5-4]. */
  rememberTitles?: boolean;
}

/** GET/PUT /api/v1/sessions/{id}/branding (and rooms/{id}/branding). */
export interface BrandSettings {
  showName: string;
  accent: string;
  tileLabels: boolean;
  lowerThirdsMode: LowerThirdsMode;
  logo?: { type: string; size: number; version: string };
}
