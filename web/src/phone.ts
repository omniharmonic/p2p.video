// Phone camera page (PRD F3) [SL-P1-9]. Opened from the QR code a participant
// showed in the call: joins as that participant's extra source (role
// "subsource"), publishes its camera to the call (video only: the phone's mic
// would echo next to its participant), records camera + room mic locally in
// every take and uploads under that participant. Minimal UI: full-screen
// preview, front/back switch, recording/uploading state.
import './tokens.css';
import './phone.css';
import { ClockSync } from './lib/clock';
import { phoneStatusText, phoneVideoConstraints, PHONE_AUDIO_CONSTRAINTS, type PhoneState } from './lib/phonecam';
import {
  RoleSubsource, TypeAdmission, TypeClockPing, TypeClockPong, TypeHello, TypeIceCandidate, TypeParticipantState,
  TypeRecordStart, TypeRecordStop, TypeSdpAnswer, TypeSdpOffer, TypeSessionEnd,
  type AdmissionPayload, type ClockPongPayload, type Envelope, type HelloPayload, type IceCandidatePayload, type RecordStartPayload, type SdpPayload,
} from './lib/protocol';
import { LocalTrackRecorder } from './lib/recorder';
import { PeerClosedError, WebRTCClient } from './lib/rtc';
import { ProgressiveUploadQueue, type UploadProgress } from './lib/upload';
import { signalingRetryDelayMs } from './lib/backoff';
import { ICONS } from './components/icons';

const PHONE_VIDEO_BITRATE_BPS = 8_000_000;

const $ = <T extends HTMLElement>(id: string) => document.getElementById(id) as T;
const preview = $<HTMLVideoElement>('phone-preview');
const statusEl = $<HTMLElement>('phone-status');
const nameEl = $<HTMLElement>('phone-name');
const recPill = $<HTMLElement>('phone-rec');
const recLabel = $<HTMLElement>('phone-rec-label');
const progress = $<HTMLElement>('phone-progress');
const flipBtn = $<HTMLButtonElement>('phone-flip');
const startBtn = $<HTMLButtonElement>('phone-start');
const leaveBtn = $<HTMLButtonElement>('phone-leave');
const hint = $<HTMLElement>('phone-hint');
flipBtn.innerHTML = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 8.5h3l1.5-2h7l1.5 2h3v10H4z"/><path d="M9.5 13.5a2.5 2.5 0 0 1 4.6-1.4M14.5 13.5a2.5 2.5 0 0 1-4.6 1.4M14.6 10.6v1.6H13M9.4 16.4v-1.6H11"/></svg>`;
leaveBtn.innerHTML = ICONS.end;

// The capability works once; it is read, then dropped from the address bar.
const params = new URLSearchParams(location.search);
const sessionId = params.get('session') || '';
let credential = params.get('token') || '';
history.replaceState(null, '', location.pathname);

let stream: MediaStream | null = null;
let facing: 'user' | 'environment' = 'user';
let ws: WebSocket | null = null;
let grant = '';
let parentName = '';
let admitted = false;
let stopped = false;
let reconnectAttempt = 0;
let state: PhoneState = 'starting';
let chain = Promise.resolve();
let clockTimer: number | null = null;
const clock = new ClockSync();
let clockSamples = 0;
let wakeLock: { release(): Promise<void> } | null = null;

interface Take { takeId: string; recorder: LocalTrackRecorder; queue: ProgressiveUploadQueue; trackId: string; progress: UploadProgress | null; finishing?: Promise<boolean> }
let take: Take | null = null;
let pendingStart: number | null = null;
const finishing = new Set<Take>();

const rtc = new WebRTCClient(() => { /* publish-only: the SFU sends nothing back */ }, candidate => send({ t: TypeIceCandidate, payload: candidate }));
rtc.onMediaFailure = () => { if (admitted) void negotiate(); };

function send(env: Envelope): void { if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify(env)); }

function render(message?: string): void {
  const uploaded = [...finishing, ...(take ? [take] : [])].reduce((s, t) => s + (t.progress?.uploadedBytes ?? 0), 0);
  const pending = [...finishing, ...(take ? [take] : [])].reduce((s, t) => s + (t.progress?.bufferedBytes ?? 0), 0);
  statusEl.textContent = phoneStatusText({ state, uploadedBytes: uploaded, pendingBytes: pending, parentName, message });
  document.body.dataset.state = state;
  recPill.hidden = state !== 'recording' && state !== 'countdown';
  recLabel.textContent = state === 'countdown' ? 'Starting' : 'Recording';
  const total = uploaded + pending;
  progress.hidden = state !== 'uploading' || total === 0;
  (progress.firstElementChild as HTMLElement).style.width = `${total ? Math.round((uploaded / total) * 100) : 0}%`;
  flipBtn.disabled = !stream || !!take || state === 'ended';
  flipBtn.title = take ? 'The camera is locked while recording' : '';
  flipBtn.setAttribute('aria-label', facing === 'user' ? 'Switch to the back camera' : 'Switch to the front camera');
  hint.textContent = take ? 'Camera switching is locked while recording.' : finishing.size ? 'Keep this page open until the upload finishes.' : 'Recording stays on this phone until it uploads to the host.';
}

function setState(next: PhoneState, message?: string): void { state = next; render(message); }

async function startCamera(): Promise<void> {
  stream = await navigator.mediaDevices.getUserMedia({ video: phoneVideoConstraints(facing), audio: PHONE_AUDIO_CONSTRAINTS });
  preview.srcObject = stream;
  preview.classList.toggle('is-mirrored', facing === 'user');
}

async function flip(): Promise<void> {
  if (!stream || take) return;
  const next = facing === 'user' ? 'environment' : 'user';
  flipBtn.disabled = true;
  try {
    const fresh = await navigator.mediaDevices.getUserMedia({ video: phoneVideoConstraints(next), audio: false });
    const track = fresh.getVideoTracks()[0];
    const old = stream.getVideoTracks()[0];
    stream.removeTrack(old);
    stream.addTrack(track);
    await rtc.replaceSenderTrack(old, track);
    old.stop();
    facing = next;
    preview.srcObject = stream;
    preview.classList.toggle('is-mirrored', facing === 'user');
  } catch (err) {
    statusEl.textContent = `Couldn’t switch camera: ${err instanceof Error ? err.message : String(err)}`;
  } finally { render(); }
}

function connect(): void {
  if (stopped) return;
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const socket = new WebSocket(`${proto}//${location.host}/api/v1/ws?role=${RoleSubsource}&session=${encodeURIComponent(sessionId)}&token=${encodeURIComponent(grant || credential)}`);
  ws = socket;
  chain = Promise.resolve();
  socket.onopen = () => {
    const hello: HelloPayload = { name: '', role: RoleSubsource as HelloPayload['role'], resumeToken: grant || undefined, capabilities: { opfs: !!navigator.storage?.getDirectory } };
    send({ t: TypeHello, payload: hello });
    if (clockTimer) clearInterval(clockTimer);
    clockTimer = window.setInterval(() => send({ t: TypeClockPing, payload: clock.createPing() }), 1000);
    send({ t: TypeClockPing, payload: clock.createPing() });
  };
  socket.onmessage = e => { chain = chain.then(() => (ws === socket ? handle(JSON.parse(e.data)) : undefined)).catch(err => { if (!(err instanceof PeerClosedError)) console.warn(err); }); };
  socket.onclose = () => {
    if (ws !== socket) return;
    if (clockTimer) { clearInterval(clockTimer); clockTimer = null; }
    rtc.close(); admitted = false;
    if (stopped) return;
    // The capability is spent: only the phone's own grant can reconnect.
    if (!grant) { setState('error', 'This phone link has expired or was already used. Scan a new code from your computer.'); return; }
    if (state !== 'recording' && state !== 'uploading') setState('reconnecting');
    window.setTimeout(connect, signalingRetryDelayMs(reconnectAttempt++));
  };
}

async function negotiate(): Promise<void> {
  if (!stream || !admitted) return;
  rtc.init(adminIce);
  rtc.addTracks(new MediaStream(stream.getVideoTracks()));
  const sdp = await rtc.createOffer();
  send({ t: TypeSdpOffer, payload: { sdp, type: 'offer' } as SdpPayload });
}
let adminIce: NonNullable<AdmissionPayload['iceServers']> = [];

async function handle(env: Envelope): Promise<void> {
  switch (env.t) {
    case TypeAdmission: {
      const a = env.payload as AdmissionPayload;
      if (a.status === 'denied') {
        stopped = true; ws?.close();
        finishTake();
        setState('ended', a.reason === 'removed' ? 'The host removed this phone camera. Anything recorded keeps uploading while this page is open.' : 'This phone link has expired or was already used. Scan a new code from your computer.');
        return;
      }
      if (a.status !== 'admitted') return;
      admitted = true; reconnectAttempt = 0;
      grant = a.uploadToken || grant; credential = '';

      parentName = (a.name || '').replace(/ · Phone$/, '');
      nameEl.textContent = a.name || 'Phone camera';
      adminIce = a.iceServers ?? [];
      await negotiate();
      send({ t: TypeParticipantState, payload: { micMuted: true, camOff: false } });
      if (!take && !pendingStart) setState('live');
      // A take is already recording: join it as a late source.
      if (a.activeTakeId && !take) void waitForClock().then(() => startRecording(a.activeTakeId!));
      return;
    }
    case TypeSdpAnswer: await rtc.handleAnswer((env.payload as SdpPayload).sdp); return;
    case TypeSdpOffer: {
      const answer = await rtc.handleOffer((env.payload as SdpPayload).sdp);
      if (answer !== null) send({ t: TypeSdpAnswer, payload: { sdp: answer, type: 'answer' } });
      return;
    }
    case TypeIceCandidate: {
      const c = env.payload as IceCandidatePayload;
      if (c?.candidate) await rtc.addIceCandidate({ candidate: c.candidate, sdpMid: c.sdpMid, sdpMLineIndex: c.sdpMLineIndex });
      return;
    }
    case TypeClockPong: clock.handlePong(env.payload as ClockPongPayload); clockSamples++; return;
    case TypeRecordStart: {
      const p = env.payload as RecordStartPayload;
      if (take) return;
      setState('countdown');
      const wait = Math.max(0, (p.startAtHostUs - clock.toHostUs(performance.now())) / 1000);
      pendingStart = window.setTimeout(() => { pendingStart = null; void startRecording(p.takeId); }, wait);
      return;
    }
    case TypeRecordStop:
      if (pendingStart !== null) { clearTimeout(pendingStart); pendingStart = null; setState('live'); }
      finishTake();
      return;
    case TypeSessionEnd:
      stopped = true; ws?.close();
      finishTake();
      setState('ended', 'The host ended the meeting. Keep this page open until the upload finishes.');
      return;
  }
}

function waitForClock(): Promise<void> {
  return new Promise(resolve => {
    const t0 = performance.now();
    const check = () => (clockSamples >= 3 || performance.now() - t0 > 3000 ? resolve() : window.setTimeout(check, 100));
    check();
  });
}

async function startRecording(takeId: string): Promise<void> {
  if (take || !stream) return;
  try {
    const mime = LocalTrackRecorder.preferredMimeType('video', { videoCodec: 'h264' });
    const res = await fetch(`/api/v1/takes/${encodeURIComponent(takeId)}/tracks`, { method: 'POST', headers: { Authorization: `Bearer ${grant}`, 'Content-Type': 'application/json' }, body: JSON.stringify({ kind: 'video', mime, label: 'Camera' }) });
    if (!res.ok) throw new Error((await res.text()).trim());
    const { id: trackId } = await res.json();
    const t = { takeId, trackId, progress: null } as unknown as Take;
    t.queue = new ProgressiveUploadQueue(takeId, trackId, grant, p => {
      t.progress = p;
      send({ t: 'recorder_status', payload: { trackId, state: p.error ? 'error' : p.isComplete ? 'done' : take === t ? 'recording' : 'uploading', bufferedBytes: p.bufferedBytes, uploadedBytes: p.uploadedBytes, lastSeq: p.lastSeq } });
      if (p.isComplete) finishing.delete(t);
      if (take !== t) setState(finishing.size ? 'uploading' : take ? 'recording' : 'done');
      else render();
    }, { kind: 'video' });
    await t.queue.ready();
    t.recorder = new LocalTrackRecorder(trackId, 'video', chunk => t.queue.enqueue(chunk), clock.getFit(), { engine: 'mediarecorder', videoBitrateBps: PHONE_VIDEO_BITRATE_BPS, videoCodec: 'h264', onError: err => setState('error', `Recording failed on this phone: ${err.message}`) });
    await t.recorder.start(stream);
    take = t;
    setState('recording');
  } catch (err) {
    setState(admitted ? 'live' : 'reconnecting');
    statusEl.textContent = `This phone couldn’t record this time: ${err instanceof Error ? err.message : String(err)}`;
  }
}

function finishTake(): void {
  const t = take;
  if (!t) return;
  take = null;
  finishing.add(t);
  setState('uploading');
  t.finishing = (async () => {
    const stoppedRec = await t.recorder.stop();
    return t.queue.complete(stoppedRec.lastSeq, stoppedRec.sidecar);
  })();
  void t.finishing.then(ok => {
    finishing.delete(t);
    if (!ok) setState('error', 'Part of this recording couldn’t upload. Keep this page open and reopen a phone link to retry.');
    else if (!take) setState(finishing.size ? 'uploading' : admitted ? 'done' : state === 'ended' ? 'ended' : 'done', state === 'ended' ? 'Recording uploaded. You can close this page.' : undefined);
  });
}

async function leave(): Promise<void> {
  if (take && !confirm('A take is recording. Leave and stop recording on this phone? What is recorded keeps uploading while this page is open.')) return;
  stopped = true;
  finishTake();
  ws?.close();
  rtc.close();
  if (!take && !finishing.size) stream?.getTracks().forEach(t => t.stop());
  setState('ended', finishing.size ? 'You left the call. Keep this page open until the upload finishes.' : 'You left the call. You can close this page.');
}

async function keepAwake(): Promise<void> {
  try {
    const nav = navigator as Navigator & { wakeLock?: { request(type: 'screen'): Promise<{ release(): Promise<void> }> } };
    wakeLock = await nav.wakeLock?.request('screen') ?? null;
  } catch { /* Not supported or not visible. */ }
}
document.addEventListener('visibilitychange', () => { if (document.visibilityState === 'visible' && !wakeLock) void keepAwake(); });
window.addEventListener('beforeunload', e => { if (take || finishing.size) { e.preventDefault(); e.returnValue = ''; } });

flipBtn.addEventListener('click', () => void flip());
leaveBtn.addEventListener('click', () => void leave());

async function boot(): Promise<void> {
  if (!sessionId || !credential) { setState('error', 'This page needs the link from the QR code. Scan it again from your computer.'); return; }
  setState('starting');
  try { await startCamera(); }
  catch {
    // iOS may need a tap before the camera opens.
    startBtn.hidden = false;
    setState('error', 'Allow camera and microphone access to use this phone as a camera.');
    await new Promise<void>(resolve => startBtn.addEventListener('click', () => resolve(), { once: true }));
    startBtn.hidden = true;
    try { await startCamera(); } catch (err) { setState('error', `Camera unavailable: ${err instanceof Error ? err.message : String(err)}`); return; }
  }
  void keepAwake();
  setState('connecting');
  connect();
}
void boot();
