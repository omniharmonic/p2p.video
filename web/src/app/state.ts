// Typed session, participant and take state shared by the app modules
// [SL-TECH-1]. Module-private details (sockets, timers, recorders) stay in
// their modules; only what several modules read or write lives here.
import type { SessionSettingsData } from '../components/SettingsDrawer';
import type { DashboardPayload, ICEServer, ParticipantStatePayload } from '../lib/protocol';

export type RecordState = 'idle' | 'arming' | 'recording' | 'stopping';

export interface AppState {
  /** Session and credential from the URL, or from the home screen once a meeting opens. */
  sessionId: string;
  token: string;
  /** Assigned by the hub on admission; the signed resume grant is `uploadToken`. */
  participantId: string;
  uploadToken: string;
  isHost: boolean;
  // Producer [SL-P1-11]: admitted assistant, never recorded. The hub enforces
  // every permission; the UI shows only what a producer may do.
  isProducer: boolean;
  joiningAsProducer: boolean;
  producerSeesRecording: boolean;
  /**
   * A take is recording on the host but this page is not recording it: the
   * host's window or tab closed mid-take and was reopened [SL-P2-3]. Record
   * then stops that take, like a producer's button.
   */
  hostSeesUnrecordedTake: boolean;
  /** Joined from the green room (devices locked). */
  hasJoined: boolean;
  // Call media recovery: the PeerConnection is rebuilt and re-offered when it
  // fails, independently of the signaling socket and of local recording.
  callAdmitted: boolean;
  admittedIceServers: ICEServer[];
  peerState: RTCPeerConnectionState;
  /** Camera + recording microphone (the capture stream). */
  localStream: MediaStream | null;
  isMicMuted: boolean;
  isCamOff: boolean;
  /** Local take lifecycle. */
  recordState: RecordState;
  isRecording: boolean;
  currentTakeId: string;
  currentTakeNum: number;
  recStartTimeMs: number;
  exportedTakeId: string;
  latestDashboard: DashboardPayload | null;
  pendingGuestId: string | null;
  /** Active settings with defaults. */
  activeSettings: SessionSettingsData;
}

const urlParams = new URLSearchParams(window.location.search);

export const state: AppState = {
  sessionId: urlParams.get('session') || '',
  token: urlParams.get('token') || '',
  participantId: '',
  uploadToken: '',
  isHost: false,
  isProducer: false,
  joiningAsProducer: false,
  producerSeesRecording: false,
  hostSeesUnrecordedTake: false,
  hasJoined: false,
  callAdmitted: false,
  admittedIceServers: [],
  peerState: 'new',
  localStream: null,
  isMicMuted: false,
  isCamOff: false,
  recordState: 'idle',
  isRecording: false,
  currentTakeId: '',
  currentTakeNum: 1,
  recStartTimeMs: 0,
  exportedTakeId: '',
  latestDashboard: null,
  pendingGuestId: null,
  activeSettings: {
    video_bitrate_bps: 12_000_000,
    video_codec: 'h264',
    target_fps: 30.0,
    audio_sample_rate: 48000,
    audio_bit_depth: 24,
    xcorr_threshold: 0.65,
    xcorr_window_ms: 500,
    engine: 'mediarecorder',
  },
};

/** Display names by participant ID (session detail, dashboard, participant_state). */
export const participantNames = new Map<string, string>();
/** Remote mic/camera state by participant ID. */
export const participantStates = new Map<string, ParticipantStatePayload>();
/** Remote video elements that have decoded a frame (connection pill). */
export const decodedRemoteVideos = new Set<string>();

/** Host or producer: may admit guests, start/stop takes and see source health. */
export const canAssist = (): boolean => state.isHost || state.isProducer;
