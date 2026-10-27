// Async record link page [SL-P5-3] ("record solo"). A guest opens
// /record?link=<id>&token=<secret>: no call, no host. Green room (camera,
// microphone, microphone sound, uncompressed option, pre-flight), the host's
// script, then takes recorded locally at full quality with the same OPFS
// journal + progressive upload as a call (lib/recorder, lib/pcm-recorder,
// lib/upload). Review or delete takes, then send them. Reopening the link in
// the same browser resumes this browser's submission and its uploads.
//
// Recording reads only local capture tracks (AGENTS.md guardrail): there is
// no WebRTC on this page at all.
import './tokens.css';
import './record.css';
import { annotateSourceLinks } from './lib/brandmark';
import { captureAudioConstraints, openAudio } from './lib/callmic';
import { ClockSync } from './lib/clock';
import { loadRecordingMicMode, RECORDING_PROCESSING, recordingMicExplanation, recordingMicState, resolveRecordingMic, saveRecordingMicMode, isRecordingMicMode } from './lib/micmode';
import { OPFSStorage } from './lib/opfs';
import { LocalPcmRecorder } from './lib/pcm-recorder';
import { evaluateCamera, evaluateHeadphones, evaluateMic, evaluateOPFS, evaluatePersist, evaluateResolution, evaluateStorage, expectedBytesPerSecond, overallStatus } from './lib/preflight';
import { advance, clampSpeed, maxScroll, parseScript } from './lib/prompter';
import type { PreflightItem, RecordingMicMode, RecordingMicState } from './lib/protocol';
import { LocalTrackRecorder, type SidecarData } from './lib/recorder';
import {
  INITIAL_SOLO_STATE, RecordApiError, RecordGuestApi, canDiscard, canRecord, canSubmit, draftKey, expiryLabel, formatBytes, formatClock, formatLimit,
  linkErrorPhase, linkFromUrl, parseDraft, remainingSec, secretKey, soloReduce, uploadSummary,
  type RecordGuestSubmission, type RecordLinkPublic, type SavedDraft, type SoloEvent, type SoloState, type SoloTake,
} from './lib/recordlink';
import { ProgressiveUploadQueue, recoverTakeFromOPFS, type UploadProgress } from './lib/upload';

const $ = <T extends HTMLElement>(id: string) => document.getElementById(id) as T;
const el = {
  title: $('rec-title'), expiry: $('rec-expiry'), notice: $('rec-notice'), noticeTitle: $('rec-notice-title'), noticeBody: $('rec-notice-body'),
  main: $('rec-main'), preview: $<HTMLVideoElement>('rec-preview'), review: $<HTMLVideoElement>('rec-review'), reviewLabel: $('rec-review-label'),
  pill: $('rec-pill'), timer: $('rec-timer'), left: $('rec-left'), countdown: $('rec-countdown'), countdownNum: $('rec-countdown-num'),
  level: $('rec-level'), record: $<HTMLButtonElement>('rec-record'), recordLabel: $('rec-record-label'), backLive: $<HTMLButtonElement>('rec-back-live'),
  status: $('rec-status'), takes: $('rec-takes'), takesCount: $('rec-takes-count'), takesEmpty: $('rec-takes-empty'),
  submit: $<HTMLButtonElement>('rec-submit'), submitHint: $('rec-submit-hint'), scriptCard: $('rec-script-card'), script: $('rec-script'),
  scroll: $<HTMLInputElement>('rec-scroll'), speed: $<HTMLInputElement>('rec-speed'), ready: $('rec-ready'), name: $<HTMLInputElement>('rec-name'),
  camera: $<HTMLSelectElement>('rec-camera'), mic: $<HTMLSelectElement>('rec-mic'), micmode: $<HTMLSelectElement>('rec-micmode'),
  micmodeHint: $('rec-micmode-hint'), pcm: $<HTMLInputElement>('rec-pcm'), preflight: $('rec-preflight'), dialog: $<HTMLDialogElement>('rec-dialog'),
};

// ------------------------------------------------------------ credentials
// The secret leaves the address bar at once (history, screenshots, Referer);
// this tab keeps it for reloads. Reopening the link supplies it again.
const params = new URLSearchParams(location.search);
const fromUrl = linkFromUrl(location.search);
const linkId = fromUrl?.linkId ?? (params.get('link') || '');
let secret = fromUrl?.secret ?? '';
try {
  if (fromUrl) sessionStorage.setItem(secretKey(linkId), fromUrl.secret);
  else secret = sessionStorage.getItem(secretKey(linkId)) || '';
} catch { /* Private mode: the URL is the only copy. */ }
if (linkId) history.replaceState(null, '', `/record?link=${encodeURIComponent(linkId)}`);
const api = new RecordGuestApi(linkId, secret);

// ------------------------------------------------------------------ state
let state: SoloState = INITIAL_SOLO_STATE;
let link: RecordLinkPublic | null = null;
let draft: SavedDraft | null = null;
let grant = '';
const clock = new ClockSync();
let clockSamples = 0;
let camera: MediaStream | null = null;
let micStream: MediaStream | null = null;
let micState: RecordingMicState | null = null;
let peakDb: number | null = null;
let framesAdvancing: boolean | null = null;
let headphones = false;
let preflightItems: PreflightItem[] = [];
let wakeLock: { release(): Promise<void> } | null = null;

interface LocalTrack { trackId: string; kind: 'video' | 'audio'; mime: string; queue: ProgressiveUploadQueue; recorder: LocalTrackRecorder | LocalPcmRecorder; progress: UploadProgress | null; lastSeq?: number; done?: boolean }
interface LocalTake { takeId: string; tracks: LocalTrack[]; pcmStream: MediaStream | null; startedAt: number; reviewUrl?: string }
const local = new Map<string, LocalTake>();
let active: LocalTake | null = null;
let timerHandle: number | null = null;
let countdownHandle: number | null = null;
let reviewingTakeId = '';

function dispatch(e: SoloEvent): void { state = soloReduce(state, e); render(); }

// ------------------------------------------------------------------ render

function setNotice(title: string, body: string): void {
  el.notice.hidden = false; el.main.hidden = true;
  el.noticeTitle.textContent = title; el.noticeBody.textContent = body;
}

function render(): void {
  document.body.dataset.phase = state.phase;
  switch (state.phase) {
    case 'loading': return;
    case 'invalid': setNotice('This link doesn’t work any more', state.message || 'The host replaced or turned off this recording link. Ask them for the current one.'); stopDevices(); return;
    case 'expired': setNotice('This recording link has expired', 'Ask the host for a new link if you still want to record.'); stopDevices(); return;
    case 'full': setNotice('This link has all its recordings', 'The host has received as many recordings as this link accepts. Thank you!'); stopDevices(); return;
  }
  el.notice.hidden = true; el.main.hidden = false;
  const recording = state.phase === 'recording', counting = state.phase === 'countdown', stopping = state.phase === 'stopping';
  const submitted = state.phase === 'submitted', submitting = state.phase === 'submitting';
  // Transport.
  el.record.disabled = !(recording || counting || (canRecord(state) && devicesReady() && nameOk()));
  el.record.classList.toggle('is-recording', recording || counting);
  el.recordLabel.textContent = counting ? 'Cancel' : recording ? 'Stop recording' : stopping ? 'Saving…' : state.takes.length ? 'Record another' : 'Start recording';
  el.record.hidden = submitted || submitting;
  el.pill.hidden = !recording;
  el.left.hidden = !recording || !link;
  el.countdown.hidden = !counting;
  // Setup locks while a take is running and once the recording is sent.
  const locked = recording || counting || stopping || submitted || submitting;
  for (const c of [el.camera, el.mic, el.micmode, el.pcm, el.name]) c.disabled = locked;
  if (draft) el.name.disabled = true; // the name belongs to the submission
  // Takes.
  el.takesEmpty.hidden = state.takes.length > 0;
  el.takesCount.textContent = state.takes.length ? `${state.takes.length} kept` : '';
  // Upload progress re-renders the list often: keep keyboard focus on the same control.
  const focused = document.activeElement instanceof HTMLElement && el.takes.contains(document.activeElement) ? document.activeElement : null;
  const focusTake = focused?.closest<HTMLElement>('[data-take-id]')?.dataset.takeId, focusAct = focused?.dataset.act;
  el.takes.replaceChildren(...state.takes.map(takeRow));
  if (focusTake && focusAct) el.takes.querySelector<HTMLElement>(`[data-take-id="${CSS.escape(focusTake)}"] [data-act="${CSS.escape(focusAct)}"]`)?.focus();
  const up = uploadSummary(state.takes.filter(t => t.status !== 'recording'));
  el.submit.hidden = submitted;
  el.submit.disabled = !canSubmit(state);
  el.submit.textContent = submitting ? 'Sending…' : state.takes.length > 1 ? `Send ${state.takes.length} recordings to the host` : state.takes.length ? 'Send it to the host' : 'Send to the host';
  el.submitHint.textContent = submitted
    ? (up.allUploaded ? '' : `Uploading ${formatBytes(up.pendingBytes)}… keep this tab open until it finishes.`)
    : state.takes.length ? 'You can keep recording until you send. Uploads continue after you send.' : '';
  el.status.textContent = statusLine(up);
  document.body.classList.toggle('is-submitted', submitted);
  $('rec-done').hidden = !submitted;
  if (submitted) {
    $('rec-done-title').textContent = up.allUploaded ? 'Sent to the host' : 'Sent. Finishing the upload…';
    $('rec-done-body').textContent = up.allUploaded ? `Thank you${draft?.name ? `, ${draft.name}` : ''}. You can close this tab.` : `${formatBytes(up.pendingBytes)} to go. Keep this tab open.`;
  }
}

function nameOk(): boolean { return !!(draft?.name || el.name.value.trim()); }
function devicesReady(): boolean { return !!camera?.getVideoTracks()[0] && camera.getVideoTracks()[0].readyState === 'live' && !!micStream?.getAudioTracks()[0]; }

function statusLine(up: ReturnType<typeof uploadSummary>): string {
  if (state.message) return state.message;
  switch (state.phase) {
    case 'setup': return !devicesReady() ? 'Allow camera and microphone access to record.' : !nameOk() ? 'Add your name, then start recording.' : `Ready. Each recording can run up to ${formatLimit(link?.maxDurationSec ?? 0)}.`;
    case 'countdown': return 'Get ready…';
    case 'recording': return 'Recording on this device. It uploads to the host as you go.';
    case 'stopping': return 'Saving the recording…';
    case 'review': return up.pendingBytes > 0 ? `Uploading ${formatBytes(up.pendingBytes)} in the background.` : 'Play your recordings back, record another, or send them to the host.';
    case 'submitting': return 'Sending…';
    case 'submitted': return up.allUploaded ? 'Thank you! The host has everything.' : 'Thank you! Finishing the upload…';
    default: return '';
  }
}

function takeRow(t: SoloTake): HTMLLIElement {
  const li = document.createElement('li');
  li.className = 'rec-take';
  li.dataset.takeId = t.id;
  li.dataset.uploaded = String(t.uploaded);
  const total = t.uploadedBytes + t.pendingBytes;
  const pct = t.uploaded ? 100 : total ? Math.round((t.uploadedBytes / total) * 100) : 0;
  const head = document.createElement('div');
  head.className = 'rec-take-head';
  const name = document.createElement('strong');
  name.textContent = `Recording ${t.number}`;
  const meta = document.createElement('span');
  meta.className = 'rec-take-meta';
  meta.textContent = t.status === 'recording' ? 'Recording…' : `${formatClock(t.durationSec)} · ${t.error ? 'Upload paused' : t.uploaded ? 'Uploaded' : total ? `Uploading ${pct}%` : 'Waiting to upload'}`;
  head.append(name, meta);
  const bar = document.createElement('div');
  bar.className = 'rec-take-bar';
  bar.setAttribute('role', 'progressbar');
  bar.setAttribute('aria-label', `Recording ${t.number} upload`);
  bar.setAttribute('aria-valuenow', String(pct));
  bar.setAttribute('aria-valuemin', '0');
  bar.setAttribute('aria-valuemax', '100');
  const fill = document.createElement('span');
  fill.style.width = `${pct}%`;
  bar.append(fill);
  const actions = document.createElement('div');
  actions.className = 'rec-take-actions';
  const lt = local.get(t.id);
  if (lt && t.status !== 'recording') {
    const play = document.createElement('button');
    play.type = 'button'; play.className = 'btn btn-secondary btn-compact'; play.dataset.act = 'play';
    play.textContent = reviewingTakeId === t.id ? 'Playing' : 'Play';
    play.setAttribute('aria-label', `Play recording ${t.number}`);
    actions.append(play);
  }
  if (canDiscard(state, t.id)) {
    const del = document.createElement('button');
    del.type = 'button'; del.className = 'btn btn-quiet btn-compact'; del.dataset.act = 'delete';
    del.textContent = 'Delete';
    del.setAttribute('aria-label', `Delete recording ${t.number}`);
    actions.append(del);
  }
  li.append(head, bar);
  if (t.error) { const err = document.createElement('p'); err.className = 'rec-take-error'; err.textContent = t.error; li.append(err); }
  if (actions.childElementCount) li.append(actions);
  return li;
}

// ---------------------------------------------------------------- devices

async function listDevices(): Promise<void> {
  const devices = await navigator.mediaDevices.enumerateDevices().catch(() => [] as MediaDeviceInfo[]);
  const fill = (select: HTMLSelectElement, kind: MediaDeviceKind, current?: string) => {
    const list = devices.filter(d => d.kind === kind);
    select.replaceChildren(...list.map((d, i) => { const o = document.createElement('option'); o.value = d.deviceId; o.textContent = d.label || `${kind === 'videoinput' ? 'Camera' : 'Microphone'} ${i + 1}`; return o; }));
    if (current && list.some(d => d.deviceId === current)) select.value = current;
  };
  fill(el.camera, 'videoinput', camera?.getVideoTracks()[0]?.getSettings().deviceId);
  fill(el.mic, 'audioinput', micStream?.getAudioTracks()[0]?.getSettings().deviceId);
  headphones = evaluateHeadphones({ outputs: devices.filter(d => d.kind === 'audiooutput').map(d => d.label), input: micStream?.getAudioTracks()[0]?.label }).status === 'ok';
}

function micMode(): RecordingMicMode { return isRecordingMicMode(el.micmode.value) ? el.micmode.value : 'auto'; }
/** Nothing plays on this page while recording, so "automatic" is raw unless the host chose otherwise. */
function micDefault(): RecordingMicMode { const d = link?.recording.recording_mic_mode; return d === 'raw' || d === 'clean' ? d : 'raw'; }

async function openCamera(deviceId?: string): Promise<void> {
  camera?.getTracks().forEach(t => t.stop());
  camera = await navigator.mediaDevices.getUserMedia({ video: { ...(deviceId ? { deviceId: { exact: deviceId } } : {}), width: { ideal: 1920 }, height: { ideal: 1080 }, frameRate: { ideal: link?.recording.target_fps || 30 } }, audio: false });
  el.preview.srcObject = camera;
  watchFrames();
}

async function openMic(deviceId?: string): Promise<void> {
  micStream?.getTracks().forEach(t => t.stop());
  const { resolved } = resolveRecordingMic(micMode(), micDefault(), headphones);
  micStream = await openAudio(exact => captureAudioConstraints(deviceId, RECORDING_PROCESSING[resolved], exact));
  const track = micStream.getAudioTracks()[0];
  micState = recordingMicState(micMode(), micDefault(), headphones, track.getSettings());
  el.micmodeHint.textContent = recordingMicExplanation(resolved);
  meter(track);
}

let meterCtx: AudioContext | null = null;
let meterRaf = 0;
function meter(track: MediaStreamTrack): void {
  cancelAnimationFrame(meterRaf);
  void meterCtx?.close().catch(() => undefined);
  try {
    meterCtx = new AudioContext();
    const analyser = meterCtx.createAnalyser();
    analyser.fftSize = 1024;
    meterCtx.createMediaStreamSource(new MediaStream([track])).connect(analyser);
    const buf = new Float32Array(analyser.fftSize);
    let lastCheck = 0;
    const tick = (now: number) => {
      analyser.getFloatTimeDomainData(buf);
      let peak = 0;
      for (const v of buf) peak = Math.max(peak, Math.abs(v));
      const db = peak > 0 ? 20 * Math.log10(peak) : -100;
      peakDb = peakDb === null ? db : Math.max(db, peakDb - 0.5); // slow decay
      el.level.style.width = `${Math.max(0, Math.min(100, (db + 60) / 60 * 100))}%`;
      if (now - lastCheck > 1000) { lastCheck = now; void updatePreflight(); }
      meterRaf = requestAnimationFrame(tick);
    };
    meterRaf = requestAnimationFrame(tick);
    document.addEventListener('pointerdown', () => void meterCtx?.resume(), { once: true });
  } catch { /* No Web Audio: the level check stays "unmeasured". */ }
}

function watchFrames(): void {
  framesAdvancing = null;
  const video = el.preview;
  const t0 = video.currentTime;
  window.setTimeout(() => { framesAdvancing = video.currentTime > t0 || video.videoWidth > 0; void updatePreflight(); }, 1500);
}

async function updatePreflight(): Promise<void> {
  const v = camera?.getVideoTracks()[0], a = micStream?.getAudioTracks()[0];
  const bps = expectedBytesPerSecond(link?.recording.video_bitrate_bps || 8_000_000, el.pcm.checked ? { sampleRate: 48000, channels: 2 } : undefined);
  const est = await navigator.storage?.estimate?.().catch(() => null) ?? null;
  const persisted = await navigator.storage?.persisted?.().catch(() => null) ?? null;
  const storageBackend = await OPFSStorage.getInstance().backend();
  preflightItems = [
    evaluateCamera({ hasTrack: !!v, live: v?.readyState === 'live', enabled: v?.enabled ?? false, framesAdvancing }),
    evaluateMic({ hasTrack: !!a, live: a?.readyState === 'live', enabled: a?.enabled ?? false, peakDb }),
    evaluateResolution(v?.getSettings() ?? null),
    evaluateStorage(est, bps, Math.max(1, Math.ceil((link?.maxDurationSec ?? 600) / 60))),
    evaluateOPFS(storageBackend !== 'memory', OPFSStorage.getInstance().unavailableReason, storageBackend),
    evaluatePersist(persisted),
  ];
  if (!headphones) preflightItems.push({ id: 'headphones', label: 'Headphones', status: 'info', detail: 'Not needed here: nothing plays while you record' });
  const overall = overallStatus(preflightItems);
  el.ready.textContent = overall === 'ok' ? 'Ready to record' : overall === 'warn' ? 'Something to check' : 'Needs attention';
  el.ready.className = `badge ${overall === 'ok' ? 'badge-success' : 'badge-waiting'}`;
  el.preflight.replaceChildren(...preflightItems.map(it => {
    const li = document.createElement('li');
    li.className = 'rec-check'; li.dataset.status = it.status;
    const dot = document.createElement('span'); dot.className = 'status-dot'; dot.setAttribute('aria-hidden', 'true');
    const label = document.createElement('strong'); label.textContent = it.label;
    const detail = document.createElement('span'); detail.textContent = it.detail ?? '';
    li.append(dot, label, detail);
    return li;
  }));
  if (state.phase === 'setup' || state.phase === 'review') render();
}

function stopDevices(): void {
  if (active) return;
  camera?.getTracks().forEach(t => t.stop()); camera = null;
  micStream?.getTracks().forEach(t => t.stop()); micStream = null;
  cancelAnimationFrame(meterRaf);
}

// ----------------------------------------------------------------- script

let scrollRaf = 0;
function renderScript(text: string): void {
  const blocks = parseScript(text);
  el.scriptCard.hidden = !blocks.length;
  el.script.replaceChildren(...blocks.map(b => {
    const node = document.createElement(b.kind === 'h1' ? 'h3' : b.kind === 'h2' ? 'h4' : b.kind === 'gap' ? 'div' : 'p');
    node.className = `rec-script-${b.kind}`;
    node.textContent = b.text;
    return node;
  }));
}
function scrollScript(on: boolean): void {
  cancelAnimationFrame(scrollRaf);
  if (!on || !el.scroll.checked) return;
  let last = performance.now();
  const step = (now: number) => {
    const max = maxScroll(el.script.scrollHeight, el.script.clientHeight);
    el.script.scrollTop = advance(el.script.scrollTop, now - last, clampSpeed(Number(el.speed.value)), 18, max);
    last = now;
    scrollRaf = requestAnimationFrame(step);
  };
  scrollRaf = requestAnimationFrame(step);
}

// -------------------------------------------------------------- recording

async function syncClock(pings: number): Promise<void> {
  if (!draft) return;
  for (let i = 0; i < pings; i++) {
    const ping = clock.createPing();
    const pong = await api.clock(draft.submissionId, draft.key, ping.t0);
    clock.handlePong(pong);
    clockSamples++;
  }
}

function saveDraft(d: SavedDraft): void {
  draft = d;
  try { localStorage.setItem(draftKey(linkId), JSON.stringify(d)); } catch { /* This tab only. */ }
}

async function ensureSubmission(): Promise<void> {
  if (draft) return;
  const started = await api.start(el.name.value.trim());
  saveDraft({ submissionId: started.submission.id, key: started.key, name: started.submission.name });
  grant = started.submission.uploadToken;
  el.name.value = started.submission.name;
}

async function onRecordButton(): Promise<void> {
  if (state.phase === 'countdown') { cancelCountdown(); return; }
  if (state.phase === 'recording') { await stopTake(); return; }
  if (!canRecord(state)) return;
  if (!nameOk()) { el.name.focus(); return; }
  showLive();
  dispatch({ type: 'record' });
  try {
    await ensureSubmission();
    // A few fresh pings per take keep the host-clock fit current (6 the first time).
    await syncClock(clockSamples < 3 ? 6 : 3);
  } catch (err) { dispatch({ type: 'start-failed', message: errorText(err) }); return; }
  if ((state as SoloState).phase !== 'countdown') return; // cancelled meanwhile
  let n = 3;
  el.countdownNum.textContent = String(n);
  countdownHandle = window.setInterval(() => {
    n--;
    if (n > 0) { el.countdownNum.textContent = String(n); return; }
    if (countdownHandle) clearInterval(countdownHandle);
    countdownHandle = null;
    void beginTake();
  }, 1000);
}

function cancelCountdown(): void {
  if (countdownHandle) clearInterval(countdownHandle);
  countdownHandle = null;
  dispatch({ type: 'stop' });
}

async function registerTrack(takeId: string, kind: 'video' | 'audio', mime: string, label: string, take: LocalTake, deferredWavHeader = false): Promise<LocalTrack> {
  const res = await fetch(`/api/v1/takes/${encodeURIComponent(takeId)}/tracks`, { method: 'POST', headers: { Authorization: `Bearer ${grant}`, 'Content-Type': 'application/json' }, body: JSON.stringify({ kind, mime, label }) });
  if (!res.ok) throw new Error((await res.text()).trim() || `Couldn’t start recording (HTTP ${res.status})`);
  const { id } = await res.json() as { id: string };
  const track = { trackId: id, kind, mime, progress: null } as unknown as LocalTrack;
  track.queue = new ProgressiveUploadQueue(takeId, id, grant, p => { track.progress = p; reportProgress(take); }, { kind, deferredWavHeader });
  await track.queue.ready();
  take.tracks.push(track);
  return track;
}

function reportProgress(take: LocalTake): void {
  const uploaded = take.tracks.reduce((n, t) => n + (t.progress?.uploadedBytes ?? 0), 0);
  const pending = take.tracks.reduce((n, t) => n + (t.progress?.bufferedBytes ?? 0), 0);
  const error = take.tracks.find(t => t.progress?.error)?.progress?.error;
  const done = take.tracks.length > 0 && take.tracks.every(t => t.done || t.progress?.isComplete);
  dispatch({ type: 'progress', takeId: take.takeId, uploadedBytes: uploaded, pendingBytes: pending, uploaded: done, error });
}

async function beginTake(): Promise<void> {
  if (state.phase !== 'countdown' || !draft || !camera || !micStream) return;
  let takeId = '';
  const take: LocalTake = { takeId: '', tracks: [], pcmStream: null, startedAt: 0 };
  try {
    const started = await api.startTake(draft.submissionId, draft.key);
    takeId = started.takeId; take.takeId = takeId;
    local.set(takeId, take);
    const fit = clock.getFit();
    const fail = (error: Error) => { if (active === take) { state = { ...state, message: `Recording stopped: ${error.message}` }; void stopTake(); } };
    const stream = new MediaStream([...camera.getVideoTracks(), ...micStream.getAudioTracks()]);
    const codec = link?.recording.video_codec;
    const video = await registerTrack(takeId, 'video', LocalTrackRecorder.preferredMimeType('video', { videoCodec: codec }), 'Camera + microphone', take);
    video.recorder = new LocalTrackRecorder(video.trackId, 'video', chunk => video.queue.enqueue(chunk), fit,
      { engine: 'mediarecorder', videoBitrateBps: link?.recording.video_bitrate_bps, videoCodec: codec, targetFps: link?.recording.target_fps, micProcessing: micState ?? undefined, onError: fail });
    let pcm: LocalTrack | null = null;
    if (el.pcm.checked) {
      pcm = await registerTrack(takeId, 'audio', 'audio/wav', 'Uncompressed microphone', take, true);
      const { resolved } = resolveRecordingMic(micMode(), micDefault(), headphones);
      const deviceId = micStream.getAudioTracks()[0]?.getSettings().deviceId;
      take.pcmStream = await openAudio(exact => ({ ...captureAudioConstraints(deviceId, RECORDING_PROCESSING[resolved], exact), sampleRate: { ideal: 48000 }, channelCount: { ideal: 2 } }));
      const queue = pcm.queue;
      pcm.recorder = new LocalPcmRecorder(pcm.trackId, chunk => queue.enqueue(chunk), fit,
        { sampleRate: 48000, replacesAudioOfTrackId: video.trackId, micProcessing: recordingMicState(micMode(), micDefault(), headphones, take.pcmStream.getAudioTracks()[0].getSettings()), onError: fail });
    }
    await Promise.all([video.recorder.start(stream), ...(pcm && take.pcmStream ? [pcm.recorder.start(take.pcmStream)] : [])]);
    active = take;
    take.startedAt = performance.now();
    dispatch({ type: 'started', take: { takeId, number: started.number } });
    void keepAwake();
    scrollScript(true);
    const limit = started.maxDurationSec;
    const tick = () => {
      const elapsed = (performance.now() - take.startedAt) / 1000;
      el.timer.textContent = formatClock(elapsed);
      const left = remainingSec(limit, elapsed);
      el.left.textContent = `${formatClock(left)} left`;
      el.left.classList.toggle('is-low', left <= 30);
      // The host stops a take at its limit; stop just before, cleanly.
      if (elapsed >= limit - 0.5 && active === take) { state = { ...state, message: `That recording reached the ${formatLimit(limit)} limit and was saved.` }; void stopTake(); }
    };
    tick();
    timerHandle = window.setInterval(tick, 250);
  } catch (err) {
    // Undo a half-started take so it doesn't linger on the host.
    for (const t of take.tracks) void t.recorder?.stop().catch(() => undefined);
    take.pcmStream?.getTracks().forEach(t => t.stop());
    if (takeId && draft) {
      await api.stopTake(draft.submissionId, draft.key, takeId).catch(() => undefined);
      await api.discardTake(draft.submissionId, draft.key, takeId).catch(() => undefined);
      for (const t of take.tracks) await OPFSStorage.getInstance().deleteTrack(takeId, t.trackId).catch(() => undefined);
      local.delete(takeId);
    }
    active = null;
    dispatch({ type: 'start-failed', message: `Couldn’t start recording: ${errorText(err)}` });
  }
}

async function stopTake(): Promise<void> {
  const take = active;
  if (!take || !draft) return;
  active = null;
  dispatch({ type: 'stop' });
  if (timerHandle) { clearInterval(timerHandle); timerHandle = null; }
  scrollScript(false);
  const stopped = await Promise.all(take.tracks.map(t => t.recorder.stop().then(r => r, () => null as { lastSeq: number; sidecar: SidecarData } | null)));
  take.pcmStream?.getTracks().forEach(t => t.stop());
  let duration = (performance.now() - take.startedAt) / 1000;
  try {
    const res = await api.stopTake(draft.submissionId, draft.key, take.takeId);
    duration = res.durationSec || duration;
  } catch (err) { state = { ...state, message: errorText(err) }; }
  dispatch({ type: 'stopped', takeId: take.takeId, durationSec: duration });
  void releaseWake();
  // Finish uploads in the background; the take row shows progress.
  take.tracks.forEach((t, i) => {
    const s = stopped[i];
    if (!s) { t.progress = { ...(t.progress ?? { bufferedBytes: 0, uploadedBytes: 0, lastSeq: -1, isComplete: false }), error: 'This track stopped early; reopen the link to retry' }; reportProgress(take); return; }
    t.lastSeq = s.lastSeq;
    void t.queue.complete(s.lastSeq, s.sidecar).then(ok => { t.done = ok; reportProgress(take); });
  });
  void showReview(take.takeId);
}

/** Plays a take back from this browser's own copy (OPFS), not the host's. */
async function showReview(takeId: string): Promise<void> {
  const take = local.get(takeId);
  const video = take?.tracks.find(t => t.kind === 'video');
  if (!take || !video || video.lastSeq === undefined) return;
  if (!take.reviewUrl) {
    const opfs = OPFSStorage.getInstance();
    const parts: BlobPart[] = [];
    for (let seq = 0; seq <= video.lastSeq; seq++) {
      const bytes = await opfs.getChunk(takeId, video.trackId, seq);
      if (!bytes) return;
      parts.push(bytes as unknown as BlobPart);
    }
    take.reviewUrl = URL.createObjectURL(new Blob(parts, { type: video.mime.split(';')[0] }));
  }
  reviewingTakeId = takeId;
  el.review.src = take.reviewUrl;
  el.review.hidden = false; el.preview.hidden = true; el.backLive.hidden = false;
  el.reviewLabel.hidden = false;
  el.reviewLabel.textContent = `Playing recording ${state.takes.find(t => t.id === takeId)?.number ?? ''}`;
  render();
}

function showLive(): void {
  reviewingTakeId = '';
  el.review.pause();
  el.review.hidden = true; el.preview.hidden = false; el.backLive.hidden = true; el.reviewLabel.hidden = true;
}

async function deleteTake(takeId: string): Promise<void> {
  const t = state.takes.find(x => x.id === takeId);
  if (!draft || !t || !canDiscard(state, takeId)) return;
  if (!await confirmDialog(`Delete recording ${t.number}?`, 'It is removed from this device and from the host. This can’t be undone.', 'Delete recording')) return;
  try {
    await api.discardTake(draft.submissionId, draft.key, takeId);
  } catch (err) { state = { ...state, message: errorText(err) }; render(); return; }
  const take = local.get(takeId);
  if (reviewingTakeId === takeId) showLive();
  if (take?.reviewUrl) URL.revokeObjectURL(take.reviewUrl);
  local.delete(takeId);
  const opfs = OPFSStorage.getInstance();
  for (const tr of take?.tracks ?? []) await opfs.deleteTrack(takeId, tr.trackId).catch(() => undefined);
  dispatch({ type: 'discarded', takeId });
}

async function submit(): Promise<void> {
  if (!draft || !canSubmit(state)) return;
  const n = state.takes.length;
  if (!await confirmDialog(`Send ${n > 1 ? `${n} recordings` : 'your recording'} to the host?`, 'After sending you can’t add or delete recordings. Keep this tab open until the upload finishes.', 'Send')) return;
  showLive();
  dispatch({ type: 'submit' });
  try {
    const sub = await api.submit(draft.submissionId, draft.key);
    grant = sub.uploadToken;
    dispatch({ type: 'submitted' });
  } catch (err) { dispatch({ type: 'submit-failed', message: errorText(err) }); }
}

// ----------------------------------------------------------------- resume

/** Reopened link: this browser's submission, and any upload a closed tab left behind. */
async function resume(): Promise<void> {
  draft = parseDraft((() => { try { return localStorage.getItem(draftKey(linkId)); } catch { return null; } })());
  if (!draft) return;
  el.name.value = draft.name;
  let sub: RecordGuestSubmission;
  try { sub = await api.get(draft.submissionId, draft.key); }
  catch (err) {
    if (err instanceof RecordApiError && err.status === 404) { try { localStorage.removeItem(draftKey(linkId)); } catch { /* ignore */ } draft = null; return; }
    throw err;
  }
  grant = sub.uploadToken;
  // A take left recording by a closed tab ends now (the host would end it at its limit).
  for (const t of sub.takes.filter(t => t.status === 'recording')) {
    const r = await api.stopTake(sub.id, draft.key, t.id).catch(() => null);
    if (r) { t.status = r.status; t.durationSec = r.durationSec; }
  }
  dispatch({ type: 'resumed', submission: sub });
  await recoverUploads(sub.takes.map(t => t.id));
}

async function recoverUploads(takeIds: string[]): Promise<void> {
  const opfs = OPFSStorage.getInstance();
  if (!opfs.isSupported()) return;
  const metas = (await opfs.listUnfinishedTracks().catch(() => [])).filter(m => takeIds.includes(m.takeId));
  if (!metas.length) return;
  state = { ...state, message: 'Resuming uploads from this device…' };
  render();
  const progress = new Map<string, UploadProgress>();
  const report = (takeId: string) => {
    const mine = metas.filter(m => m.takeId === takeId);
    const up = mine.reduce((n, m) => n + (progress.get(m.trackId)?.uploadedBytes ?? 0), 0);
    const pend = mine.reduce((n, m) => n + (progress.get(m.trackId)?.bufferedBytes ?? 0), 0);
    const t = state.takes.find(x => x.id === takeId);
    dispatch({ type: 'progress', takeId, uploadedBytes: Math.max(up, t?.uploadedBytes ?? 0), pendingBytes: pend, uploaded: false, error: mine.map(m => progress.get(m.trackId)?.error).find(Boolean) });
  };
  await Promise.all(metas.map(async meta => {
    // The journal's grant may have expired: use the fresh one.
    meta.uploadToken = grant;
    await recoverTakeFromOPFS(meta, grant, p => { progress.set(meta.trackId, p); report(meta.takeId); });
  }));
  // The host's view is the truth once recovery settles.
  if (draft) {
    const sub = await api.get(draft.submissionId, draft.key).catch(() => null);
    if (sub) {
      grant = sub.uploadToken;
      for (const t of sub.takes) {
        const done = t.tracks.length > 0 && t.tracks.every(tr => tr.status === 'assembled' || tr.status === 'partial' || tr.status === 'processed');
        dispatch({ type: 'progress', takeId: t.id, uploadedBytes: t.tracks.reduce((n, tr) => n + tr.bytes, 0), pendingBytes: 0, uploaded: done });
      }
    }
  }
  state = { ...state, message: undefined };
  render();
}

// ------------------------------------------------------------------ misc

function errorText(err: unknown): string {
  if (err instanceof RecordApiError) return err.message;
  return err instanceof Error ? err.message : String(err);
}

function confirmDialog(title: string, body: string, ok: string): Promise<boolean> {
  return new Promise(resolve => {
    const d = el.dialog;
    $('rec-dialog-title').textContent = title;
    $('rec-dialog-body').textContent = body;
    $('rec-dialog-ok').textContent = ok;
    d.addEventListener('close', () => resolve(d.returnValue === 'ok'), { once: true });
    d.returnValue = '';
    d.showModal();
    $<HTMLButtonElement>('rec-dialog-ok').focus();
  });
}

async function keepAwake(): Promise<void> {
  try {
    const nav = navigator as Navigator & { wakeLock?: { request(type: 'screen'): Promise<{ release(): Promise<void> }> } };
    wakeLock = await nav.wakeLock?.request('screen') ?? null;
  } catch { /* Not supported. */ }
}
async function releaseWake(): Promise<void> { await wakeLock?.release().catch(() => undefined); wakeLock = null; }

function busy(): boolean {
  return !!active || state.takes.some(t => !t.uploaded && t.status !== 'recording' && (t.pendingBytes > 0 || local.has(t.id)));
}
window.addEventListener('beforeunload', e => { if (busy()) { e.preventDefault(); e.returnValue = ''; } });

// ---------------------------------------------------------------- wiring

el.record.addEventListener('click', () => void onRecordButton());
el.submit.addEventListener('click', () => void submit());
el.backLive.addEventListener('click', () => { showLive(); render(); });
el.takes.addEventListener('click', e => {
  const btn = (e.target as HTMLElement).closest<HTMLButtonElement>('button[data-act]');
  const id = btn?.closest<HTMLElement>('[data-take-id]')?.dataset.takeId;
  if (!btn || !id) return;
  if (btn.dataset.act === 'play') void showReview(id);
  if (btn.dataset.act === 'delete') void deleteTake(id);
});
el.name.addEventListener('input', () => render());
el.camera.addEventListener('change', () => void openCamera(el.camera.value).then(updatePreflight).catch(err => { state = { ...state, message: `Camera unavailable: ${errorText(err)}` }; render(); }));
el.mic.addEventListener('change', () => void openMic(el.mic.value).then(updatePreflight).catch(err => { state = { ...state, message: `Microphone unavailable: ${errorText(err)}` }; render(); }));
el.micmode.addEventListener('change', () => { saveRecordingMicMode(localStorage, micMode()); void openMic(el.mic.value).then(updatePreflight); });
el.pcm.addEventListener('change', () => { try { localStorage.setItem('studiolink.recordPcm', el.pcm.checked ? '1' : '0'); } catch { /* ignore */ } void updatePreflight(); });
document.addEventListener('keydown', e => {
  if (e.target instanceof HTMLInputElement || e.target instanceof HTMLTextAreaElement || e.target instanceof HTMLSelectElement || el.dialog.open) return;
  if (e.code === 'Space' && (state.phase === 'recording' || state.phase === 'countdown')) { e.preventDefault(); void onRecordButton(); }
});

async function boot(): Promise<void> {
  if (!linkId || !secret) { dispatch({ type: 'link-error', status: 401, message: 'This page needs the full recording link from the host. Open it again from their message.' }); return; }
  try { el.micmode.value = loadRecordingMicMode(localStorage); el.pcm.checked = localStorage.getItem('studiolink.recordPcm') === '1'; } catch { /* defaults */ }
  try {
    link = await api.info();
  } catch (err) {
    const status = err instanceof RecordApiError ? err.status : 0;
    // A closed tab's uploads still finish with their own grants, link or not.
    if (linkErrorPhase(status)) void recoverOrphans();
    dispatch({ type: 'link-error', status: status || 401, message: status ? undefined : 'Can’t reach the host right now. Check your connection and reload.' });
    if (!status) setNotice('Can’t reach the host', 'Check your connection and reload this page.');
    return;
  }
  document.title = `${link.title} — Record`;
  el.title.textContent = link.title;
  el.expiry.hidden = false;
  el.expiry.textContent = expiryLabel(link.expiresAt);
  renderScript(link.prompt);
  try { await resume(); }
  catch (err) { state = { ...state, message: `Couldn’t load your earlier recording: ${errorText(err)}` }; }
  if (state.phase === 'loading') dispatch({ type: 'link-ok', accepting: link.accepting });
  if (state.phase === 'full' || state.phase === 'submitted') { render(); if (state.phase === 'submitted') void startDevicesQuietly(false); return; }
  await startDevicesQuietly(true);
}

/** Opens camera and microphone (the green room). */
async function startDevicesQuietly(open: boolean): Promise<void> {
  if (!open) return;
  try {
    await openCamera();
    await listDevices();
    await openMic(el.mic.value || undefined);
    await listDevices();
    void navigator.storage?.persist?.().catch(() => false);
  } catch (err) {
    state = { ...state, message: `Allow camera and microphone access to record (${errorText(err)}).` };
  }
  render();
  void updatePreflight();
}

/** Link refused but this browser has a submission: finish its uploads with the journal's own grants. */
async function recoverOrphans(): Promise<void> {
  const d = parseDraft((() => { try { return localStorage.getItem(draftKey(linkId)); } catch { return null; } })());
  if (!d) return;
  const opfs = OPFSStorage.getInstance();
  const metas = (await opfs.listUnfinishedTracks().catch(() => [])).filter(m => m.takeId.startsWith(`take_${d.submissionId}_`));
  for (const meta of metas) await recoverTakeFromOPFS(meta, meta.uploadToken).catch(() => false);
}

void boot();
void annotateSourceLinks();
