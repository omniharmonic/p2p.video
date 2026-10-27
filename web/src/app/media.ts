// Local capture [SL-TECH-1]: camera + recording microphone, the call
// microphone (CallMicrophone), audio processing and recording-mic mode,
// background effects, device switching, mute/camera toggles and the level
// meter. The recording capture and the call capture stay separate
// (docs/ARCHITECTURE.md §5).
import { BackgroundEffects } from '../components/BackgroundEffects';
import { DevicePicker } from '../components/DevicePicker';
import { NoiseSuppression } from '../components/NoiseSuppression';
import { RecordingMic } from '../components/RecordingMic';
import {
  type CallAudioProcessing, CallMicrophone, captureAudioConstraints, openCaptureMic, processingMatches,
  SharedProcessingError,
} from '../lib/callmic';
import type { RecordingMicState } from '../lib/protocol';
import { rtc, sendParticipantState } from './call';
import {
  cameraSelect, joinBtn, localVideo, micSelect, previewVideo, toggleCamBtn, toggleMicBtn, videoGrid, vuBarL, vuBarR,
  waitingPreviewVideo,
} from './dom';
import { preflight, showBrowserNote } from './greenroom';
import { pcmMicStream } from './recording';
import { state } from './state';
import { renderTileState, showToast, updateResolutionSpec } from './ui';

let audioContext: AudioContext | null = null;
let analyserL: AnalyserNode | null = null;
let analyserR: AnalyserNode | null = null;
let meterFrame = 0;
let meterSource: MediaStreamAudioSourceNode | null = null;
// The call sends a clone of the mic so a host mute silences the call only;
// self-mute still silences call and recording (docs/ARCHITECTURE.md §5).
export const callMic = new CallMicrophone();
export let devicePicker: DevicePicker;
export let noiseSuppression: NoiseSuppression;
export let recordingMic: RecordingMic;
export let effects: BackgroundEffects;

export function createDevicePicker(): void {
  devicePicker = new DevicePicker({
    localStream: () => state.localStream,
    inputsLocked: () => state.recordState !== 'idle' ? 'Camera and microphone are locked while a take is recording, so your recording is not interrupted. Stop the take to switch. Speaker output can still change.' : '',
    switchInput: (kind, deviceId) => switchCallDevice(kind, deviceId),
    toast: msg => showToast(msg),
  }, videoGrid);
}

/** Call audio processing (noise suppression etc.) for the call mic only [SL-P1-8]. */
export function createNoiseSuppression(): void {
  noiseSuppression = new NoiseSuppression({
    toast: msg => showToast(msg),
    apply: processing => applyCallAudio(processing, true),
    sharedWithRecording: () => callMic.sharesRecordingProcessing,
    followsRecording: () => {
      if (!callMic.followsRecording || !callMic.callTrack) return null;
      const s = callMic.callTrack.getSettings();
      return { echoCancellation: !!s.echoCancellation, noiseSuppression: !!s.noiseSuppression, autoGainControl: !!s.autoGainControl };
    },
  });
}

/**
 * Recording microphone mode [SL-P1-10]: raw with headphones, clean without
 * (or what the participant / host chose). The recording capture is reopened
 * with exact constraints when the resolved processing changes; the call keeps
 * its own capture (CallMicrophone), measured separate in Chromium.
 */
export function createRecordingMic(): void {
  recordingMic = new RecordingMic({
    lockedReason: () => state.recordState !== 'idle' ? 'Locked while a take is recording, like device switching.' : '',
    apply: () => applyRecordingMic(),
    toast: msg => showToast(msg),
  });
}

/** Background blur/replace for the call sender only; recording keeps the raw camera [SL-P1-7]. */
export function createEffects(): void {
  effects = new BackgroundEffects({
    camera: () => state.localStream?.getVideoTracks()[0] ?? null,
    toast: msg => showToast(msg),
    outputChanged: async (previous, next) => {
      if (previous && next && previous !== next) await rtc.replaceSenderTrack(previous, next);
      refreshSelfViews();
    },
  });
}

export function showDeviceError(error: unknown): void {
  const message = error instanceof Error ? error.message : String(error);
  const status = document.getElementById('device-status');
  if (status) status.textContent = `Camera or microphone unavailable: ${message}. Check browser permissions and select working inputs.`;
  joinBtn.disabled = !state.localStream;
}

// Makes the call microphone use `processing`. Where the browser can't process
// the call capture separately, the capture mic is reopened with the call's
// processing instead (then it applies to the recording too, and is locked
// while a take records, like device switching).
export async function applyCallAudio(processing?: CallAudioProcessing, userChange = false): Promise<void> {
  if (!state.localStream) return;
  // A processed (clean) recording capture can't coexist with a differently
  // processed call capture in Chromium: the call then shares it [SL-P1-10].
  const rec = state.localStream.getAudioTracks()[0]?.getSettings();
  callMic.shareWhenRefused = !!(rec && (rec.echoCancellation || rec.noiseSuppression || rec.autoGainControl));
  let previous: MediaStreamTrack | null;
  try { previous = await callMic.prepare(state.localStream, processing); }
  catch (err) {
    if (!(err instanceof SharedProcessingError)) throw err;
    if (state.recordState !== 'idle') {
      if (userChange) throw new Error('in this browser call audio settings also change your recording microphone, so they are locked while a take records.');
      // Rebuilding call media mid-take (join, media recovery): never touch the
      // recording capture; the call shares its processing until the take ends.
      callMic.shareWhenRefused = true;
      previous = await callMic.prepare(state.localStream);
    } else {
      const old = state.localStream.getAudioTracks()[0];
      const fresh = (await openCaptureMic(old?.getSettings().deviceId, callMic.processing)).getAudioTracks()[0];
      old?.stop();
      if (old) state.localStream.removeTrack(old);
      fresh.enabled = !state.isMicMuted;
      state.localStream.addTrack(fresh);
      setupAudioMeter(state.localStream);
      // The reopened capture has the call's processing as far as this device
      // can apply it. A device that can't process at all (Firefox's fake
      // microphone, some USB interfaces) reports none; the call then shares
      // the capture rather than failing to connect [SL-P1-12].
      callMic.shareWhenRefused = true;
      previous = await callMic.prepare(state.localStream);
      reportRecordingMic();
    }
  }
  if (previous && callMic.callTrack) await rtc.replaceSenderTrack(previous, callMic.callTrack);
  noiseSuppression.refresh();
}

/** Processing the recording microphone should have right now. */
export function recordingProcessing(): CallAudioProcessing {
  return callMic.sharesRecordingProcessing ? callMic.processing : recordingMic.processing;
}

/** The recording mic state to report for a track (measured getSettings()). */
export function recordingMicFor(track: MediaStreamTrack | undefined): RecordingMicState {
  return recordingMic.state(track?.getSettings());
}

export function reportRecordingMic(): void {
  recordingMic.setMeasured(state.localStream ? recordingMicFor(state.localStream.getAudioTracks()[0]) : null);
  preflight.refreshExtras();
}

export async function applyRecordingMic(): Promise<void> {
  if (!state.localStream) return;
  if (state.recordState !== 'idle') throw new Error('Inputs are locked while a take is recording.');
  const track = state.localStream.getAudioTracks()[0];
  const want = recordingProcessing();
  if (!track || processingMatches(track.getSettings() as Partial<Record<keyof CallAudioProcessing, boolean>>, want)) { reportRecordingMic(); return; }
  await replaceCaptureMic(track.getSettings().deviceId || micSelect.value || undefined, want);
}

/** Swaps the recording (capture) microphone; the call follows via CallMicrophone. */
export async function replaceCaptureMic(deviceId: string | undefined, processing: CallAudioProcessing): Promise<void> {
  // Chromium allows one processed capture per microphone (raw captures always
  // coexist), so a processed recording capture needs the call's own processed
  // capture gone first (a moment of call silence) [SL-P1-10].
  const processed = processing.echoCancellation || processing.noiseSuppression || processing.autoGainControl;
  if (processed) callMic.releaseCallCapture();
  const fresh = (await openCaptureMic(deviceId, processing)).getAudioTracks()[0];
  if (!state.localStream || state.recordState !== 'idle') { fresh?.stop(); throw new Error('Inputs are locked while a take is recording.'); }
  const old = state.localStream.getAudioTracks()[0];
  fresh.enabled = !state.isMicMuted;
  if (old) state.localStream.removeTrack(old);
  state.localStream.addTrack(fresh);
  // The old capture goes before the call reopens its own, for the same reason.
  old?.stop();
  if (state.callAdmitted) await applyCallAudio();
  setupAudioMeter(state.localStream);
  refreshSelfViews();
  reportRecordingMic();
}

/** What this device shows as itself: the call's (possibly processed) camera. */
export function selfView(): MediaStream | null { return state.localStream ? effects.callStream(state.localStream) : null; }

export function refreshSelfViews(): void {
  for (const video of [previewVideo, waitingPreviewVideo, localVideo]) if (video && video.srcObject) video.srcObject = selfView();
}

// 1. Device Initialisation
export async function initDevices(): Promise<void> {
  try {
    if (state.hasJoined) throw new Error('Device changes are locked after joining. Leave this page to choose different inputs.');
    await refreshDeviceChoices();

    await startPreview();
  } catch (err) {
    showDeviceError(err);
    throw err;
  }
}

export async function refreshDeviceChoices(): Promise<void> {
  const videoId = state.localStream?.getVideoTracks()[0]?.getSettings().deviceId || cameraSelect.value;
  const audioId = state.localStream?.getAudioTracks()[0]?.getSettings().deviceId || micSelect.value;
  const devices = await navigator.mediaDevices.enumerateDevices();
  cameraSelect.replaceChildren();
  micSelect.replaceChildren();
  for (const device of devices) {
    if (device.kind !== 'videoinput' && device.kind !== 'audioinput') continue;
    const option = document.createElement('option');
    option.value = device.deviceId;
    option.textContent = device.label || (device.kind === 'videoinput' ? 'Default camera' : 'Default microphone');
    const select = device.kind === 'videoinput' ? cameraSelect : micSelect;
    select.appendChild(option);
  }
  if ([...cameraSelect.options].some(option => option.value === videoId)) cameraSelect.value = videoId;
  if ([...micSelect.options].some(option => option.value === audioId)) micSelect.value = audioId;
}

/** Releases the camera and mic while the host browses the home (the camera
 *  light goes off); Ready to join opens them again [SL-UX-2]. */
export function stopPreview(): void {
  if (state.hasJoined || !state.localStream) return;
  state.localStream.getTracks().forEach(track => track.stop());
  state.localStream = null;
  previewVideo.srcObject = null;
  joinBtn.disabled = true;
}

export async function startPreview(): Promise<void> {
  if (state.hasJoined) throw new Error('Device changes are locked while connected to the studio');
  const previousStream = state.localStream;

  const videoId = cameraSelect.value;
  const audioId = micSelect.value;

  const nextStream = await openCameraAndMic(videoId || undefined, audioId || undefined);

  state.localStream = nextStream;
  previousStream?.getTracks().forEach(track => track.stop());
  previewVideo.srcObject = state.localStream;
  void effects.cameraChanged();
  // Refresh labels/IDs after permission without reopening the camera.
  try { await refreshDeviceChoices(); } catch { /* The acquired stream remains usable. */ }
  const status = document.getElementById('device-status');
  if (status) {
    const video = state.localStream.getVideoTracks()[0]?.getSettings();
    const lowResolution = video?.width && video?.height && video.width * video.height < 1920 * 1080;
    status.textContent = lowResolution
      ? `Camera provides ${video.width} × ${video.height}, below requested Full HD. Try another camera before joining. Recording uses this actual resolution.`
      : 'Camera and microphone are ready. Device choices lock when you join.';
  }
  joinBtn.disabled = false;
  updateResolutionSpec();
  setupAudioMeter(state.localStream);
  reportRecordingMic();
  void preflight.run();
  void showBrowserNote();
}

/**
 * Camera + recording microphone in one request (one permission prompt). The
 * recording mic opens with its mode's processing, requested exactly; a
 * browser that refuses exact values gets ideal ones [SL-P1-8, SL-P1-10].
 */
export async function openCameraAndMic(videoId: string | undefined, audioId: string | undefined): Promise<MediaStream> {
  // IDs are hidden before Safari's first permission grant. Quality constraints
  // must still be requested when no device ID is available.
  const video = { ...(videoId ? { deviceId: { exact: videoId } } : {}), width: { ideal: 1920 }, height: { ideal: 1080 }, frameRate: { ideal: 30 } };
  try {
    return await navigator.mediaDevices.getUserMedia({ video, audio: captureAudioConstraints(audioId, recordingProcessing()) });
  } catch (err) {
    if ((err as { name?: string })?.name !== 'OverconstrainedError') throw err;
    return navigator.mediaDevices.getUserMedia({ video, audio: captureAudioConstraints(audioId, recordingProcessing(), false) });
  }
}

export function setupAudioMeter(stream: MediaStream): void {
  if (!audioContext) audioContext = new AudioContext();
  if (meterFrame) cancelAnimationFrame(meterFrame);
  meterSource?.disconnect();
  const source = audioContext.createMediaStreamSource(stream);
  meterSource = source;

  // Split stereo
  const splitter = audioContext.createChannelSplitter(2);
  analyserL = audioContext.createAnalyser();
  analyserR = audioContext.createAnalyser();
  analyserL.fftSize = 256;
  analyserR.fftSize = 256;

  source.connect(splitter);
  splitter.connect(analyserL, 0);
  splitter.connect(analyserR, 1 > splitter.numberOfOutputs - 1 ? 0 : 1);

  const dataL = new Uint8Array(analyserL.frequencyBinCount);
  const dataR = new Uint8Array(analyserR.frequencyBinCount);

  function checkLevel() {
    if (analyserL && analyserR && !state.isMicMuted) {
      analyserL.getByteFrequencyData(dataL);
      analyserR.getByteFrequencyData(dataR);

      let sumL = 0;
      for (let i = 0; i < dataL.length; i++) sumL += dataL[i];
      const avgL = sumL / dataL.length;

      let sumR = 0;
      for (let i = 0; i < dataR.length; i++) sumR += dataR[i];
      const avgR = sumR / dataR.length;

      if (vuBarL) vuBarL.style.width = `${Math.min(100, avgL * 2.2)}%`;
      if (vuBarR) vuBarR.style.width = `${Math.min(100, (avgR || avgL) * 2.2)}%`;
    } else {
      if (vuBarL) vuBarL.style.width = '0%';
      if (vuBarR) vuBarR.style.width = '0%';
    }
    meterFrame = requestAnimationFrame(checkLevel);
  }
  checkLevel();
}

// Mid-call device switch: swap the capture track and the call sender with
// replaceTrack (no renegotiation, no rejoin). Locked while a take records
// (docs/DECISIONS.md), so a recorder never loses its source mid-take.
export async function switchCallDevice(kind:'videoinput'|'audioinput',deviceId:string):Promise<void>{
  if(!state.localStream)throw new Error('No local media');
  if(state.recordState!=='idle')throw new Error('Inputs are locked while a take is recording.');
  const video=kind==='videoinput';
  const fresh=video
    ?await navigator.mediaDevices.getUserMedia({video:{deviceId:{exact:deviceId},width:{ideal:1920},height:{ideal:1080},frameRate:{ideal:30}}})
    :await openCaptureMic(deviceId,recordingProcessing()); // the recording mic keeps its mode [SL-P1-10]
  const next=(video?fresh.getVideoTracks():fresh.getAudioTracks())[0];
  if(!next||state.recordState!=='idle'){fresh.getTracks().forEach(t=>t.stop());throw new Error('Inputs are locked while a take is recording.');}
  const old=(video?state.localStream.getVideoTracks():state.localStream.getAudioTracks())[0];
  next.enabled=video?!state.isCamOff:!state.isMicMuted;
  if(old)state.localStream.removeTrack(old);
  state.localStream.addTrack(next);
  if(video){if(!effects.active)await rtc.replaceSenderTrack(old,next);await effects.cameraChanged();}
  else{
    const oldCall=callMic.callTrack;
    await applyCallAudio(); // the new microphone for the call, with the call's processing
    if(callMic.callTrack)await rtc.replaceSenderTrack(oldCall,callMic.callTrack);
    setupAudioMeter(state.localStream);
    reportRecordingMic();
  }
  old?.stop();
  localVideo.srcObject=selfView();
  updateResolutionSpec();
}

export function renderMicState(): void {
  const suffix = state.isProducer ? '' : ' + recording';
  toggleMicBtn.innerText = callMic.isHostMuted ? 'Unmute call mic (host muted you)' : state.isMicMuted ? `Unmute mic${suffix}` : `Mute mic${suffix}`;
  renderTileState(document.getElementById('local-tile')!, { micMuted: callMic.callMuted, camOff: state.isCamOff });
  sendParticipantState();
}

// Host mute request (Meet semantics): the call stops hearing this microphone,
// but the local ISO recording keeps the raw mic, and we say so plainly.
export function applyHostMute(by: string): void {
  if (callMic.callMuted) return;
  callMic.setHostMuted(true);
  renderMicState();
  showToast(`${by} muted your microphone in the call.${state.isRecording ? ' Your local recording still captures your mic; use “Mute mic + recording” to stop that too.' : ''} Unmute when you want to be heard.`);
}

export function renderCamState(): void {
  // A producer is never recorded, so its labels don't mention recording [SL-P1-11].
  const suffix = state.isProducer ? '' : ' + recording';
  toggleCamBtn.innerText = state.isCamOff ? `Enable camera${suffix}` : `Disable camera${suffix}`;
  renderTileState(document.getElementById('local-tile')!, { micMuted: callMic.callMuted, camOff: state.isCamOff });
  sendParticipantState();
}

/** Leaving: stop the level meter loop (the capture tracks are stopped by the caller). */
export function stopAudioMeter(): void {
  if (meterFrame) { cancelAnimationFrame(meterFrame); meterFrame = 0; }
}

export function playSyncChirp(): void {
  if (!audioContext) audioContext = new AudioContext();
  const osc = audioContext.createOscillator();
  const gain = audioContext.createGain();
  osc.connect(gain);
  gain.connect(audioContext.destination);

  osc.type = 'sine';
  osc.frequency.setValueAtTime(1000, audioContext.currentTime);
  osc.frequency.exponentialRampToValueAtTime(3000, audioContext.currentTime + 0.1);

  gain.gain.setValueAtTime(0.5, audioContext.currentTime);
  gain.gain.exponentialRampToValueAtTime(0.01, audioContext.currentTime + 0.1);

  osc.start();
  osc.stop(audioContext.currentTime + 0.1);
}

/** Mic and camera toggles: muting covers the call and the local recording. */
export function bindMuteControls(): void {
  toggleMicBtn.onclick = () => {
    if (!state.localStream) return;
    const wasHostMuted = callMic.isHostMuted;
    // Unmuting after a host mute only restores the call; recording never stopped.
    if (!wasHostMuted) state.isMicMuted = !state.isMicMuted;
    callMic.setSelfMuted(state.isMicMuted, [state.localStream, pcmMicStream]);
    renderMicState();
    showToast(wasHostMuted ? 'Microphone back on in the call' : state.isMicMuted ? 'Microphone muted in the call and local recording' : 'Microphone active in the call and local recording');
  };


  toggleCamBtn.onclick = () => {
    if (!state.localStream) return;
    state.isCamOff = !state.isCamOff;
    state.localStream.getVideoTracks().forEach((t) => (t.enabled = !state.isCamOff));
    renderCamState();
    showToast(state.isProducer ? (state.isCamOff ? 'Camera off' : 'Camera on — others can see you') : state.isCamOff ? 'Camera disabled in the call and local recording' : 'Camera active in the call and local recording');
  };
}
