// The call path [SL-TECH-1]: the SFU PeerConnection, remote tiles and names,
// media recovery, screen share, layout and connection quality. Nothing here
// touches local recording (docs/ARCHITECTURE.md §4).
import { CallNetwork } from '../components/CallNetwork';
import { LayoutSwitcher } from '../components/LayoutSwitcher';
import { ScreenShare } from '../components/ScreenShare';
import { soundUnlock } from '../components/SoundUnlock';
import {
  IceCandidatePayload, ParticipantStatePayload, ScreenStreamSuffix, SdpPayload, TypeIceCandidate,
  TypeParticipantState, TypeSdpAnswer, TypeSdpOffer,
} from '../lib/protocol';
import { WebRTCClient } from '../lib/rtc';
import { layoutSwitcherAnchor, telemetryStatus, videoGrid } from './dom';
import { preflight } from './greenroom';
import { applyCallAudio, callMic, effects, noiseSuppression } from './media';
import { startScreenRecording } from './recording';
import { queueSignalingWork, sendEnvelope, signalingOpen } from './signaling';
import { decodedRemoteVideos, participantNames, participantStates, state } from './state';
import { callFeatures } from './studio';
import { renderTileState, showToast, updateConnectionPill } from './ui';

export let layoutSwitcher: LayoutSwitcher;
export let rtc: WebRTCClient;
export let screenShare: ScreenShare;
export let callNetwork: CallNetwork;
// Call media recovery: the PeerConnection is rebuilt and re-offered when it
// fails, independently of the signaling socket and of local recording.
let mediaRecoveryTimer: number | null = null;
let mediaRecoveryAttempt = 0;
let mediaConnectWatchdog: number | null = null;
const MEDIA_CONNECT_TIMEOUT_MS = 20_000;

/** Active speaker uses remote audio levels from the call (never recording). */
export function createLayoutSwitcher(): void {
  layoutSwitcher = new LayoutSwitcher(videoGrid, undefined, { getPeerConnection: () => rtc.pc });
  if (layoutSwitcherAnchor) {
    layoutSwitcherAnchor.appendChild(layoutSwitcher.getElement());
  }
}

// WebRTC SFU Client with Trickle ICE and Connection State Monitoring
// The SFU labels every forwarded track with its publisher's participant ID as
// the stream ID, so one tile per stream is one tile per participant.
function remoteTileCount(): number {
  return videoGrid.querySelectorAll('.tile[data-participant-id]').length;
}
function updateCallParticipantCount(): void {
  void rtc.setParticipantCount(remoteTileCount() + 1);
}
// A departed (or recovering) publisher's tracks are removed by SFU
// renegotiation; drop the tile once its stream has no tracks left. Deferred so
// a removal and re-addition in the same renegotiation don't flicker the tile.
function pruneRemoteTile(stream: MediaStream): void {
  setTimeout(() => {
    const tile = document.getElementById(`tile-${stream.id}`);
    const video = tile?.querySelector('video');
    if (!tile || !video || video.srcObject !== stream) return;
    if (stream.getTracks().some(track => track.readyState === 'live')) return;
    tile.remove();
    decodedRemoteVideos.delete(`remote-${stream.id}`);
    layoutSwitcher.applyLayout();
    updateCallParticipantCount();
    updateConnectionPill();
  }, 0);
}
/** WebRTC SFU client with trickle ICE and connection state monitoring. */
export function createCallPeer(): void {
  rtc = new WebRTCClient(
    (track, stream) => {
      // Screen shares arrive as stream "<participantId>_screen" (ScreenShare.ts).
      if (stream.id.endsWith(ScreenStreamSuffix)) { screenShare.attachRemote(track, stream); return; }
      const remoteId = `remote-${stream.id}`;
      let remoteVideo = document.getElementById(remoteId) as HTMLVideoElement;
      if (!remoteVideo) {
        const tile = document.createElement('div');
        tile.className = 'tile';
        tile.id = `tile-${stream.id}`;
        tile.dataset.participantId = stream.id;
        tile.innerHTML = `
        <video autoplay playsinline></video>
        <div class="tile-name">Remote source</div>
        <div class="tile-state"><span class="tile-state-mic" hidden>Mic off</span><span class="tile-state-cam" hidden>Camera off</span></div>
      `;
        videoGrid.appendChild(tile);
        remoteVideo = tile.querySelector('video')!;
        remoteVideo.id = remoteId;
        layoutSwitcher.applyLayout();
        updateCallParticipantCount();
        if (!participantNames.has(stream.id)) void refreshParticipantNames();
      }
      updateRemoteLabels();
      remoteVideo.onplaying = () => { if (remoteVideo.videoWidth > 0) decodedRemoteVideos.add(remoteId); updateConnectionPill(); };
      remoteVideo.onwaiting = remoteVideo.onstalled = () => { decodedRemoteVideos.delete(remoteId); updateConnectionPill(); };
      track.addEventListener('ended', () => { decodedRemoteVideos.delete(remoteId); updateConnectionPill(); pruneRemoteTile(stream); });
      stream.onremovetrack = () => pruneRemoteTile(stream);
      if (remoteVideo.srcObject !== stream) remoteVideo.srcObject = stream;
      soundUnlock.ensurePlaying(remoteVideo); // Safari autoplay policy [SL-P1-12]
    },
    (candidate) => {
      sendEnvelope({
        t: TypeIceCandidate,
        participantId: state.participantId,
        payload: candidate,
      });
    }
  );

  rtc.onConnectionStateChange = (connection) => {
    state.peerState = connection; updateConnectionPill();
    if (connection === 'connected') {
      if (mediaConnectWatchdog !== null) { clearTimeout(mediaConnectWatchdog); mediaConnectWatchdog = null; }
      if (mediaRecoveryAttempt > 0) showToast('Call media reconnected');
      mediaRecoveryAttempt = 0;
      if (state.recordState === 'idle') void preflight.run(); // report the new connection path promptly
      telemetryStatus.innerText = state.isRecording ? 'Recording locally' : '● Media connection established';
    } else if (connection === 'failed' || connection === 'disconnected') {
      telemetryStatus.innerText = state.isRecording ? `Call media ${connection} · recording continues locally` : `● Call media ${connection}`;
    }
  };
  rtc.onMediaFailure = () => scheduleMediaRecovery();
}

/** Screen share (ROADMAP P1): UI and remote share tiles live in the component. */
export function createScreenShare(): void {
  screenShare = new ScreenShare({
    send: env => sendEnvelope(env),
    selfId: () => state.participantId,
    isHost: () => state.isHost,
    rtc,
    toast: msg => showToast(msg),
    startRecording: stream => startScreenRecording(stream),
  }, videoGrid);
}

/** Connection quality bars, adaptive call caps and upload pacing [SL-P0-8]. */
export function createCallNetwork(): void {
  callNetwork = new CallNetwork({
    rtc, grid: videoGrid, send: env => sendEnvelope(env), selfId: () => state.participantId, inCall: () => state.callAdmitted,
    recording: () => state.recordState === 'arming' || state.recordState === 'recording',
    toast: msg => showToast(msg), peopleChanged: () => callFeatures.refreshPeople(),
  });
}

/** Admitted (again): the next media failure retries immediately. */
export function resetMediaRecovery(): void {
  mediaRecoveryAttempt = 0;
}

// Build a fresh PeerConnection and offer it. The SFU replaces any previous
// peer for this participant. Capture streams, recorders and uploads are never
// touched: closing a PeerConnection does not stop its source tracks.
export async function negotiateMedia(): Promise<void> {
  if (!state.callAdmitted || !signalingOpen()) return;
  clearRemoteTiles();
  // Call audio processing is optional: a microphone this browser can't
  // process must never keep the call from connecting [SL-P1-12].
  if (state.localStream) await applyCallAudio(noiseSuppression.processing).catch(error => showToast(`Call audio processing unavailable in this browser (${error instanceof Error ? error.message : String(error)}); the call uses your microphone as captured.`));
  rtc.init(state.admittedIceServers);
  if (state.localStream) rtc.addTracks(effects.callStream(callMic.callStream(state.localStream)));
  screenShare.beforeOffer();
  const pc = rtc.pc;
  const offerSdp = await rtc.createOffer();
  sendEnvelope({ t: TypeSdpOffer, participantId: state.participantId, payload: { sdp: offerSdp, type: 'offer' } as SdpPayload });
  if (mediaConnectWatchdog !== null) clearTimeout(mediaConnectWatchdog);
  // A lost or rejected answer leaves the peer stuck in 'new'/'connecting'.
  mediaConnectWatchdog = window.setTimeout(() => {
    mediaConnectWatchdog = null;
    if (pc && rtc.pc === pc && pc.connectionState !== 'connected') scheduleMediaRecovery();
  }, MEDIA_CONNECT_TIMEOUT_MS);
}

export function scheduleMediaRecovery(): void {
  if (mediaRecoveryTimer !== null || !state.callAdmitted || !signalingOpen()) return;
  // First retry is immediate; repeated failures back off to 8 s.
  const delay = mediaRecoveryAttempt === 0 ? 0 : Math.min(8000, 1000 * 2 ** Math.min(mediaRecoveryAttempt - 1, 3));
  mediaRecoveryAttempt++;
  telemetryStatus.textContent = state.isRecording ? 'Call media lost · reconnecting · recording continues locally' : 'Call media lost · reconnecting…';
  mediaRecoveryTimer = window.setTimeout(() => {
    mediaRecoveryTimer = null;
    queueSignalingWork(negotiateMedia, () => scheduleMediaRecovery());
  }, delay);
}

export function sendParticipantState(): void {
  if (!state.callAdmitted) return;
  sendEnvelope({ t: TypeParticipantState, payload: { micMuted: callMic.callMuted, camOff: state.isCamOff } as ParticipantStatePayload });
}

export function clearRemoteTiles(): void {
 decodedRemoteVideos.clear();
 for (const tile of videoGrid.querySelectorAll<HTMLElement>('.tile[data-participant-id]')) tile.remove();
 screenShare.clearRemote();
 layoutSwitcher.applyLayout();
 updateCallParticipantCount();
}

export function clearRemoteMedia(): void {
 state.callAdmitted = false;
 if (mediaRecoveryTimer !== null) { clearTimeout(mediaRecoveryTimer); mediaRecoveryTimer = null; }
 if (mediaConnectWatchdog !== null) { clearTimeout(mediaConnectWatchdog); mediaConnectWatchdog = null; }
 clearRemoteTiles();
 rtc.close(); state.peerState = 'new'; updateConnectionPill();
}

export async function onIceCandidate(cand: IceCandidatePayload): Promise<void> {
  if (cand && cand.candidate) {
    await rtc.addIceCandidate({
      candidate: cand.candidate,
      sdpMid: cand.sdpMid,
      sdpMLineIndex: cand.sdpMLineIndex,
    });
  }
}

export async function onSdpOffer(offer: SdpPayload): Promise<void> {
  const answer=await rtc.handleOffer(offer.sdp);
  // null: stale offer for a peer the SFU is replacing after our re-offer.
  if(answer!==null)sendEnvelope({t:TypeSdpAnswer,payload:{sdp:answer,type:'answer'}});
}

export async function onSdpAnswer(ans: SdpPayload): Promise<void> {
  await rtc.handleAnswer(ans.sdp);
}

export function onParticipantState(remote: ParticipantStatePayload): void {
  if(!remote?.participantId||remote.participantId===state.participantId)return;
  participantStates.set(remote.participantId,remote);
  if(remote.name)participantNames.set(remote.participantId,remote.name);
  updateRemoteLabels();
}

export function updateRemoteLabels(): void {
  for (const tile of videoGrid.querySelectorAll<HTMLElement>('.tile[data-participant-id]')) {
    const name = participantNames.get(tile.dataset.participantId || '');
    const label = tile.querySelector<HTMLElement>('.tile-name');
    if (label) label.textContent = name || 'Remote source';
    renderTileState(tile, participantStates.get(tile.dataset.participantId || ''));
  }
  callNetwork.tilesChanged();
}

export function rememberParticipantNames(participants: Array<{id?:string;participantId?:string;name:string}> = []): void {
  if (!Array.isArray(participants)) return;
  for (const participant of participants) {
    const id = participant.participantId || participant.id;
    if (id && typeof participant.name === 'string') participantNames.set(id,participant.name);
  }
  updateRemoteLabels();
}

export async function refreshParticipantNames(): Promise<void> {
  try {
    // An invite alone only shows the waiting-room view; an admitted guest's
    // participant grant also lists display names [SL-SEC-1].
    const credential = state.isHost ? state.token : (state.uploadToken || state.token);
    const response = await fetch(`/api/v1/sessions/${encodeURIComponent(state.sessionId)}`,{headers:{Authorization:`Bearer ${credential}`}});
    if (response.ok) {const detail=await response.json();rememberParticipantNames(detail.participants);}
  } catch { /* Names remain unspecified until authorized metadata is available. */ }
}
