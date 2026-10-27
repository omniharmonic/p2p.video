import { ICONS } from './icons';
import { soundUnlock } from './SoundUnlock';
import {
  ScreenStreamSuffix,
  SourceKindScreen,
  TypeActionError,
  TypeRoster,
  TypeScreenStop,
  TypeSdpOffer,
  TypeSourceAdd,
  TypeSourceRemove,
  type ActionErrorPayload,
  type Envelope,
  type RosterPayload,
  type ScreenStopPayload,
  type SdpPayload,
  type SourceAddPayload,
} from '../lib/protocol';
import type { WebRTCClient } from '../lib/rtc';

/** A screen recording registered in the current take. */
export interface ScreenRecording {
  /** Stops the recorder and completes its upload; true when the host assembled it. */
  finalize(): Promise<boolean>;
}

export interface ScreenShareHost {
  send(env: Envelope): void;
  selfId(): string;
  isHost(): boolean;
  rtc: WebRTCClient;
  toast(message: string): void;
  /**
   * Registers a late "screen" track in the current take and starts its local
   * recorder (own OPFS chunks, upload queue and sidecar). Resolves null when
   * no take is recording on this device.
   */
  startRecording(stream: MediaStream): Promise<ScreenRecording | null>;
}

const ACK_TIMEOUT_MS = 6000;
const SOURCE_ID = 'screen';

// Screen share (ROADMAP P1, Meet parity). Flow for the presenter:
//   getDisplayMedia (user gesture) → source_add {trackIds} → wait for the
//   roster to show us presenting (or action_error conflict) → add the tracks
//   to the existing PeerConnection and renegotiate (rtc.addScreen).
// The browser's own "Stop sharing" bar ends the video track; we treat that
// exactly like our Stop button. While a take records, the screen is also
// recorded locally as its own ISO source from the capture track (never from
// anything received over WebRTC).
export class ScreenShare {
  readonly button: HTMLButtonElement;
  private stream: MediaStream | null = null;
  private presenterId = '';
  private names = new Map<string, string>();
  private recording: ScreenRecording | null = null;
  private starting = false;
  private ack: { resolve: (ok: boolean) => void; timer: ReturnType<typeof setTimeout> } | null = null;
  private readonly grid: HTMLElement;

  constructor(private readonly host: ScreenShareHost, grid: HTMLElement) {
    this.grid = grid;
    this.button = document.createElement('button');
    this.button.type = 'button';
    this.button.id = 'screen-share-btn';
    this.button.className = 'btn btn-secondary collab-btn screen-share-btn';
    this.button.setAttribute('aria-pressed', 'false');
    this.button.innerHTML = `<span class="collab-icon" aria-hidden="true">${ICONS.screen}</span><span class="collab-label">Present</span>`;
    this.button.onclick = () => void (this.stream ? this.stop('user') : this.start());
    host.rtc.onRenegotiationOffer = sdp => host.send({ t: TypeSdpOffer, payload: { sdp, type: 'offer', renegotiate: true } as SdpPayload });
    if (typeof navigator === 'undefined' || !navigator.mediaDevices || typeof navigator.mediaDevices.getDisplayMedia !== 'function') {
      this.button.disabled = true;
      this.button.title = 'This browser cannot share a screen';
    }
  }

  get presenting(): boolean { return !!this.stream; }

  /** The current presenter's participant ID ('' when nobody presents). */
  get presenter(): string { return this.presenterId; }

  async start(): Promise<void> {
    if (this.stream || this.starting) return;
    if (this.presenterId && this.presenterId !== this.host.selfId()) {
      this.host.toast(`${this.names.get(this.presenterId) || 'Someone'} is presenting. Ask them or the host to stop first.`);
      return;
    }
    this.starting = true;
    let stream: MediaStream;
    try {
      // Called straight from the click, so the browser's user-activation
      // requirement holds. System/tab audio is offered where supported.
      stream = await navigator.mediaDevices.getDisplayMedia({
        video: { frameRate: { ideal: 15, max: 30 }, width: { max: 1920 }, height: { max: 1080 } },
        audio: true,
        selfBrowserSurface: 'exclude',
        surfaceSwitching: 'include',
        systemAudio: 'include',
      } as DisplayMediaStreamOptions);
    } catch (error) {
      this.starting = false;
      const name = (error as DOMException)?.name;
      if (name !== 'NotAllowedError' && name !== 'AbortError') this.host.toast(`Screen sharing failed: ${(error as Error)?.message || name}`);
      return;
    }
    const video = stream.getVideoTracks()[0];
    if (!video) { stream.getTracks().forEach(t => t.stop()); this.starting = false; return; }
    // Text and UI: favour sharpness over motion (call encoder and recorder).
    try { video.contentHint = 'detail'; } catch { /* Older engines. */ }
    // The browser's own "Stop sharing" control ends the capture track.
    video.addEventListener('ended', () => void this.stop('ended'));
    this.stream = stream;
    this.render();
    const accepted = await this.declare(stream, true);
    this.starting = false;
    if (this.stream !== stream) return; // stopped meanwhile
    if (!accepted) {
      this.stream = null;
      stream.getTracks().forEach(t => t.stop());
      this.render();
      return;
    }
    this.host.rtc.addScreen(stream);
    this.showLocalTile(stream);
    this.render();
    this.host.toast(stream.getAudioTracks().length ? 'You are presenting, with the shared audio' : 'You are presenting to everyone');
    const recording = await this.host.startRecording(stream).catch(error => {
      this.host.toast(`Screen recording could not start: ${error instanceof Error ? error.message : String(error)}. The call share continues.`);
      return null;
    });
    if (this.stream === stream) this.recording = recording;
    else void recording?.finalize();
  }

  /** reason: user (our button), ended (browser bar / capture ended), host, rejected. */
  async stop(reason: 'user' | 'ended' | 'host' | 'rejected'): Promise<void> {
    const stream = this.stream;
    if (!stream) return;
    this.stream = null;
    this.settleAck(false);
    stream.getTracks().forEach(t => t.stop());
    this.host.rtc.removeScreen();
    if (reason !== 'rejected') this.host.send({ t: TypeSourceRemove, payload: { sourceId: SOURCE_ID, kind: SourceKindScreen } });
    document.getElementById('local-screen-tile')?.remove();
    this.render();
    const recording = this.recording;
    this.recording = null;
    if (recording) {
      const ok = await recording.finalize().catch(() => false);
      this.host.toast(ok ? 'Presentation stopped. The screen recording for this take is saved.' : 'Presentation stopped. The screen recording needs recovery; keep this browser’s site data.');
    }
  }

  /**
   * Called by main.ts just before a full (re)offer: join after a reconnect, or
   * media recovery. Re-declares and republishes an ongoing share on the new
   * PeerConnection without a separate renegotiation.
   */
  beforeOffer(): void {
    const stream = this.stream;
    if (!stream || this.starting) return;
    this.declare(stream, false);
    this.host.rtc.addScreen(stream, false);
  }

  /** A take started on this device: record the ongoing share as its own source. */
  async onTakeStarted(): Promise<void> {
    const stream = this.stream;
    if (!stream || this.recording) return;
    const recording = await this.host.startRecording(stream).catch(error => {
      this.host.toast(`Screen recording could not start: ${error instanceof Error ? error.message : String(error)}`);
      return null;
    });
    if (this.stream === stream) this.recording = recording;
    else void recording?.finalize();
  }

  /** The take stopped; main.ts finalizes every track of the take, including ours. */
  onTakeStopped(): void {
    this.recording = null;
  }

  /** Observes signaling (does not consume it: CallFeatures also handles roster/errors). */
  handle(env: Envelope): void {
    if (env.t === TypeRoster) {
      const roster = env.payload as RosterPayload;
      this.names = new Map(roster.participants.map(p => [p.participantId, p.name]));
      this.presenterId = roster.participants.find(p => p.screenSharing)?.participantId ?? '';
      if (this.presenterId && this.presenterId === this.host.selfId()) this.settleAck(true);
      this.relabel();
      this.render();
    } else if (env.t === TypeActionError) {
      const err = env.payload as ActionErrorPayload;
      if (err.action !== TypeSourceAdd) return;
      if (this.ack) this.settleAck(false);
      else if (this.stream) void this.stop('rejected');
    } else if (env.t === TypeScreenStop) {
      const p = env.payload as ScreenStopPayload;
      if (p.participantId === this.host.selfId() && this.stream) {
        void this.stop('host');
        this.host.toast(`${p.by || 'The host'} stopped your presentation.`);
      }
    }
  }

  /** Remote screen track from the SFU (stream ID participantId + suffix). */
  attachRemote(track: MediaStreamTrack, stream: MediaStream): void {
    const owner = stream.id.slice(0, -ScreenStreamSuffix.length);
    const id = `screen-tile-${owner}`;
    let tile = document.getElementById(id);
    if (!tile) {
      tile = this.makeTile(id, owner, false);
      this.grid.appendChild(tile);
    }
    const video = tile.querySelector('video')!;
    if (video.srcObject !== stream) video.srcObject = stream;
    soundUnlock.ensurePlaying(video); // Safari autoplay policy [SL-P1-12]
    this.relabel();
    const prune = () => setTimeout(() => {
      const current = document.getElementById(id);
      if (!current || current.querySelector('video')?.srcObject !== stream) return;
      if (stream.getTracks().some(t => t.readyState === 'live')) return;
      current.remove();
    }, 0);
    track.addEventListener('ended', prune);
    stream.addEventListener('removetrack', prune);
  }

  /** Media path rebuilt: remote screen tiles return with the new subscriptions. */
  clearRemote(): void {
    for (const tile of this.grid.querySelectorAll('.tile-screen:not(.tile-screen-local)')) tile.remove();
  }

  private declare(stream: MediaStream, wait: boolean): Promise<boolean> {
    const payload: SourceAddPayload = { sourceId: SOURCE_ID, kind: SourceKindScreen, label: 'Screen', trackIds: stream.getTracks().map(t => t.id) };
    this.settleAck(false);
    const result = wait
      ? new Promise<boolean>(resolve => {
        this.ack = { resolve, timer: setTimeout(() => { this.host.toast('The host did not confirm the presentation. Try again.'); this.settleAck(false); }, ACK_TIMEOUT_MS) };
      })
      : Promise.resolve(true);
    this.host.send({ t: TypeSourceAdd, payload });
    return result;
  }

  private settleAck(ok: boolean): void {
    const ack = this.ack;
    if (!ack) return;
    this.ack = null;
    clearTimeout(ack.timer);
    ack.resolve(ok);
  }

  private showLocalTile(stream: MediaStream): void {
    document.getElementById('local-screen-tile')?.remove();
    const tile = this.makeTile('local-screen-tile', this.host.selfId(), true);
    const video = tile.querySelector('video')!;
    video.muted = true; // never play our own shared audio back
    video.srcObject = new MediaStream(stream.getVideoTracks());
    this.grid.appendChild(tile);
    this.relabel();
  }

  private makeTile(id: string, owner: string, local: boolean): HTMLElement {
    const tile = document.createElement('div');
    tile.className = `tile tile-screen${local ? ' tile-screen-local' : ''}`;
    tile.id = id;
    tile.dataset.screenOf = owner;
    const video = document.createElement('video');
    video.autoplay = true;
    video.playsInline = true;
    const name = document.createElement('div');
    name.className = 'tile-name tile-screen-name';
    tile.append(video, name);
    if (local) {
      const note = document.createElement('div');
      note.className = 'tile-screen-note';
      note.textContent = 'You are presenting to everyone';
      tile.append(note);
    }
    const stop = document.createElement('button');
    stop.type = 'button';
    stop.className = 'tile-screen-stop';
    stop.textContent = 'Stop presenting';
    stop.onclick = (event) => {
      event.stopPropagation();
      if (local) void this.stop('user');
      else this.host.send({ t: TypeScreenStop, payload: { participantId: owner } as ScreenStopPayload });
    };
    // Only the presenter or the host can stop a share (the host enforces it).
    stop.hidden = !local && !this.host.isHost();
    tile.append(stop);
    return tile;
  }

  private relabel(): void {
    for (const tile of this.grid.querySelectorAll<HTMLElement>('.tile-screen')) {
      const owner = tile.dataset.screenOf || '';
      const local = tile.classList.contains('tile-screen-local');
      const label = tile.querySelector<HTMLElement>('.tile-screen-name');
      if (label) label.textContent = local ? 'Your screen' : `${this.names.get(owner) || 'Someone'} is presenting`;
      const stop = tile.querySelector<HTMLButtonElement>('.tile-screen-stop');
      if (stop) {
        stop.hidden = !local && !this.host.isHost();
        stop.setAttribute('aria-label', local ? 'Stop presenting' : `Stop ${this.names.get(owner) || 'this'} presentation`);
      }
    }
  }

  private render(): void {
    const on = !!this.stream;
    this.button.setAttribute('aria-pressed', String(on));
    this.button.classList.toggle('active', on);
    this.button.querySelector('.collab-label')!.textContent = on ? 'Stop presenting' : 'Present';
    const other = this.presenterId && this.presenterId !== this.host.selfId() ? this.names.get(this.presenterId) || 'Someone' : '';
    this.button.title = on ? 'Stop sharing your screen' : other ? `${other} is presenting` : 'Share a screen, window or tab';
  }
}
