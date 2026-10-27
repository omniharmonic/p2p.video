// Small shared UI helpers: toasts, the connection pill, the resolution spec and
// tile mic/camera badges [SL-TECH-1].
import { LocalPcmRecorder } from '../lib/pcm-recorder';
import { OPFSStorage } from '../lib/opfs';
import { resolutionSpec, telemetryStorage, toastContainer } from './dom';
import { recordingTracks } from './recording';
import { ws } from './signaling';
import { decodedRemoteVideos, state } from './state';

export function showToast(msg: string): void {
  const toast = document.createElement('div');
  toast.className = 'toast';
  toast.innerText = msg;
  toastContainer.appendChild(toast);
  while (toastContainer.children.length > 2) toastContainer.firstElementChild?.remove();
  setTimeout(() => {
    toast.remove();
  }, 4000);
}

export function updateConnectionPill(): void {
  const text = document.getElementById('conn-text');
  const dot = document.querySelector<HTMLElement>('#connection-pill .status-dot');
  const signaling = ws?.readyState === WebSocket.OPEN;
  if (text) text.textContent = !signaling ? (state.hasJoined ? 'Signaling disconnected' : 'Not connected') :
    decodedRemoteVideos.size > 0 && state.peerState === 'connected' ? 'Remote video decoded' :
    state.peerState === 'connected' ? 'Peer connected · awaiting remote video' : `Signaling connected · media ${state.peerState}`;
  const level = !signaling ? (state.hasJoined ? 'lost' : 'idle') : decodedRemoteVideos.size && state.peerState === 'connected' ? 'ok' : state.peerState === 'failed' || state.peerState === 'disconnected' ? 'lost' : 'connecting';
  document.getElementById('connection-pill')?.setAttribute('data-state', level);
  if (dot) dot.style.background = level === 'ok' ? 'var(--sl-ok)' : level === 'lost' ? 'var(--sl-rec)' : level === 'connecting' ? 'var(--sl-warn)' : 'var(--sl-text-3)';
}

export function updateResolutionSpec(): void {
  const v = state.localStream?.getVideoTracks()[0]?.getSettings();
  const settings = v ? `${v.width ?? '?'} × ${v.height ?? '?'}, ${v.frameRate?.toFixed(0) ?? '?'} fps` : 'Camera not ready';
  const pcm=recordingTracks.find(track=>track.recorder instanceof LocalPcmRecorder)?.recorder as LocalPcmRecorder|undefined;
  const audio=pcm?.getStats();
  if (resolutionSpec) resolutionSpec.textContent = `This device records ${settings}${audio?.sampleRate?`, plus studio-quality audio at ${audio.sampleRate/1000} kHz`:''}`;
  const previewLabel = document.getElementById('preview-spec');
  if (previewLabel) previewLabel.textContent = `${settings}`;
  if (telemetryStorage) void OPFSStorage.getInstance().backend().then(backend => {
    telemetryStorage.textContent = backend === 'memory' ? 'Private browsing: your recording is kept in memory until it reaches the host' : 'Your recording is kept safe in this browser until the host has it';
  });
}

export function renderTileState(tile: Element, tileState: { micMuted: boolean; camOff: boolean } | undefined): void {
  const mic = tile.querySelector<HTMLElement>('.tile-state-mic');
  const cam = tile.querySelector<HTMLElement>('.tile-state-cam');
  if (mic) mic.hidden = !tileState?.micMuted;
  if (cam) cam.hidden = !tileState?.camOff;
  tile.classList.toggle('cam-off', !!tileState?.camOff);
}
