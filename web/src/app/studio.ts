// The studio (in-call) screen [SL-TECH-1]: admission and role, room state,
// session end, host/producer controls, the feature components that live in
// the call (call features, captions, livestream, settings, phone camera,
// teleprompter, branding) and leaving / rejoining.
import { Branding } from '../components/Branding';
import { CallFeatures } from '../components/CallFeatures';
import { Captions } from '../components/Captions';
import { showInviteDialog } from '../components/InviteDialog';
import { LeaveCall } from '../components/LeaveCall';
import { mountLivestream } from '../components/LivestreamPanel';
import { PhoneCamera } from '../components/PhoneCamera';
import { showProducerLinkDialog } from '../components/ProducerLink';
import { SettingsDrawer } from '../components/SettingsDrawer';
import { Teleprompter } from '../components/Teleprompter';
import { setHeading } from '../components/Home';
import { denialMessage } from '../lib/callstate';
import {
  type AdmissionPayload, type Envelope, RoleProducer, type RoomStatePayload, TypeChirp, TypeRecordStart,
  TypeRecordStop,
} from '../lib/protocol';
import {
  callNetwork, clearRemoteMedia, layoutSwitcher, negotiateMedia, refreshParticipantNames, resetMediaRecovery,
  screenShare, sendParticipantState,
} from './call';
import {
  chirpBtn, copyInviteBtn, diagnosticStatus, greenRoomSec, localNameTag, localVideo, nameInput, pcmToggle,
  postprocBtn, recBtn, recIndicator, roleSelect, sessionHealthAside, settingsBtn, studioSec,
  telemetryStatus, toggleCamBtn, toggleHealthBtn, toggleMicBtn, videoGrid, waitingPreviewVideo, waitingRoomSec,
} from './dom';
import { entry, preflight, sendPreflight, updateStorageQuota } from './greenroom';
import {
  applyHostMute, callMic, effects, openCameraAndMic, playSyncChirp, recordingMic, renderCamState, renderMicState,
  selfView, setupAudioMeter, stopAudioMeter,
} from './media';
import {
  recordersFlushed, releasePcmMic, stopLocalRecording, transfersModal, triggerPostProcess, uploadBacklog,
} from './recording';
import {
  blockReconnect, closeSignaling, detachSignaling, leaveSignaling, rejoinSignaling, resetReconnectBackoff,
  sendEnvelope,
} from './signaling';
import { canAssist, state } from './state';
import { showToast, updateResolutionSpec } from './ui';

export let livestream: ReturnType<typeof mountLivestream>;
export let settingsDrawer: SettingsDrawer;
export let callFeatures: CallFeatures;
export let captions: Captions;
export let phoneCamera: PhoneCamera;
export let teleprompter: Teleprompter;
export let branding: Branding;
export let leaveCall: LeaveCall;
let producerLinkBtn: HTMLButtonElement;
let leftDeviceIds: { video?: string; audio?: string } = {};

/** P3 livestream: host-only button + drawer; reads tiles from #video-grid and session/token from the URL. */
export function createLivestream(): void {
  livestream = mountLivestream(document.getElementById('more-list') as HTMLElement);
}

export function createSettingsDrawer(): void {
  settingsDrawer = new SettingsDrawer(state.sessionId, state.activeSettings, (newSettings) => {
    state.activeSettings = newSettings;
    updateResolutionSpec();
    showToast('Recording quality saved');
  }, state.token);

  if (settingsBtn) {
    settingsBtn.onclick = () => settingsDrawer.toggle();
  }
}

/** Call features (people, chat, hands, reactions, shortcuts) and live captions [SL-P4-2]. */
export function createCallFeatures(): void {
  callFeatures = new CallFeatures({
    send: env => sendEnvelope(env),
    selfId: () => state.participantId,
    isHost: () => state.isHost,
    isProducer: () => state.isProducer,
    toggleMic: () => toggleMicBtn.click(),
    toggleCam: () => toggleCamBtn.click(),
    hostMuteCall: by => applyHostMute(by),
    toggleCaptions: () => captions.toggle(),
    toast: msg => showToast(msg),
    netStats: id => callNetwork.netStats(id),
  });
  callFeatures.mount();
  // Live captions [SL-P4-2]: UI, host audio feed and transcript in the component.
  captions = new Captions({ send: env => sendEnvelope(env), selfId: () => state.participantId, isHost: () => state.isHost, sessionId: () => state.sessionId, token: () => state.token, toast: msg => showToast(msg) });
  captions.mount();
}

/** Phone as an extra camera (PRD F3) [SL-P1-9]: QR link, phone tiles. */
export function createPhoneCamera(): void {
  phoneCamera = new PhoneCamera({ sessionId: () => state.sessionId, grant: () => (state.callAdmitted ? state.uploadToken : ''), selfId: () => state.participantId, toast: msg => showToast(msg) }, videoGrid);
}

const productionRole = (): 'host' | 'producer' | 'guest' | '' => (!state.callAdmitted ? '' : state.isHost ? 'host' : state.isProducer ? 'producer' : 'guest');

/** Production extras [SL-P5-1, SL-P5-2]: teleprompter, branding and lower thirds. */
export function createProductionExtras(): void {
  teleprompter = new Teleprompter({ send: env => sendEnvelope(env), role: productionRole, toast: msg => showToast(msg) });
  teleprompter.mount();
  branding = new Branding({ send: env => sendEnvelope(env), role: productionRole, selfId: () => state.participantId, toast: msg => showToast(msg), session: () => ({ sessionId: state.sessionId, token: state.token }) });
  branding.mount();
}

/** room_state: the host's recording mic default, and a producer's Record button. */
export function onRoomState(st: RoomStatePayload): void {
  recordingMic.setSessionDefault(st.recordingMicMode);
  if (state.isHost && !state.isProducer) {
    // Rejoined mid-take [SL-P2-3]: the take keeps going without this page's
    // recorder (its earlier fragments upload from local storage). Show it and
    // let Record stop it; a take this page records follows its own recorder.
    if (state.recordState !== 'idle') { state.hostSeesUnrecordedTake = false; return; }
    const was = state.hostSeesUnrecordedTake;
    state.hostSeesUnrecordedTake = st.recording;
    if (!st.recording && !was) return;
    recBtn.disabled = false;
    recBtn.textContent = st.recording ? 'Stop recording' : 'Start recording';
    recIndicator.style.display = st.recording ? 'inline-flex' : 'none';
    document.getElementById('take-status-badge')!.textContent = st.recording ? `Recording ${st.takeNumber ?? ''} is running. You rejoined, so this device isn’t recorded.` : 'Not recording';
    return;
  }
  if (!state.isProducer) return;
  state.producerSeesRecording = st.recording;
  recBtn.disabled = false;
  recBtn.textContent = st.recording ? 'Stop recording' : 'Start recording';
  recIndicator.style.display = st.recording ? 'inline-flex' : 'none';
  document.getElementById('take-status-badge')!.textContent = st.recording ? `Recording ${st.takeNumber ?? ''} is running. You aren’t recorded.` : 'Not recording';
}

export async function onAdmission(adm: AdmissionPayload): Promise<void> {
  if (state.participantId && adm.participantId !== state.participantId) return;
  state.participantId = adm.participantId;
  resetReconnectBackoff();
  state.isHost = adm.role === 'host';
  const becameProducer = !state.isProducer && adm.role === RoleProducer && adm.status === 'admitted';
  state.isProducer = adm.role === RoleProducer;
  transfersModal.setToken(state.token,state.isHost);
  preflight.setHost(canAssist());
  roleSelect.value = state.isHost ? 'host' : 'guest';
  // Producers get Record/Stop and source health, never host management [SL-P1-11].
  recBtn.style.display = canAssist() ? 'inline-flex' : 'none';
  chirpBtn.style.display = postprocBtn.style.display = state.isHost ? 'inline-flex' : 'none';
  settingsBtn.style.display = copyInviteBtn.style.display = state.isHost ? 'inline-flex' : 'none';
  producerLinkBtn.style.display = state.isHost ? '' : 'none';
  toggleHealthBtn.style.display = canAssist() ? 'inline-flex' : 'none';
  if (!canAssist()) sessionHealthAside.style.display = 'none';
  document.body.classList.toggle('role-producer', state.isProducer);
  if (becameProducer) startAsProducer();
  callFeatures.onAdmission(adm);
  captions.onAdmission(adm);
  livestream.setHost(state.isHost && adm.status === 'admitted');
  if (adm.status === 'admitted') { entry.setInCall(state.isHost, () => state.sessionId, () => state.token); leaveCall.admitted(state.isHost); }
  if (adm.status === 'pending') {
    // Asking again after a denial: the heading waits for admission again.
    if (waitingRoomSec.dataset.denied) {
      delete waitingRoomSec.dataset.denied;
      setHeading(waitingRoomSec, 'Session access', 'Waiting for admission', 'The host must approve your request before the call begins.', 'Awaiting host');
    }
    waitingRoomSec.style.display = 'block';
    greenRoomSec.style.display = 'none';
    studioSec.style.display = 'none';
    if (waitingPreviewVideo) waitingPreviewVideo.srcObject = selfView();
    void updateStorageQuota();
    sendPreflight();
    telemetryStatus.innerText = 'Waiting for the host to let you in';
  } else if (adm.status === 'admitted') {
    waitingRoomSec.style.display = 'none';
    greenRoomSec.style.display = 'none';
    studioSec.style.display = 'flex';
    localVideo.srcObject = selfView();
    localNameTag.innerText = state.isProducer ? `${nameInput.value || 'You'} (Producer, not recorded)` : nameInput.value || 'You';
    layoutSwitcher.applyLayout();

    state.uploadToken = adm.uploadToken || '';
    void refreshParticipantNames();
    telemetryStatus.innerText = 'Connecting the call';
    showToast('You joined the meeting');

    state.admittedIceServers = adm.iceServers ?? [];
    state.callAdmitted = true;
    resetMediaRecovery();
    recBtn.disabled=state.recordState!=='idle' && state.recordState!=='recording';
    await negotiateMedia();
    callNetwork.start();
    sendParticipantState();
    sendPreflight();
    if(state.currentTakeId && (state.recordState==='recording'||state.recordState==='arming') && adm.activeTakeId!==state.currentTakeId) {
      void stopLocalRecording();
    }
  } else if (adm.status === 'denied') {
    blockReconnect();
    const denial = denialMessage(adm.reason);
    telemetryStatus.textContent=denial.status;
    // Behind the denial dialog, the waiting room no longer says "Waiting for admission" [SL-P5-4].
    if (waitingRoomSec.style.display === 'block') {
      waitingRoomSec.dataset.denied = adm.reason || 'declined';
      setHeading(waitingRoomSec, 'Session access', denial.status, denial.detail, adm.reason === 'room_locked' ? 'Locked' : 'Not admitted');
    }
    showToast(denial.detail);
    leaveCall.rejoinFailed();
    // Removed mid-call: stop sharing and stop capturing on this device.
    clearRemoteMedia();
    if (state.recordState==='recording'||state.recordState==='arming') void stopLocalRecording();
    if (diagnosticStatus) {
      diagnosticStatus.innerText = adm.reason==='room_locked' ? 'Call locked by host' : 'Admission declined by host';
      diagnosticStatus.style.color = 'var(--sl-danger)';
    }
  }
}

/** The host ended the meeting: stop reconnecting, drop call media, show the summary. */
export function onSessionEnd(): void {
  blockReconnect();
  clearRemoteMedia(); closeSignaling();
  if (state.recordState==='recording'||state.recordState==='arming') void stopLocalRecording();
  else if (state.recordState==='idle') state.localStream?.getTracks().forEach(t=>t.stop());
  if (!state.isHost) transfersModal.hide(); // the ended screen shows this device's uploads
  entry.sessionEnded(state.isHost);
  leaveCall.meetingEnded();
  captions.sessionEnded();
}

export function onChirp(): void {
  playSyncChirp();
  showToast('Sync chirp played');
}

/** Everything else goes to the feature components; each ignores types it doesn't own. */
export function dispatchToComponents(env: Envelope): void {
  if (callNetwork.handle(env)) return;
  screenShare.handle(env);
  callFeatures.handle(env);
  captions.handle(env);
  phoneCamera.handle(env);
  teleprompter.handle(env);
  branding.handle(env);
}

/** Host controls: record/stop (host and producer), sync chirp, editor export. */
export function bindStudioControls(): void {
  recBtn.onclick = () => {
    // A producer starts and stops takes without recording itself: its button
    // follows room_state, not local recorders [SL-P1-11].
    if(state.isProducer){
      recBtn.disabled=true;
      if(!state.producerSeesRecording)void preflight.confirmStart().then(ok=>{if(ok&&!state.producerSeesRecording)sendEnvelope({t:TypeRecordStart});else recBtn.disabled=false;});
      else sendEnvelope({t:TypeRecordStop});
      return;
    }
    if(!state.isHost)return;
    if(state.hostSeesUnrecordedTake&&state.recordState==='idle'){recBtn.disabled=true;sendEnvelope({t:TypeRecordStop});return;}
    // Red pre-flight items ask for confirmation; they never hard-block [SL-P0-5].
    if(state.recordState==='idle'){recBtn.disabled=true;void preflight.confirmStart().then(ok=>{if(ok&&state.recordState==='idle')sendEnvelope({t:TypeRecordStart});else recBtn.disabled=state.recordState!=='idle';});}
    else if(state.recordState==='recording') {recBtn.disabled=true;sendEnvelope({t:TypeRecordStop});}
  };

  chirpBtn.onclick = () => {
    sendEnvelope({ t: TypeChirp });
  };

  postprocBtn.onclick = () => {
    void triggerPostProcess(state.currentTakeId);
  };
}

/** Host: invite a producer (separate, rotatable link) [SL-P1-11]. */
export function createProducerLink(): void {
  producerLinkBtn = document.createElement('button');
  producerLinkBtn.id = 'producer-link-btn';
  producerLinkBtn.type = 'button';
  producerLinkBtn.className = 'btn more-item';
  producerLinkBtn.textContent = 'Invite producer';
  producerLinkBtn.style.display = 'none';
  producerLinkBtn.onclick = () => { if (state.isHost) void showProducerLinkDialog(state.sessionId, state.token, showToast); };
  document.getElementById('settings-btn')?.after(producerLinkBtn);
}

/** First admission as producer: heard, camera off until they turn it on, never recorded. */
function startAsProducer(): void {
  pcmToggle.checked = false;
  if (state.localStream && !state.isCamOff) {
    state.isCamOff = true;
    state.localStream.getVideoTracks().forEach(t => (t.enabled = false));
  }
  renderMicState();
  renderCamState();
  showToast('You’re in as producer: everyone hears you; your camera is off (turn it on any time). You are not recorded.');
}

/**
 * The meeting link (guest invite). Creating one replaces the previous link on
 * the host, so this page asks once and reuses it for Add others and Copy.
 */
let inviteUrl = '';
async function meetingLink(): Promise<string> {
  if (inviteUrl) return inviteUrl;
  const res = await fetch(`/api/v1/sessions/${state.sessionId}/invite`, { method: 'POST', headers: { Authorization: `Bearer ${state.token}` } });
  if (!res.ok) throw new Error(await res.text());
  inviteUrl = (await res.json()).inviteUrl;
  return inviteUrl;
}

/** Copy meeting link (ready card and meeting details): a status line for the caller. */
export async function copyMeetingLink(): Promise<string> {
  if (!state.isHost) return 'Only the host can share the meeting link.';
  let url: string;
  try { url = await meetingLink(); } catch (err) { return err instanceof Error ? err.message : String(err); }
  try { await navigator.clipboard.writeText(url); showToast('Meeting link copied'); return 'Meeting link copied.'; }
  catch { showInviteDialog(url); return 'Copy the link from the window that opened.'; }
}

/** What the call chrome needs from the app [SL-UX-2]. */
export function callChromeOptions(): { copyMeetingLink: () => Promise<string>; localStream: () => MediaStream | null; sessionId: () => string } {
  return { copyMeetingLink, localStream: () => state.localStream, sessionId: () => state.sessionId };
}

/** Host: Add others shows the meeting link with how to reach the host. */
export function bindInvite(): void {
  copyInviteBtn.onclick = async () => {
    if(!state.isHost){showToast('Only the host can add people');return;}
    try { showInviteDialog(await meetingLink()); }
    catch(err){showToast(err instanceof Error?err.message:String(err));}
  };
}

/**
 * Leave call for guests [SL-P1-6]: call media and signaling go; recorders
 * finalize and uploads keep draining while the tab stays open. Rejoin uses the
 * in-memory resume grant, so the host is not asked to admit again.
 */
export function createLeaveCall(): void {
  leaveCall = new LeaveCall({
    isHost: () => state.isHost,
    toast: msg => showToast(msg),
    backlog: () => uploadBacklog(),
    leave: async () => {
      leaveSignaling();
      if (screenShare.presenting) await screenShare.stop('user').catch(() => {});
      if (state.recordState === 'arming' || state.recordState === 'recording') void stopLocalRecording();
      transfersModal.hide();
      detachSignaling();
      clearRemoteMedia();
      telemetryStatus.textContent = 'You left the call';
      // Release the camera and mic once every recorder has flushed its last chunk.
      await recordersFlushed();
      leftDeviceIds = { video: state.localStream?.getVideoTracks()[0]?.getSettings().deviceId, audio: state.localStream?.getAudioTracks()[0]?.getSettings().deviceId };
      stopAudioMeter();
      await effects.stop();
      state.localStream?.getTracks().forEach(t => t.stop());
      callMic.release();
      if (state.recordState === 'idle') releasePcmMic();
      callNetwork.kick();
    },
    rejoin: async () => {
      const res = await fetch(`/api/v1/sessions/${encodeURIComponent(state.sessionId)}`, { headers: { Authorization: `Bearer ${state.token}` } }).catch(() => null);
      if (!res) throw new Error('the host can’t be reached. Check your connection and try again.');
      if (!res.ok) return 'ended';
      if (!state.localStream?.getTracks().some(t => t.readyState === 'live')) {
        const { video, audio } = leftDeviceIds;
        const fresh = await openCameraAndMic(video, audio);
        fresh.getVideoTracks().forEach(t => (t.enabled = !state.isCamOff));
        fresh.getAudioTracks().forEach(t => (t.enabled = !state.isMicMuted));
        state.localStream = fresh;
        localVideo.srcObject = fresh;
        setupAudioMeter(fresh);
        await effects.cameraChanged();
      }
      rejoinSignaling();
      return 'connecting';
    },
  });
  leaveCall.mount(document.querySelector('#studio-view .dock') as HTMLElement);
}
