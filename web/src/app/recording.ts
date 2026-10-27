// Local recording [SL-TECH-1]: take lifecycle (countdown, start, stop),
// recorders and progressive upload queues, finalization receipts, screen
// recording, the transfers modal and the editor export. Recording reads only
// local capture tracks, never WebRTC-received media (AGENTS.md guardrail).
import { EcosystemActions } from '../components/EcosystemActions';
import { OPFSRecoveryBanner } from '../components/OPFSRecoveryBanner';
import type { ScreenRecording } from '../components/ScreenShare';
import { type TrackTransferItem, TransfersRecoveryModal } from '../components/TransfersRecoveryModal';
import { callAudioConstraints, openAudio } from '../lib/callmic';
import { describeExport, pollTakeExport } from '../lib/exportprogress';
import type { UploadBacklog } from '../lib/leave';
import { LocalPcmRecorder } from '../lib/pcm-recorder';
import { type RecordStartPayload, TypeRecordStop } from '../lib/protocol';
import { LocalTrackRecorder, type SidecarData } from '../lib/recorder';
import { ProgressiveUploadQueue, type UploadProgress } from '../lib/upload';
import { callNetwork, screenShare } from './call';
import {
  closeModalBtn, countdownOverlay, countdownVal, displaySessionTitle, dockTransfersBtn, exportModal, localIsoBadge,
  mainContainer, micSelect, nameInput, openFolderBtn, pcmToggle, postprocBtn, recBtn, recIndicator, recLabel,
  recTimer, telemetryChunks, telemetryStatus, viewTransfersAsideBtn,
} from './dom';
import { stopDiagnosticClip } from './greenroom';
import { devicePicker, recordingMic, recordingMicFor, recordingProcessing } from './media';
import { clockSync, sendEnvelope } from './signaling';
import { state } from './state';
import { leaveCall } from './studio';
import { showToast, updateResolutionSpec } from './ui';

let captureGeneration = 0;
/** Elapsed recording time for the REC pill: 4:05, or 1:02:09 past an hour. */
export function elapsed(ms:number):string{const t=Math.max(0,Math.floor(ms/1000));const h=Math.floor(t/3600),m=Math.floor(t/60)%60,sec=String(t%60).padStart(2,'0');return h?`${h}:${String(m).padStart(2,'0')}:${sec}`:`${m}:${sec}`;}
let captureFailureMessage = '';
interface ActiveRecordingTrack {trackId:string;kind:'video'|'audio';label:string;queue:ProgressiveUploadQueue;recorder:LocalTrackRecorder|LocalPcmRecorder|null;progress:UploadProgress|null;stopped?:Promise<{lastSeq:number;sidecar:SidecarData}|null>;finalization?:Promise<boolean>;}
export let recordingTracks:ActiveRecordingTrack[]=[];
// Every track recorded in this page, until the host confirms it (leave screen) [SL-P1-6].
const unconfirmedTracks=new Set<ActiveRecordingTrack>();
export let pcmMicStream:MediaStream|null=null;
let countdownTimer: number | null = null;
let recTimerInterval: number | null = null;
export let ecosystemActions: EcosystemActions;
export let transfersModal: TransfersRecoveryModal;
export let recoveryBanner: OPFSRecoveryBanner;

export function createEcosystemActions(): void {
  ecosystemActions = new EcosystemActions(document.getElementById('ecosystem-actions') as HTMLElement);
}

export function createTransfersModal(): void {
  transfersModal = new TransfersRecoveryModal(state.token, (takeId) => {
    void triggerPostProcess(takeId);
  });
}

/** Mount OPFS Recovery Banner if unfinished takes exist */
export function createRecoveryBanner(): void {
  recoveryBanner = new OPFSRecoveryBanner(state.token, () => {
    showToast('Local recovery updated. Check each track receipt and the host take status.');
  });
  // Unfinished local tracks (e.g. a tab closed mid-take) resume uploading on load [SL-P0-7].
  void recoveryBanner.checkAndMount(mainContainer).then(found => found ? recoveryBanner.resume() : undefined).catch(error => showToast(`Local recovery scan failed: ${String(error)}`));
}

export function onRecordStart(p: RecordStartPayload): void {
  state.currentTakeId = p.takeId;
  state.currentTakeNum = p.takeNumber;
  runCountdownAndStart(p);
}

export function runCountdownAndStart(p: RecordStartPayload): void {
  if(state.recordState!=='idle')return;
  stopDiagnosticClip();
  document.getElementById('take-status-badge')!.textContent=`Recording ${p.takeNumber} is about to start`;
  state.recordState='arming';recBtn.disabled=true;countdownOverlay.style.display='flex';devicePicker.refreshLock();recordingMic.refreshLock();
  const tick=()=>{
    const remaining=p.startAtHostUs-clockSync.toHostUs(performance.now());
    countdownVal.textContent=String(Math.max(1,Math.ceil(remaining/1e6)));
    if(remaining<=0){if(countdownTimer)clearInterval(countdownTimer);countdownTimer=null;countdownOverlay.style.display='none';void startLocalRecording(p.takeId);}
  };countdownTimer=window.setInterval(tick,50);tick();
}

export function reportCaptureFailure(error: unknown): void {
  if(captureFailureMessage)return;
  const reason = error instanceof Error ? error.message : String(error);
  captureFailureMessage = state.isHost ? 'Recording failed on this device, so it was stopped for everyone. What was saved so far is kept.' : 'Recording failed on this device. What was saved so far is kept for recovery.';
  telemetryStatus.textContent = captureFailureMessage;
  document.getElementById('take-status-badge')!.textContent = `Recording ${state.currentTakeNum} failed on this device`;
  if (state.isHost) {
    sendEnvelope({t: TypeRecordStop});
    showToast(`Recording stopped for everyone because it failed on this device: ${reason}. What was saved so far is kept; check it before editing.`);
  } else showToast(`Recording stopped on this device: ${reason}. The host will need to check this recording.`);
}

export function updateTrackProgress(tracks:ActiveRecordingTrack[]):void {
  if(recordingTracks!==tracks)return;
  const uploaded=tracks.reduce((sum,track)=>sum+(track.progress?.uploadedBytes||0),0);
  const buffered=tracks.reduce((sum,track)=>sum+(track.progress?.bufferedBytes||0),0);
  const error=tracks.find(track=>track.progress?.error);
  telemetryChunks.textContent=error?`${error.label}: ${error.progress?.error}`:`${(uploaded/1048576).toFixed(1)} MB saved on the host, ${(buffered/1048576).toFixed(1)} MB waiting to upload (${tracks.length} ${tracks.length===1?'file':'files'})`;
  renderUploadSummary(tracks);
}

/** Uploads in plain words for the Studio panel [SL-UX-2]; the byte counts stay in Technical details. */
export function renderUploadSummary(tracks:ActiveRecordingTrack[]):void {
  const summary=document.getElementById('studio-upload-summary');
  const bar=document.querySelector<HTMLElement>('.studio-upload-bar');
  if(!summary)return;
  const uploaded=tracks.reduce((sum,t)=>sum+(t.progress?.uploadedBytes||0),0);
  const buffered=tracks.reduce((sum,t)=>sum+(t.progress?.bufferedBytes||0),0);
  const done=tracks.filter(t=>t.progress?.isComplete).length;
  const error=tracks.find(t=>t.progress?.error);
  let text='Nothing to upload yet';let fraction:number|null=null;
  if(error)text=`Upload paused for ${error.label.toLowerCase()}. It keeps trying.`;
  else if(tracks.length&&done===tracks.length)text=tracks.length===1?'Your recording is saved on the host':'All recordings saved on the host';
  else if(tracks.length&&state.isRecording)text=uploaded>=1048576?`Sending to the host as you record (${(uploaded/1048576).toFixed(0)} MB so far)`:'Sending to the host as you record';
  else if(tracks.length){text=`Uploading ${Math.min(done+1,tracks.length)} of ${tracks.length}`;fraction=uploaded+buffered>0?uploaded/(uploaded+buffered):0;}
  summary.textContent=text;
  summary.dataset.state=error?'paused':tracks.length&&done===tracks.length?'done':tracks.length?'active':'idle';
  if(bar){bar.hidden=fraction===null;const fill=bar.firstElementChild as HTMLElement|null;if(fill&&fraction!==null)fill.style.width=`${Math.round(fraction*100)}%`;}
}

export async function createCaptureTrack(takeId:string,kind:'video'|'audio',mime:string,label:string,tracks:ActiveRecordingTrack[],deferredWavHeader=false,source?:'screen'):Promise<ActiveRecordingTrack> {
  const response=await fetch(`/api/v1/takes/${takeId}/tracks`,{method:'POST',headers:{Authorization:`Bearer ${state.uploadToken}`,'Content-Type':'application/json'},body:JSON.stringify({kind,mime,label,...(source?{source}:{})})});
  if(!response.ok)throw new Error(await response.text());
  const registered=await response.json();
  const capture={trackId:registered.id,kind,label,progress:null,recorder:null} as unknown as ActiveRecordingTrack;
  unconfirmedTracks.add(capture);
  capture.queue=new ProgressiveUploadQueue(takeId,capture.trackId,state.uploadToken,progress=>{
    capture.progress=progress;updateTrackProgress(tracks);if(progress.isComplete)unconfirmedTracks.delete(capture);
    sendEnvelope({t:'recorder_status',payload:{trackId:capture.trackId,state:progress.error?'error':progress.isComplete?'done':state.isRecording?'recording':'uploading',bufferedBytes:progress.bufferedBytes,uploadedBytes:progress.uploadedBytes,lastSeq:progress.lastSeq}});
  },{kind,deferredWavHeader,scheduler:callNetwork.scheduler});
  tracks.push(capture);await capture.queue.ready();return capture;
}

export async function finalizeCaptureTracks(tracks:ActiveRecordingTrack[]):Promise<boolean> {
  const results=await Promise.allSettled(tracks.map(track=>{
    // Stopping the recorder and completing the upload are separate steps, so
    // leaving can release the camera once recorders flush [SL-P1-6].
    track.stopped ||= track.recorder ? track.recorder.stop() : Promise.resolve(null);
    track.finalization ||= (async()=>{const stopped=await track.stopped;if(!stopped)return false;return await track.queue.complete(stopped.lastSeq,stopped.sidecar);})();
    return track.finalization;
  }));
  return results.length>0&&results.every(result=>result.status==='fulfilled'&&result.value);
}

export async function startLocalRecording(takeId:string):Promise<void> {
  const generation=++captureGeneration;captureFailureMessage='';
  const tracks:ActiveRecordingTrack[]=[];recordingTracks=tracks;
  let microphone:MediaStream|null=null;
  const cancel=async()=>{await finalizeCaptureTracks(tracks);microphone?.getTracks().forEach(track=>track.stop());};
  try {
    if(!state.localStream)throw new Error('No local camera/microphone');
    const fit=clockSync.getFit();
    const failure=(error:Error)=>{if(recordingTracks!==tracks)return;reportCaptureFailure(error);if(state.recordState==='recording')void stopLocalRecording();};
    const video=await createCaptureTrack(takeId,'video',LocalTrackRecorder.preferredMimeType('video',{videoCodec:state.activeSettings.video_codec}),'Camera + microphone',tracks);
    if(generation!==captureGeneration){await cancel();return;}
    // The camera file's muxed mic: raw or clean, as measured [SL-P1-10].
    const micProcessing=recordingMicFor(state.localStream.getAudioTracks()[0]);
    video.recorder=new LocalTrackRecorder(video.trackId,'video',chunk=>video.queue.enqueue(chunk),fit,{engine:'mediarecorder',videoBitrateBps:state.activeSettings.video_bitrate_bps,videoCodec:state.activeSettings.video_codec,targetFps:state.activeSettings.target_fps,micProcessing,onError:failure});
    let pcm:ActiveRecordingTrack|undefined;
    if(pcmToggle.checked){
      pcm=await createCaptureTrack(takeId,'audio','audio/wav','Uncompressed microphone',tracks,true);
      if(generation!==captureGeneration){await cancel();return;}
      const deviceId=state.localStream.getAudioTracks()[0]?.getSettings().deviceId||micSelect.value;
      // The uncompressed mic gets the same processing as the recording mic (raw or clean) [SL-P1-10].
      const processing=recordingProcessing();
      microphone=await openAudio(exact=>({...callAudioConstraints(deviceId||undefined,processing,exact),sampleRate:{ideal:48000},channelCount:{ideal:2}}));
      if(generation!==captureGeneration){await cancel();return;}
      microphone.getAudioTracks().forEach(track=>track.enabled=!state.isMicMuted);
      pcmMicStream=microphone;
      const queue=pcm.queue;
      pcm.recorder=new LocalPcmRecorder(pcm.trackId,chunk=>queue.enqueue(chunk),fit,{sampleRate:48000,replacesAudioOfTrackId:video.trackId,micProcessing:recordingMicFor(microphone.getAudioTracks()[0]),onError:failure});
    }
    await Promise.all([video.recorder.start(state.localStream),...(pcm&&microphone?[pcm.recorder!.start(microphone)]:[])]);
    if(generation!==captureGeneration){await cancel();return;}
    document.getElementById('take-status-badge')!.textContent=state.currentTakeNum>1?`Recording ${state.currentTakeNum} of this meeting`:'Everyone is recorded on their own device';
    state.recordState='recording';state.isRecording=true;recBtn.disabled=false;recBtn.textContent='Stop recording';callNetwork.kick();
    recIndicator.style.display='inline-flex';localIsoBadge.style.display='block';recLabel.textContent='Recording';recTimer.textContent='0:00';
    state.recStartTimeMs=Date.now();renderUploadSummary(tracks);telemetryStatus.textContent=pcm?'Recording on this device, with studio-quality audio':'Recording on this device';
    recTimerInterval=window.setInterval(()=>{recTimer.textContent=elapsed(Date.now()-state.recStartTimeMs)},1000);
    updateResolutionSpec();
    void screenShare.onTakeStarted();devicePicker.refreshLock();recordingMic.refreshLock();
  }catch(error){
    if(generation===captureGeneration){reportCaptureFailure(error);state.recordState='stopping';recBtn.disabled=true;}
    await cancel();
    if(recordingTracks===tracks){state.recordState='idle';state.isRecording=false;recBtn.disabled=false;recBtn.textContent='Start recording';recIndicator.style.display='none';localIsoBadge.style.display='none';if(recTimerInterval)clearInterval(recTimerInterval);if(captureFailureMessage)telemetryStatus.textContent=captureFailureMessage;}
  }
}

export async function stopLocalRecording(): Promise<void> {
  captureGeneration++;
  if(!captureFailureMessage)document.getElementById('take-status-badge')!.textContent=`Recording ${state.currentTakeNum} stopped`;
  if (countdownTimer) {clearInterval(countdownTimer);countdownTimer=null;countdownOverlay.style.display='none';state.recordState='idle';recBtn.disabled=false;return;}
  if (state.recordState === 'arming') {state.recordState='idle';recBtn.disabled=false;recBtn.textContent='Start recording';return;}
  if (!state.isRecording || !recordingTracks.length) return;
  document.getElementById('take-status-badge')!.textContent=`Recording ${state.currentTakeNum} stopped`;
  state.recordState='stopping';recBtn.disabled=true;
  state.isRecording = false;
  callNetwork.kick(); // uploads catch up at full speed now
  screenShare.onTakeStopped(); // its track is finalized with the others below
  recIndicator.style.display = 'none';
  localIsoBadge.style.display = 'none';
  telemetryStatus.innerText = 'Sending your recording to the host…';
  if (recTimerInterval) clearInterval(recTimerInterval);
  showToast('Recording stopped. Sending the files to the host…');

  try {
  const ok=await finalizeCaptureTracks(recordingTracks);
  if(ok){telemetryStatus.textContent='Your recording is saved on the host';showToast('Your recording is saved on the host.');}
  else {telemetryStatus.textContent='Part of your recording hasn’t reached the host yet. Keep this browser’s site data so it can be recovered.';showToast('Part of your recording hasn’t reached the host yet. Keep this browser’s site data so it can be recovered.');}
  renderUploadSummary(recordingTracks);
  pcmMicStream?.getTracks().forEach(track=>track.stop());pcmMicStream=null;
  openTransfersModal();
  } catch(err){showToast(`Couldn’t finish saving the recording: ${err}`);telemetryStatus.textContent='This recording needs recovery';}
  finally{state.recordState='idle';recBtn.disabled=false;recBtn.textContent='Start recording';if(captureFailureMessage)telemetryStatus.textContent=captureFailureMessage;devicePicker.refreshLock();recordingMic.refreshLock();void recordingMic.reapply();}
}

// Screen share recording: the shared screen (capture track, never received
// media) is recorded as its own late-registered "screen" source of the take,
// through the same OPFS + upload pipeline. Its sidecar carries its own start.
const SCREEN_RECORDING_BITRATE_BPS = 6_000_000;

export async function startScreenRecording(stream: MediaStream): Promise<ScreenRecording|null> {
  if(state.recordState!=='recording'||!state.currentTakeId)return null;
  const tracks=recordingTracks,takeId=state.currentTakeId;
  const mime=LocalTrackRecorder.preferredMimeType('video',{videoCodec:state.activeSettings.video_codec});
  const capture=await createCaptureTrack(takeId,'video',mime,'Screen',tracks,false,'screen');
  capture.recorder=new LocalTrackRecorder(capture.trackId,'video',chunk=>capture.queue.enqueue(chunk),clockSync.getFit(),{engine:'mediarecorder',videoBitrateBps:SCREEN_RECORDING_BITRATE_BPS,videoCodec:state.activeSettings.video_codec,onError:error=>showToast(`Screen recording failed: ${error.message}. Your camera recording continues.`)});
  await capture.recorder.start(stream);
  const finalize=()=>finalizeCaptureTracks([capture]);
  // The take ended while we registered: close this short segment right away.
  if(recordingTracks!==tracks||state.recordState!=='recording'){await finalize();return null;}
  return {finalize};
}

export async function triggerPostProcess(takeId: string): Promise<void> {
  const targetId = takeId || state.currentTakeId;
  if (!targetId) {
    showToast('Record something first');
    return;
  }
  telemetryStatus.innerText = 'Getting files for editing ready…';
  showToast('Getting files for editing ready…');
  // Live stage/percent while the export request runs [SL-P0-9].
  const stopProgress = pollTakeExport(targetId, state.token, v => { if (v.postproc?.state === 'running') telemetryStatus.textContent = describeExport(v.postproc); });

  try {
    const res = await fetch(`/api/v1/takes/${targetId}/postprocess`, { method: 'POST', headers:{Authorization:`Bearer ${state.token}`} });
    stopProgress();
    if (res.ok) {
      state.exportedTakeId = targetId;
      exportModal.style.display = 'flex';
      ecosystemActions.show(targetId, state.sessionId, state.token);
      closeModalBtn.focus();
      telemetryStatus.innerText = 'Files for editing are ready';
      transfersModal.hide();
      const location = document.getElementById('export-location');
      if (location) location.textContent = `In the host’s exports folder (<data-dir>/exports, or the folder set with --export-dir), under meeting ${state.sessionId}, recording ${targetId}.`;
      const files = document.getElementById('export-files');
      if (files) files.textContent = `Files: take_${String(state.currentTakeNum).padStart(2,'0')}_multicam.fcpxml, take_${String(state.currentTakeNum).padStart(2,'0')}_premiere.xml, manifest.json, video/ and audio/`;
      showToast('Files for editing are ready. Check timing and levels in your editor.');
    } else {
      telemetryStatus.textContent='Couldn’t get files for editing ready';
      showToast(`Couldn’t get files for editing ready: ${await res.text()}`);
    }
  } catch (err) {
    stopProgress();
    telemetryStatus.textContent='Couldn’t reach the host to get files for editing';
    showToast(`Couldn’t reach the host to get files for editing: ${String(err)}`);
  }
}

export function openTransfersModal(): void {
  if (leaveCall.current !== 'in-call') return; // the leave screen shows upload progress [SL-P1-6]
  const tracks:TrackTransferItem[]=recordingTracks.map(track=>({trackId:track.trackId,sourceLabel:track.label,kind:track.kind,participantName:nameInput.value||'You',localBytes:(track.progress?.uploadedBytes||0)+(track.progress?.bufferedBytes||0),serverBytes:track.progress?.uploadedBytes||0,status:track.progress?.isComplete?'assembled':track.progress?.error?'paused':'uploading',lastSeq:track.progress?.lastSeq??-1}));
  transfersModal.show({sessionTitle:displaySessionTitle.textContent||'Meeting',takeId:state.currentTakeId,takeNumber:state.currentTakeNum,durationSec:state.recStartTimeMs? (Date.now()-state.recStartTimeMs)/1000:0,participantsCount:state.latestDashboard?.participants.filter(p=>p.status==='admitted').length||1,tracks});
}

/** Transfers modal from the health aside and the dock. */
export function bindTransfersButtons(): void {
  if (viewTransfersAsideBtn) viewTransfersAsideBtn.onclick = openTransfersModal;
  if (dockTransfersBtn) dockTransfersBtn.onclick = openTransfersModal;
}

/** Export modal: close, and download the editor package (host only). */
export function bindExportModal(): void {
  closeModalBtn.onclick = () => {
    exportModal.style.display = 'none';
    ecosystemActions.stop();
    if (state.isHost) postprocBtn.focus();
  };

  openFolderBtn.onclick = () => {
    if (!state.isHost || !state.exportedTakeId) return;
    const a = document.createElement('a');
    a.href = `/api/v1/takes/${encodeURIComponent(state.exportedTakeId)}/download?token=${encodeURIComponent(state.token)}`;
    a.download = 'p2p.video-editor-package.zip';
    a.referrerPolicy = 'no-referrer';
    document.body.appendChild(a); a.click(); a.remove();
    showToast('Downloading files for editing. Unzip them before you open the project.');
  };
}

/** What is still uploading, for the leave screen [SL-P1-6]. */
export function uploadBacklog(): UploadBacklog {
  const tracks = [...unconfirmedTracks];
  const buffered = tracks.reduce((sum, t) => sum + (t.progress?.bufferedBytes || 0), 0);
  const snapshot = callNetwork.scheduler.snapshot();
  return {
    recording: state.recordState === 'arming' || state.recordState === 'recording',
    pendingBytes: Math.max(buffered, callNetwork.scheduler.pendingBytes),
    unfinishedTracks: tracks.length,
    rateBps: snapshot.rateBps,
    uploadedBytes: [...new Set([...recordingTracks, ...tracks])].reduce((sum, t) => sum + (t.progress?.uploadedBytes || 0), 0),
  };
}

/** Leaving: resolves once every recorder has flushed its last chunk. */
export async function recordersFlushed(): Promise<void> {
  await Promise.allSettled(recordingTracks.map(t => t.stopped ?? Promise.resolve(null)));
}

/** Leaving while idle: release the uncompressed-capture microphone. */
export function releasePcmMic(): void {
  pcmMicStream?.getTracks().forEach(t => t.stop()); pcmMicStream = null;
}
