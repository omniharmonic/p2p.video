import { ICONS } from './icons';
import { CaptionTap, type CaptionTapState } from '../lib/caption-tap';
import {
  CAPTIONS_LANGUAGE_KEY,
  CAPTION_LANGUAGES,
  CaptionBoard,
  readCaptionLanguage,
  readCaptionsVisible,
  writeCaptionsVisible,
} from '../lib/captions';
import {
  TypeCaption,
  TypeCaptionControl,
  TypeCaptionState,
  type AdmissionPayload,
  type CaptionPayload,
  type CaptionStatePayload,
  type Envelope,
} from '../lib/protocol';

export interface CaptionsHost {
  send(env: Envelope): void;
  selfId(): string;
  isHost(): boolean;
  sessionId(): string;
  token(): string;
  toast(message: string): void;
}


// Meet-style live captions [SL-P4-2]: a bottom overlay with the speaker's
// name and the last lines, a CC button and `c` shortcut for each viewer, a
// "Captions on" indicator for everyone (waiting room included), and for the
// host the room switch, language, the audio feed to the Mac's recognizer and
// the transcript download. main.ts forwards signaling messages to handle().
export class Captions {
  readonly button: HTMLButtonElement;
  private readonly hostMenuBtn: HTMLButtonElement;
  private readonly hostMenu: HTMLElement;
  private readonly overlay: HTMLElement;
  private readonly indicator: HTMLElement;
  private readonly waitingNotice: HTMLElement;
  private readonly transcriptDialog: HTMLDialogElement;
  private readonly board = new CaptionBoard();
  private readonly tap: CaptionTap;
  private state: CaptionStatePayload = { enabled: false, language: 'en-US', provider: false };
  private visible = readCaptionsVisible(safeStorage());
  private inCall = false;
  private finals = 0;
  private expiryTimer: number | null = null;
  private sourceTimer: number | null = null;
  private tapState: CaptionTapState = 'idle';

  constructor(private readonly host: CaptionsHost) {
    this.button = document.createElement('button');
    this.button.type = 'button';
    this.button.id = 'cc-btn';
    this.button.className = 'btn btn-secondary collab-btn cc-btn';
    this.button.hidden = true;
    this.button.innerHTML = `<span class="collab-icon" aria-hidden="true">${ICONS.cc}</span><span class="collab-label">Captions</span>`;
    this.button.onclick = () => this.toggle();

    this.hostMenuBtn = document.createElement('button');
    this.hostMenuBtn.type = 'button';
    this.hostMenuBtn.id = 'cc-menu-btn';
    this.hostMenuBtn.className = 'btn btn-secondary collab-btn collab-icon-only cc-menu-btn';
    this.hostMenuBtn.hidden = true;
    this.hostMenuBtn.setAttribute('aria-label', 'Caption settings');
    this.hostMenuBtn.setAttribute('aria-haspopup', 'true');
    this.hostMenuBtn.setAttribute('aria-expanded', 'false');
    this.hostMenuBtn.innerHTML = ICONS.caret;
    this.hostMenuBtn.onclick = () => this.toggleHostMenu();

    this.hostMenu = document.createElement('div');
    this.hostMenu.id = 'cc-menu';
    this.hostMenu.className = 'cc-menu';
    this.hostMenu.hidden = true;
    this.hostMenu.innerHTML = `
      <label class="cc-menu-row"><span>Live captions for everyone</span>
        <button type="button" role="switch" class="switch" id="cc-room-switch" aria-checked="false"><span></span></button></label>
      <label class="cc-menu-row" for="cc-language"><span>Spoken language</span><select id="cc-language"></select></label>
      <p class="cc-menu-status" id="cc-provider-status"></p>
      <div class="cc-menu-actions"><button type="button" class="btn btn-quiet" data-format="vtt">Download transcript (.vtt)</button><button type="button" class="btn btn-quiet" data-format="json">.json</button></div>
      <p class="cc-menu-note">Recognized on the host's Mac with on-device speech. Audio and text never leave it. Captions aren't part of the recording.</p>`;
    const lang = this.hostMenu.querySelector<HTMLSelectElement>('#cc-language')!;
    for (const l of CAPTION_LANGUAGES) lang.append(new Option(l.label, l.tag));
    lang.value = readCaptionLanguage(safeStorage());
    lang.onchange = () => {
      try { safeStorage()?.setItem(CAPTIONS_LANGUAGE_KEY, lang.value); } catch { /* not remembered */ }
      if (this.state.enabled) this.control(true);
    };
    this.hostMenu.querySelector<HTMLButtonElement>('#cc-room-switch')!.onclick = () => this.control(!this.state.enabled);
    for (const b of this.hostMenu.querySelectorAll<HTMLButtonElement>('[data-format]')) b.onclick = () => void this.download(b.dataset.format as 'vtt' | 'json');
    this.hostMenu.addEventListener('keydown', e => { if (e.key === 'Escape') { e.stopPropagation(); this.closeHostMenu(true); } });
    document.addEventListener('click', e => {
      if (!this.hostMenu.hidden && !this.hostMenu.contains(e.target as Node) && !this.hostMenuBtn.contains(e.target as Node)) this.closeHostMenu(false);
    });

    this.overlay = document.createElement('div');
    this.overlay.id = 'captions-overlay';
    this.overlay.className = 'captions-overlay';
    this.overlay.setAttribute('role', 'region');
    this.overlay.setAttribute('aria-label', 'Live captions');
    this.overlay.hidden = true;

    this.indicator = document.createElement('div');
    this.indicator.id = 'captions-indicator';
    this.indicator.className = 'captions-indicator';
    this.indicator.setAttribute('role', 'status');
    this.indicator.hidden = true;

    this.waitingNotice = document.createElement('div');
    this.waitingNotice.id = 'waiting-captions-notice';
    this.waitingNotice.className = 'captions-indicator captions-waiting';
    this.waitingNotice.setAttribute('role', 'status');
    this.waitingNotice.hidden = true;
    this.waitingNotice.textContent = 'Live captions are on in this meeting. What you say will be transcribed on the host’s computer.';

    this.transcriptDialog = document.createElement('dialog');
    this.transcriptDialog.id = 'captions-transcript-dialog';
    this.transcriptDialog.className = 'confirm-dialog';
    this.transcriptDialog.setAttribute('aria-labelledby', 'captions-transcript-title');
    this.transcriptDialog.innerHTML = `
      <h2 id="captions-transcript-title">Live caption transcript</h2>
      <p>The meeting’s captions are kept on this host until it restarts. Download them now.</p>
      <div class="confirm-actions"><button type="button" class="btn btn-secondary" data-close>Close</button>
      <button type="button" class="btn btn-secondary" data-format="json">Download .json</button>
      <button type="button" class="btn btn-primary" data-format="vtt">Download .vtt</button></div>`;
    this.transcriptDialog.querySelector<HTMLButtonElement>('[data-close]')!.onclick = () => this.transcriptDialog.close();
    for (const b of this.transcriptDialog.querySelectorAll<HTMLButtonElement>('[data-format]')) b.onclick = () => void this.download(b.dataset.format as 'vtt' | 'json');

    this.tap = new CaptionTap({
      url: () => {
        const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
        return `${proto}//${location.host}/api/v1/sessions/${encodeURIComponent(host.sessionId())}/captions/ingest?token=${encodeURIComponent(host.token())}`;
      },
      onState: s => { this.tapState = s; this.render(); },
    });
  }

  /** Insert the UI into the static page. Safe to call once. */
  mount(): void {
    const studio = document.getElementById('studio-view');
    const group = studio?.querySelector('.transport-collab') ?? studio?.querySelector('.dock');
    const wrap = document.createElement('div');
    wrap.className = 'cc-wrap';
    wrap.append(this.button, this.hostMenuBtn, this.hostMenu);
    group?.prepend(wrap);
    studio?.querySelector('.stage-with-inspector')?.append(this.overlay);
    studio?.prepend(this.indicator);
    document.querySelector('#waiting-room .workspace-heading')?.after(this.waitingNotice);
    document.body.append(this.transcriptDialog);
  }

  onAdmission(adm: AdmissionPayload): void {
    this.inCall = adm.status === 'admitted';
    if (!this.inCall) this.board.clear();
    this.syncTap();
    this.render();
  }

  /** Returns true when the message was a captions message. */
  handle(env: Envelope): boolean {
    if (env.t === TypeCaptionState) {
      const was = this.state.enabled;
      this.state = env.payload as CaptionStatePayload;
      if (!this.state.enabled) this.board.clear();
      if (this.inCall && was !== this.state.enabled) {
        this.host.toast(this.state.enabled ? 'Live captions are on for this meeting.' : 'Live captions are off.');
      }
      this.syncTap();
      this.render();
      return true;
    }
    if (env.t === TypeCaption) {
      const c = env.payload as CaptionPayload;
      if (c.isFinal && c.text) this.finals++;
      if (this.board.apply(c, Date.now())) this.renderLines();
      this.scheduleExpiry();
      return true;
    }
    return false;
  }

  /** The `c` shortcut and the CC button. */
  toggle(): void {
    if (!this.inCall) return;
    if (!this.state.enabled) {
      if (this.host.isHost()) { this.visible = true; writeCaptionsVisible(safeStorage(), true); this.control(true); }
      else this.host.toast('The host hasn’t turned on captions.');
      return;
    }
    this.visible = !this.visible;
    writeCaptionsVisible(safeStorage(), this.visible);
    this.render();
  }

  /** The meeting ended: stop the feed; offer the host the transcript. */
  sessionEnded(): void {
    const offer = this.host.isHost() && this.finals > 0;
    this.inCall = false;
    this.state = { ...this.state, enabled: false };
    this.board.clear();
    this.syncTap();
    this.render();
    if (offer && !this.transcriptDialog.open) this.transcriptDialog.showModal();
  }

  private control(enabled: boolean): void {
    if (!this.host.isHost()) return;
    const language = this.hostMenu.querySelector<HTMLSelectElement>('#cc-language')!.value;
    if (enabled && !this.state.provider) this.host.toast('Live captions need the p2p.video Mac app running on the host with “Recognize live captions” on.');
    this.host.send({ t: TypeCaptionControl, payload: { enabled, language } });
  }

  private async download(format: 'vtt' | 'json'): Promise<void> {
    try {
      const res = await fetch(`/api/v1/sessions/${encodeURIComponent(this.host.sessionId())}/captions/transcript?format=${format}`, { headers: { Authorization: `Bearer ${this.host.token()}` } });
      if (!res.ok) throw new Error(String(res.status));
      const url = URL.createObjectURL(await res.blob());
      const a = document.createElement('a');
      a.href = url;
      a.download = `captions-${this.host.sessionId()}.${format}`;
      document.body.append(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 10_000);
    } catch {
      this.host.toast('The caption transcript could not be downloaded.');
    }
  }

  /** Host only: feed call audio to the daemon while captions are on. */
  private syncTap(): void {
    const want = this.inCall && this.host.isHost() && this.state.enabled;
    if (want && !this.tap.active) {
      void this.tap.start().then(() => this.refreshSources()).catch(() => this.host.toast('Live captions could not read call audio in this browser.'));
      this.sourceTimer = window.setInterval(() => this.refreshSources(), 1000);
    } else if (!want && this.tap.active) {
      this.tap.stop();
      if (this.sourceTimer !== null) { clearInterval(this.sourceTimer); this.sourceTimer = null; }
    }
  }

  /** Each participant's call audio: remote tiles (stream ID = participant ID) and the host's own mic. */
  private refreshSources(): void {
    const sources = new Map<string, MediaStreamTrack>();
    for (const tile of document.querySelectorAll<HTMLElement>('#video-grid .tile[data-participant-id]')) {
      const id = tile.dataset.participantId || '';
      const stream = tile.querySelector('video')?.srcObject;
      const track = stream instanceof MediaStream ? stream.getAudioTracks()[0] : undefined;
      if (id && track) sources.set(id, track);
    }
    const local = (document.getElementById('local-video') as HTMLVideoElement | null)?.srcObject;
    const mic = local instanceof MediaStream ? local.getAudioTracks()[0] : undefined;
    if (mic && this.host.selfId()) sources.set(this.host.selfId(), mic);
    void this.tap.setSources(sources);
  }

  private scheduleExpiry(): void {
    if (this.expiryTimer !== null) return;
    this.expiryTimer = window.setInterval(() => {
      if (this.board.expire(Date.now())) this.renderLines();
      if (!this.board.lines().length && this.expiryTimer !== null) { clearInterval(this.expiryTimer); this.expiryTimer = null; }
    }, 1000);
  }

  private render(): void {
    const on = this.state.enabled;
    const host = this.host.isHost();
    this.button.hidden = !this.inCall;
    this.button.classList.toggle('active', on && this.visible);
    this.button.setAttribute('aria-pressed', String(on && this.visible));
    const label = !on ? (host ? 'Turn on captions' : 'Captions (off)') : this.visible ? 'Hide captions' : 'Show captions';
    this.button.title = `${label} (c)`;
    this.button.setAttribute('aria-label', label);
    this.hostMenuBtn.hidden = !this.inCall || !host;
    if (this.hostMenuBtn.hidden) this.closeHostMenu(false);
    const sw = this.hostMenu.querySelector<HTMLElement>('#cc-room-switch')!;
    sw.setAttribute('aria-checked', String(on));
    const status = this.hostMenu.querySelector<HTMLElement>('#cc-provider-status')!;
    status.textContent = !this.state.provider
      ? 'Recognizer not connected: open the p2p.video Mac app on the host and turn on Settings › Live captions.'
      : !on ? `Ready (${this.state.providerName || 'on-device speech'}).`
      : this.tapState === 'streaming' ? `Captioning in ${this.state.language}.`
      : this.tapState === 'error' ? 'Reconnecting the audio feed…' : 'Starting…';
    status.classList.toggle('warn', !this.state.provider);

    this.indicator.hidden = !this.inCall || !on;
    this.indicator.innerHTML = '<span class="captions-indicator-badge" aria-hidden="true">CC</span><strong>Captions on</strong><span class="captions-indicator-detail"></span>';
    this.indicator.querySelector('.captions-indicator-detail')!.textContent = this.state.provider
      ? 'Speech in this meeting is transcribed on the host’s computer, not in the cloud.'
      : 'Waiting for the host’s on-device recognizer.';
    this.waitingNotice.hidden = this.inCall || !on;
    this.overlay.hidden = !this.inCall || !on || !this.visible;
    this.renderLines();
  }

  private renderLines(): void {
    const self = this.host.selfId();
    const rows = this.board.lines().map((l, i, all) => {
      const row = document.createElement('div');
      row.className = 'caption-line' + (l.isFinal ? '' : ' partial');
      row.dataset.participantId = l.participantId;
      if (i === 0 || all[i - 1].participantId !== l.participantId) {
        const who = document.createElement('span');
        who.className = 'caption-speaker';
        who.textContent = l.participantId === self ? 'You' : l.name || 'Participant';
        row.append(who);
      }
      const text = document.createElement('span');
      text.className = 'caption-text';
      text.textContent = l.text;
      row.append(text);
      return row;
    });
    this.overlay.replaceChildren(...rows);
    this.overlay.classList.toggle('empty', rows.length === 0);
  }

  private toggleHostMenu(): void {
    if (this.hostMenu.hidden) {
      this.hostMenu.hidden = false;
      this.hostMenuBtn.setAttribute('aria-expanded', 'true');
      this.hostMenu.querySelector<HTMLElement>('#cc-room-switch')?.focus();
    } else this.closeHostMenu(true);
  }

  private closeHostMenu(focus: boolean): void {
    if (this.hostMenu.hidden) return;
    this.hostMenu.hidden = true;
    this.hostMenuBtn.setAttribute('aria-expanded', 'false');
    if (focus) this.hostMenuBtn.focus();
  }
}

function safeStorage(): Storage | null {
  try { return window.localStorage; } catch { return null; }
}
