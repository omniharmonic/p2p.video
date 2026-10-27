// Green room and waiting room [SL-TECH-1]: meeting entry, producer-link
// detection, onboarding, device selects, the 10-second diagnostic clip, the
// storage estimate, joining, and the pre-flight "ready to record" checklist.
import { canSelectSpeaker } from '../components/DevicePicker';
import { MeetingEntry, rememberName, rememberedName } from '../components/Home';
import { HOME_ICONS } from '../components/homeIcons';
import { ICONS } from '../components/icons';
import { OnboardingWizard } from '../components/OnboardingWizard';
import { Preflight } from '../components/Preflight';
import { type BrowserCapabilities, browserNote, detectEngine, noteLimitationIds } from '../lib/browsersupport';
import { recordingMicLabel } from '../lib/micmode';
import { OPFSStorage } from '../lib/opfs';
import { expectedBytesPerSecond } from '../lib/preflight';
import { type PreflightPayload, RoleProducer, TypePreflight } from '../lib/protocol';
import { rememberParticipantNames, rtc } from './call';
import {
  cameraSelect, chirpBtn, copyInviteBtn, diagnosticClearBtn, diagnosticPlaybackContainer, diagnosticPlaybackVideo,
  diagnosticProgressBar, diagnosticProgressContainer, diagnosticRecBtn, diagnosticStatus, displaySessionTitle,
  greenRoomSec, joinBtn, localVideo, micSelect, nameInput, pcmOptionStatus, pcmToggle, postprocBtn, previewVideo,
  recBtn, roleSelect, settingsBtn, studioSec, toggleHealthBtn, waitingPreviewVideo, waitingRoomSec,
  waitingStorageQuota, wizardBtn,
} from './dom';
import {
  callMic, devicePicker, initDevices, recordingMic, recordingMicFor, renderCamState, renderMicState, selfView,
  showDeviceError, startPreview, stopPreview,
} from './media';
import { pcmMicStream, recoveryBanner, transfersModal } from './recording';
import { openSignalingConnection, sendEnvelope } from './signaling';
import { state } from './state';
import { settingsDrawer } from './studio';
import { showToast } from './ui';

export let entry: MeetingEntry;
export let onboardingWizard: OnboardingWizard;
export let preflight: Preflight;

/**
 * Gentle browser recommendation [SL-P1-12]: Safari/Firefox guests see which
 * specific features their browser can't offer (feature-detected), and only
 * when something is actually missing. Chromium never sees it.
 */
export async function showBrowserNote(): Promise<void> {
  // Guests only: the host dashboard may run in the Mac app's WebKit view.
  if (location.pathname !== '/join') return;
  const ua = navigator.userAgent;
  const engine = detectEngine(ua);
  if (engine === 'chromium') return;
  let listsOutputs = false;
  try { listsOutputs = (await navigator.mediaDevices.enumerateDevices()).some(d => d.kind === 'audiooutput'); } catch { /* none */ }
  const caps: BrowserCapabilities = {
    engine,
    mobile: /iPhone|iPad|iPod|Android|Mobile/.test(ua) || (navigator.maxTouchPoints > 1 && /Macintosh/.test(ua) && 'ontouchend' in document),
    setSinkId: canSelectSpeaker(),
    listsOutputs,
    displayMedia: typeof navigator.mediaDevices?.getDisplayMedia === 'function',
    audioWorklet: typeof AudioWorkletNode !== 'undefined',
    mediaRecorder: typeof MediaRecorder !== 'undefined',
    storage: await OPFSStorage.getInstance().backend(),
  };
  const text = browserNote(caps);
  let note = document.getElementById('browser-note');
  if (!text) { note?.remove(); return; }
  if (!note) {
    note = document.createElement('p');
    note.id = 'browser-note';
    note.className = 'producer-note browser-note'; // the green room's note style
    note.setAttribute('role', 'note');
    note.dataset.limitations = '';
    joinBtn.before(note);
  }
  note.dataset.limitations = noteLimitationIds(caps).join(' ');
  note.textContent = text;
}

/** Initial page state before anyone joins. */
export function initGreenRoom(): void {
  if(typeof AudioWorkletNode==='undefined'){pcmToggle.disabled=true;pcmOptionStatus.textContent='Studio-quality audio isn’t available in this browser.';}

  roleSelect.value = location.pathname === '/join' ? 'guest' : 'host';
  roleSelect.disabled = true;
  recBtn.style.display = chirpBtn.style.display = postprocBtn.style.display = settingsBtn.style.display = copyInviteBtn.style.display = toggleHealthBtn.style.display = 'none';
  if (!nameInput.value) nameInput.value = rememberedName();
  initReadyControls();
}

// ------------------------------------------------------------ Ready to join
// The preview's round mic / camera / background buttons, the live sound ring
// on the mic button, the speaker chip and the avatar shown with the camera
// off [SL-UX-2a]. Muting here carries into the call (and the recording).

const READY_ICONS: Record<string, string> = {
  mic: ICONS.mic, micOff: ICONS.micOff, cam: ICONS.cam, camOff: ICONS.camOff, sparkle: HOME_ICONS.sparkle,
  speaker: HOME_ICONS.speaker, chevronDown: HOME_ICONS.chevronDown, back: HOME_ICONS.back,
};
const readyEl = <T extends HTMLElement>(id: string) => document.getElementById(id) as T | null;

function renderReadyButtons(): void {
  const mic = readyEl<HTMLButtonElement>('ready-mic-btn'), cam = readyEl<HTMLButtonElement>('ready-cam-btn');
  if (mic) {
    mic.setAttribute('aria-pressed', String(state.isMicMuted));
    mic.setAttribute('aria-label', state.isMicMuted ? 'Turn on microphone' : 'Turn off microphone');
    mic.title = mic.getAttribute('aria-label')!;
    mic.querySelector('.ui-icon')!.innerHTML = state.isMicMuted ? ICONS.micOff : ICONS.mic;
  }
  if (cam) {
    cam.setAttribute('aria-pressed', String(state.isCamOff));
    cam.setAttribute('aria-label', state.isCamOff ? 'Turn on camera' : 'Turn off camera');
    cam.title = cam.getAttribute('aria-label')!;
    cam.querySelector('.ui-icon')!.innerHTML = state.isCamOff ? ICONS.camOff : ICONS.cam;
  }
  greenRoomSec.classList.toggle('is-cam-off', state.isCamOff);
  greenRoomSec.classList.toggle('is-mic-off', state.isMicMuted);
}

/** A new preview stream starts with live tracks: keep the person's choices. */
function applyReadyToggles(): void {
  const stream = state.localStream;
  if (!stream) return;
  stream.getVideoTracks().forEach(t => (t.enabled = !state.isCamOff));
  if (state.isMicMuted) callMic.setSelfMuted(true, [stream, pcmMicStream]);
}

function renderInitial(): void {
  const initial = readyEl('preview-initial');
  if (!initial) return;
  const name = nameInput.value.trim();
  initial.textContent = name ? [...name][0].toUpperCase() : '';
  initial.classList.toggle('is-empty', !name);
}

async function fillSpeakers(): Promise<void> {
  const chip = readyEl('speaker-chip'), select = readyEl<HTMLSelectElement>('speaker-select');
  if (!chip || !select || !canSelectSpeaker()) return;
  let outputs: MediaDeviceInfo[] = [];
  try { outputs = (await navigator.mediaDevices.enumerateDevices()).filter(d => d.kind === 'audiooutput' && d.label); } catch { /* none */ }
  chip.hidden = !outputs.length;
  const current = select.value;
  select.replaceChildren(...outputs.map(d => Object.assign(document.createElement('option'), { value: d.deviceId, textContent: d.label })));
  if ([...select.options].some(o => o.value === current)) select.value = current;
}

/** The live sound ring: the mic button follows the level meter (a static level with reduced motion). */
function runSoundRing(): void {
  const mic = readyEl<HTMLButtonElement>('ready-mic-btn'), bar = readyEl('vu-bar-l');
  if (!mic || !bar) return;
  let smooth = 0;
  const tick = () => {
    if (state.hasJoined) { mic.style.removeProperty('--level'); return; }
    if (greenRoomSec.offsetParent !== null) {
      const raw = state.isMicMuted ? 0 : Math.min(1, (parseFloat(bar.style.width) || 0) / 70);
      smooth = raw > smooth ? raw : smooth * 0.86 + raw * 0.14;
      mic.style.setProperty('--level', smooth.toFixed(3));
      mic.dataset.speaking = String(smooth > 0.18);
    }
    requestAnimationFrame(tick);
  };
  requestAnimationFrame(tick);
}

function initReadyControls(): void {
  for (const slot of document.querySelectorAll<HTMLElement>('[data-ready-icon]')) slot.innerHTML = READY_ICONS[slot.dataset.readyIcon!] ?? '';
  readyEl('ready-mic-btn')?.addEventListener('click', () => {
    if (!state.localStream || state.hasJoined) return;
    state.isMicMuted = !state.isMicMuted;
    callMic.setSelfMuted(state.isMicMuted, [state.localStream, pcmMicStream]);
    renderMicState();
    renderReadyButtons();
  });
  readyEl('ready-cam-btn')?.addEventListener('click', () => {
    if (!state.localStream || state.hasJoined) return;
    state.isCamOff = !state.isCamOff;
    state.localStream.getVideoTracks().forEach(t => (t.enabled = !state.isCamOff));
    renderCamState();
    renderReadyButtons();
  });
  const bgBtn = readyEl<HTMLButtonElement>('ready-bg-btn'), bgPanel = readyEl('ready-bg-panel');
  bgBtn?.addEventListener('click', () => {
    if (!bgPanel) return;
    bgPanel.hidden = !bgPanel.hidden;
    bgBtn.setAttribute('aria-expanded', String(!bgPanel.hidden));
    if (!bgPanel.hidden) bgPanel.querySelector<HTMLElement>('[aria-checked="true"]')?.focus();
  });
  bgPanel?.addEventListener('keydown', e => {
    if (e.key === 'Escape' && bgPanel && !bgPanel.hidden) { e.stopPropagation(); bgPanel.hidden = true; bgBtn?.setAttribute('aria-expanded', 'false'); bgBtn?.focus(); }
  });
  readyEl<HTMLSelectElement>('speaker-select')?.addEventListener('change', e => devicePicker?.presetSpeaker((e.target as HTMLSelectElement).value));
  // Device lists refill after permission and on device changes; the speaker list follows.
  new MutationObserver(() => void fillSpeakers()).observe(micSelect, { childList: true });
  navigator.mediaDevices?.addEventListener?.('devicechange', () => void fillSpeakers());
  nameInput.addEventListener('input', renderInitial);
  renderInitial();
  renderReadyButtons();
  runSoundRing();
}

/** Call-first entry: host home + rooms, or a room guest's wait for the host [SL-P1-5]. */
export function createMeetingEntry(): void {
  entry = new MeetingEntry({
    toast: m => showToast(m), joinButton: joinBtn, stream: () => state.localStream, joinRole: () => markProducerGreenRoom(),
    // The host's camera opens on Ready to join, not while browsing the home [SL-UX-2].
    onReadyShown: () => { if (!state.localStream && !state.hasJoined) void initDevices().catch(() => {}); },
    onHomeShown: () => stopPreview(),
  });
  // A session producer link (/join?session=…&token=…) is recognized from the session detail.
  if (location.pathname === '/join' && state.sessionId && state.token) {
    void fetch(`/api/v1/sessions/${encodeURIComponent(state.sessionId)}`, { headers: { Authorization: `Bearer ${state.token}` } })
      .then(r => (r.ok ? r.json() : null)).then(d => { if (d?.joinRole === RoleProducer) markProducerGreenRoom(); }).catch(() => {});
  }
}

/** The green room of a producer link: never recorded, camera optional [SL-P1-11]. */
function markProducerGreenRoom(): void {
  if (state.joiningAsProducer && document.getElementById('producer-note')) return;
  state.joiningAsProducer = true;
  const note = document.createElement('p');
  note.id = 'producer-note';
  note.className = 'producer-note';
  note.textContent = 'You’re joining as a producer: everyone can hear you, your camera starts off (turn it on if you like), and nothing of yours is recorded. You can let guests in, start and stop recording, and see how everyone’s connection and recording are doing.';
  document.getElementById('join-btn')?.before(note);
  pcmToggle.checked = false;
  pcmToggle.closest('label')?.setAttribute('hidden', '');
  // Nothing of a producer's is recorded: no recording options or headphones question.
  document.getElementById('recording-options')?.setAttribute('hidden', '');
  document.getElementById('ready-headphones')?.setAttribute('hidden', '');
  queueMicrotask(() => { recordingMic.greenRoom.hidden = true; });
}

export function createOnboarding(): void {
  onboardingWizard = new OnboardingWizard({
    onComplete: () => {
      showToast('You’re set up. Welcome to p2p.video.');
    },
    onRequestPermissions: async () => {
      if (state.hasJoined) return !!state.localStream?.active;
      try {
        await initDevices();
        return true;
      } catch {
        return false;
      }
    },
  });

  if (wizardBtn) {
    wizardBtn.onclick = () => onboardingWizard.show();
  }
}

/** Choosing another camera or microphone reopens the preview (before joining). */
export function bindDeviceSelects(): void {
  const restart = () => void startPreview().then(applyReadyToggles).catch(showDeviceError);
  cameraSelect.onchange = restart;
  micSelect.onchange = restart;
  // The background picker opens from the preview's background button.
  const bg = document.querySelector('#green-room .effects-row');
  if (bg) document.getElementById('ready-bg-panel')?.append(bg);
}

// 2. Join Studio & Waiting Room State Machine
export async function updateStorageQuota(): Promise<void> {
  if (navigator.storage && navigator.storage.estimate) {
    try {
      const est = await navigator.storage.estimate();
      const quotaGB = ((est.quota || 0) / (1024 * 1024 * 1024)).toFixed(1);
      const usageMB = ((est.usage || 0) / (1024 * 1024)).toFixed(1);
      const availGB = (Math.max(0, (est.quota || 0) - (est.usage || 0)) / (1024 * 1024 * 1024)).toFixed(1);
      if (waitingStorageQuota) {
        waitingStorageQuota.innerText = `${availGB} GB free for recordings (${usageMB} MB used of ${quotaGB} GB)`;
      }
    } catch {
      if (waitingStorageQuota) waitingStorageQuota.innerText = 'This browser didn’t say how much space is free.';
    }
  } else {
    if (waitingStorageQuota) waitingStorageQuota.innerText = 'This browser doesn’t report free space.';
  }
}

// 10-Second Hardware Diagnostic Recorder
let diagRecorder: MediaRecorder | null = null;
let diagChunks: Blob[] = [];
let diagTimer: number | null = null;
let diagnosticUrl = '';

/** A take is starting: the diagnostic clip recorder gives way. */
export function stopDiagnosticClip(): void {
  if (diagRecorder?.state === 'recording') diagRecorder.stop();
  if (diagTimer) {clearInterval(diagTimer);diagTimer=null;}
}

export function bindDiagnostics(): void {
  if (diagnosticRecBtn) {
    diagnosticRecBtn.onclick = () => {
      if (!state.localStream || state.recordState !== 'idle') {
        showToast('A test clip needs a working camera and mic, and no recording in progress');
        return;
      }
      diagChunks = [];
      try {
        diagRecorder = new MediaRecorder(state.localStream);
      } catch (e) {
        showToast('Couldn’t start the test clip: ' + e);
        return;
      }

      diagRecorder.ondataavailable = (e) => {
        if (e.data.size > 0) diagChunks.push(e.data);
      };

      diagRecorder.onstop = () => {
        const blob = new Blob(diagChunks, { type: diagRecorder?.mimeType || '' });
        if (diagnosticUrl) URL.revokeObjectURL(diagnosticUrl);
        const url = diagnosticUrl = URL.createObjectURL(blob);
        if (diagnosticPlaybackVideo) {
          diagnosticPlaybackVideo.src = url;
        }
        if (diagnosticPlaybackContainer) diagnosticPlaybackContainer.style.display = 'block';
        if (diagnosticStatus) {
          diagnosticStatus.innerText = 'Test clip ready';
          diagnosticStatus.style.color = 'var(--sl-ok)';
        }
        diagnosticRecBtn.disabled = false;
        diagnosticRecBtn.innerText = 'Record another test';
        if (diagnosticProgressContainer) diagnosticProgressContainer.style.display = 'none';
        showToast('Test clip ready. Play it back below.');
      };

      diagRecorder.start();
      diagnosticRecBtn.disabled = true;
      diagnosticRecBtn.innerText = 'Recording a test…';
      if (diagnosticProgressContainer) diagnosticProgressContainer.style.display = 'block';
      if (diagnosticProgressBar) diagnosticProgressBar.style.width = '0%';
      if (diagnosticStatus) diagnosticStatus.innerText = 'Recording…';

      let sec = 0;
      diagTimer = window.setInterval(() => {
        sec++;
        if (diagnosticProgressBar) diagnosticProgressBar.style.width = `${(sec / 10) * 100}%`;
        if (diagnosticStatus) diagnosticStatus.innerText = `Recording ${sec} of 10 s`;
        if (sec >= 10) {
          if (diagTimer) clearInterval(diagTimer);
          if (diagRecorder && diagRecorder.state === 'recording') {
            diagRecorder.stop();
          }
        }
      }, 1000);
    };
  }

  if (diagnosticClearBtn) {
    diagnosticClearBtn.onclick = () => {
      if (diagnosticUrl) { URL.revokeObjectURL(diagnosticUrl); diagnosticUrl = ''; }
      if (diagnosticPlaybackVideo) {
        diagnosticPlaybackVideo.pause();
        diagnosticPlaybackVideo.src = '';
      }
      if (diagnosticPlaybackContainer) diagnosticPlaybackContainer.style.display = 'none';
      if (diagnosticStatus) {
        diagnosticStatus.innerText = 'Ready to test';
        diagnosticStatus.style.color = '';
      }
      if (diagnosticProgressBar) diagnosticProgressBar.style.width = '0%';
    };
  }
}

/** Join: open (or look up) the session, lock devices, then connect signaling. */
export function bindJoin(): void {
  joinBtn.onclick = async () => {
    if (!state.localStream) {showToast('Choose working camera and microphone inputs first');return;}
    joinBtn.disabled = true;
    try {
      if (!state.sessionId) {
        const opened = await entry.openSession(nameInput.value.trim());
        state.sessionId=opened.sessionId;state.token=opened.token;
        if (opened.host) {
          const u=new URL(location.href);u.searchParams.delete('room');u.searchParams.set('session',state.sessionId);u.searchParams.set('token',state.token);history.replaceState(null,'',u);
          recoveryBanner.setToken(state.token);
          transfersModal.setToken(state.token,true);
        }
        settingsDrawer.setSession(state.sessionId,state.token);
      }
      const sres = await fetch(`/api/v1/sessions/${state.sessionId}`,{headers:{Authorization:`Bearer ${state.token}`}});
      if (!sres.ok) throw new Error('This session is unavailable or the invite has expired.');
      const data = await sres.json();
      if (data.settings) state.activeSettings={...state.activeSettings,...data.settings};
      settingsDrawer.setSettings(state.activeSettings);
      displaySessionTitle.textContent=data.session.title;
      rememberParticipantNames(data.participants);
      // A producer link: in the call, never recorded [SL-P1-11].
      state.joiningAsProducer = data.joinRole === RoleProducer;
      if (state.joiningAsProducer) { pcmToggle.checked = false; recordingMic.greenRoom.hidden = true; }
      // The host's recording mic default applies before we record anything [SL-P1-10].
      recordingMic.setSessionDefault(data.settings?.recording_mic_mode);
      recordingMic.freezeHeuristic();
      state.hasJoined=true;cameraSelect.disabled=true;micSelect.disabled=true;pcmToggle.disabled=true;
      rememberName(nameInput.value.trim());entry.leaveHome();greenRoomSec.style.display='none';waitingRoomSec.style.display='block';studioSec.style.display='none';
      if(waitingPreviewVideo)waitingPreviewVideo.srcObject=selfView();
      openSignalingConnection();
    }catch(err){if(!MeetingEntry.isCancel(err))showToast(err instanceof Error?err.message:String(err));joinBtn.disabled=false;}
  };
}

/**
 * Pre-flight "ready to record" [SL-P0-5]: green-room panel, studio chip, and
 * the host's aggregate of every participant's checklist via the dashboard.
 */
export function createPreflight(): void {
  preflight = new Preflight({
    getStream: () => state.localStream,
    getPreviewVideo: () => [localVideo, previewVideo, waitingPreviewVideo].find(v => v && v.offsetParent !== null) ?? null,
    getPeer: () => rtc.pc,
    bytesPerSecond: () => expectedBytesPerSecond(state.activeSettings.video_bitrate_bps, pcmToggle.checked ? { sampleRate: state.activeSettings.audio_sample_rate, channels: state.localStream?.getAudioTracks()[0]?.getSettings().channelCount ?? 1 } : undefined),
    inCall: () => state.callAdmitted,
    isRecording: () => state.recordState !== 'idle',
    onReport: items => {
      // Before joining, the headphone label hint pre-checks "I'm wearing headphones" [SL-P1-10].
      const hp = items.find(i => i.id === 'headphones');
      if (hp && !state.hasJoined) recordingMic.setHeuristic(hp.status === 'ok');
      sendPreflight();
    },
    requestRefresh: items => sendEnvelope({ t: TypePreflight, payload: { refresh: true, items } }),
    extraItems: () => (state.isProducer || state.joiningAsProducer || !state.localStream) ? [] : [{ id: 'recmic', label: 'Recording mic', status: 'info', detail: recordingMicLabel(recordingMicFor(state.localStream.getAudioTracks()[0])) }],
    recordsLocally: () => !state.isProducer,
  });
  // Under the join button: joining comes first; readiness is about recording [SL-UX-2a].
  preflight.mountPanel(document.getElementById('ready-readiness') ?? joinBtn.parentElement!, null);
  const studioTools = document.querySelector<HTMLElement>('.studio-tools');
  if (studioTools) preflight.mountChip(studioTools);
  preflight.start();
}

export function sendPreflight(): void {
  if (!preflight.items.length) return;
  // The host sees each participant's recording mic mode (producers aren't recorded).
  const recordingMic = state.isProducer || state.joiningAsProducer || !state.localStream ? undefined : recordingMicFor(state.localStream.getAudioTracks()[0]);
  sendEnvelope({ t: TypePreflight, payload: { items: preflight.items, ...(recordingMic ? { recordingMic } : {}) } });
}

/** The host is about to record: re-check now and always report. */
export function onPreflightRequest(request: PreflightPayload): void {
  if(request?.refresh&&!state.isHost)void preflight.run().then(sendPreflight);
}
