// Host-only livestream drawer: WYSIWYG compositor preview, destinations
// (YouTube / Twitch / custom RTMP), go-live / end and live health.
// Integration from main.ts is intentionally tiny: see mountLivestream().
import '../livestream.css';
import { escapeHtml } from '../lib/html';
import { Compositor, type CompositorSource, type LayoutMode } from '../lib/compositor';
import { BrandDecorator, loadLogo } from '../lib/programbrand';
import { brandStore } from '../lib/branding';
import {
  LivestreamClient,
  PLATFORM_PRESETS,
  bitrateLabel,
  destinationLine,
  validateDestination,
  type LivestreamClientState,
  type LivestreamDestination,
  type Platform,
} from '../lib/livestream';

export interface LivestreamHandle {
  /** Shows the toolbar button only for the host. */
  setHost(isHost: boolean): void;
  open(): void;
  close(): void;
}

export interface LivestreamSession { sessionId: string; token: string; }

/** Reads compositor sources from the studio video grid in its on-screen order. */
export function participantsFromGrid(grid: HTMLElement | null): CompositorSource[] {
  if (!grid) return [];
  const out: CompositorSource[] = [];
  for (const tile of grid.querySelectorAll<HTMLElement>('.tile')) {
    const videoEl = tile.querySelector('video');
    const stream = videoEl?.srcObject instanceof MediaStream ? videoEl.srcObject : null;
    if (!videoEl || !stream) continue;
    const label = tile.querySelector('.tile-name')?.textContent?.trim() || 'Guest';
    out.push({
      participantId: tile.dataset.participantId || tile.id || 'local',
      name: label.replace(/\s*(\/\s*(Host|Participant)|\(Producer, not recorded\))$/, ''),
      videoEl,
      audioTrack: stream.getAudioTracks().find(t => t.readyState === 'live' && t.enabled) ?? null,
    });
  }
  return out;
}

function sessionFromUrl(): LivestreamSession {
  const p = new URLSearchParams(location.search);
  return { sessionId: p.get('session') || '', token: p.get('token') || '' };
}

interface DestinationRow { platform: Platform; url: string; key: string; }

const PRESETS = { '720p30': { width: 1280, height: 720 }, '1080p30': { width: 1920, height: 1080 } } as const;
type PresetName = keyof typeof PRESETS;

class LivestreamPanel implements LivestreamHandle {
  private button: HTMLButtonElement;
  private backdrop: HTMLElement;
  private rows: DestinationRow[] = [{ platform: 'youtube', url: PLATFORM_PRESETS.youtube.url, key: '' }];
  private layout: LayoutMode = 'grid';
  private preset: PresetName = '720p30';
  private compositor: Compositor | null = null;
  private client: LivestreamClient | null = null;
  private state: LivestreamClientState | null = null;
  private isOpen = false;
  // Branded program and lower thirds [SL-P5-2].
  private readonly decorator = new BrandDecorator();
  private logoVersion = '';
  private cueTimer: number | null = null;

  constructor(anchor: HTMLElement, private getParticipants: () => CompositorSource[], private getSession: () => LivestreamSession) {
    this.button = document.createElement('button');
    this.button.id = 'livestream-btn';
    this.button.className = 'btn btn-quiet livestream-toolbar-btn';
    this.button.textContent = 'Go live';
    this.button.style.display = 'none';
    this.button.addEventListener('click', () => this.open());
    anchor.appendChild(this.button);

    this.backdrop = document.createElement('div');
    this.backdrop.id = 'livestream-panel';
    this.backdrop.className = 'livestream-backdrop';
    this.backdrop.innerHTML = this.skeleton();
    document.body.appendChild(this.backdrop);
    this.backdrop.addEventListener('click', e => { if (e.target === this.backdrop) this.close(); });
    document.addEventListener('keydown', e => { if (e.key === 'Escape' && this.isOpen) this.close(); });
    this.q('#livestream-close-btn').addEventListener('click', () => this.close());
    this.q('#livestream-add-dest').addEventListener('click', () => { if (this.rows.length < 4) { this.rows.push({ platform: 'twitch', url: PLATFORM_PRESETS.twitch.url, key: '' }); this.renderRows(); } });
    this.q('#livestream-go').addEventListener('click', () => void this.goLive());
    this.q('#livestream-end').addEventListener('click', () => void this.client?.stop());
    for (const b of this.backdrop.querySelectorAll<HTMLButtonElement>('[data-layout]')) b.addEventListener('click', () => this.setLayout(b.dataset.layout as LayoutMode));
    this.q<HTMLSelectElement>('#livestream-preset').addEventListener('change', e => { this.preset = (e.target as HTMLSelectElement).value as PresetName; this.restartPreview(); });
    this.q('#livestream-branded').addEventListener('click', () => this.setBranded(!this.decorator.branded));
    this.q('#livestream-lt-cues').addEventListener('click', e => {
      const b = (e.target as HTMLElement).closest<HTMLButtonElement>('[data-cue]');
      if (b?.dataset.cue) this.decorator.cue(b.dataset.cue);
    });
    brandStore.subscribe(() => { void this.syncLogo(); this.renderCues(); });
    this.renderRows();
    this.renderStatus();
  }

  private setBranded(on: boolean): void {
    this.decorator.branded = on;
    const sw = this.q('#livestream-branded');
    sw.setAttribute('aria-checked', String(on));
    void this.syncLogo();
  }

  /** Loads the logo again when its version changes (host token in a header, never a URL). */
  private async syncLogo(): Promise<void> {
    const version = brandStore.get().logoVersion || '';
    if (version === this.logoVersion) return;
    this.logoVersion = version;
    if (!version) { this.decorator.logo = null; return; }
    try {
      const s = this.getSession();
      const bmp = await loadLogo(s.sessionId, s.token);
      if (this.logoVersion === version) this.decorator.logo = bmp;
    } catch { this.decorator.logo = null; }
  }

  /** On-demand lower-third buttons for the people in the program. */
  private renderCues(): void {
    const holder = this.q('#livestream-lt-cues');
    const mode = brandStore.get().lowerThirdsMode;
    const hint = this.q('#livestream-lt-hint');
    hint.textContent = mode === 'off' ? 'Lower thirds are off (Branding & titles).'
      : mode === 'manual' ? 'Shown only when you press a name.'
      : 'Shown when someone becomes the active speaker, or when you press a name.';
    const people = this.getParticipants();
    const key = people.map(p => p.participantId + '\u0000' + brandStore.text(p.participantId, p.name).name).join('\u0001') + mode;
    if (holder.dataset.key === key) return;
    holder.dataset.key = key;
    holder.replaceChildren(...people.map(p => {
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'tactile-pill';
      b.dataset.cue = p.participantId;
      b.disabled = mode === 'off';
      b.textContent = brandStore.text(p.participantId, p.name).name || p.name;
      return b;
    }));
  }

  private q<T extends HTMLElement = HTMLElement>(sel: string): T { return this.backdrop.querySelector(sel) as T; }

  setHost(isHost: boolean): void {
    this.button.style.display = isHost ? 'inline-flex' : 'none';
    if (!isHost) this.close();
  }

  open(): void {
    this.isOpen = true;
    this.backdrop.classList.add('open');
    this.ensurePreview();
    this.renderCues();
    if (this.cueTimer === null) this.cueTimer = window.setInterval(() => this.renderCues(), 2000);
    this.q('#livestream-close-btn').focus();
  }

  close(): void {
    if (!this.isOpen) return;
    this.isOpen = false;
    this.backdrop.classList.remove('open');
    if (this.cueTimer !== null) { clearInterval(this.cueTimer); this.cueTimer = null; }
    // Keep compositing while live; otherwise release the preview.
    if (!this.isActive()) this.stopPreview();
    this.button.focus();
  }

  private isActive(): boolean {
    const phase = this.state?.phase;
    return phase === 'starting' || phase === 'live' || phase === 'reconnecting' || phase === 'stopping';
  }

  private ensurePreview(): void {
    if (this.compositor) return;
    const size = PRESETS[this.preset];
    this.compositor = new Compositor(this.getParticipants, { ...size, fps: 30, layout: this.layout, decorator: this.decorator });
    this.compositor.start();
    const holder = this.q('#livestream-preview');
    holder.replaceChildren(this.compositor.canvas);
  }

  private stopPreview(): void {
    this.compositor?.stop();
    this.compositor = null;
    this.q('#livestream-preview').replaceChildren();
  }

  private restartPreview(): void {
    if (this.isActive()) return;
    this.stopPreview();
    if (this.isOpen) this.ensurePreview();
  }

  private setLayout(mode: LayoutMode): void {
    this.layout = mode;
    this.compositor?.setLayout(mode);
    for (const b of this.backdrop.querySelectorAll<HTMLButtonElement>('[data-layout]')) {
      b.classList.toggle('active', b.dataset.layout === mode);
      b.setAttribute('aria-pressed', String(b.dataset.layout === mode));
    }
  }

  private destinations(): { dests: LivestreamDestination[]; error: string | null } {
    const dests: LivestreamDestination[] = [];
    for (const [i, row] of this.rows.entries()) {
      const err = validateDestination(row.platform, row.url, row.key);
      if (err) return { dests, error: `Destination ${i + 1}: ${err}` };
      dests.push({ name: PLATFORM_PRESETS[row.platform].label + (this.rows.filter(r => r.platform === row.platform).length > 1 ? ` ${i + 1}` : ''), url: row.url.trim(), key: row.key.trim() });
    }
    return { dests, error: null };
  }

  private async goLive(): Promise<void> {
    const { dests, error } = this.destinations();
    const formError = this.q('#livestream-form-error');
    formError.textContent = error ?? '';
    if (error) return;
    const session = this.getSession();
    if (!session.sessionId || !session.token) { formError.textContent = 'Join the session as host first.'; return; }
    this.ensurePreview();
    const media = this.compositor!.start();
    this.client = new LivestreamClient({ ...session, onState: s => { this.state = s; this.renderStatus(); } });
    try {
      await this.client.start({ preset: this.preset, destinations: dests }, media);
    } catch (e) {
      formError.textContent = e instanceof Error ? e.message : String(e);
    }
  }

  private renderRows(): void {
    const list = this.q('#livestream-dest-list');
    const locked = this.isActive();
    list.innerHTML = this.rows.map((row, i) => `
      <fieldset class="livestream-dest" data-index="${i}" ${locked ? 'disabled' : ''}>
        <legend class="section-label">Destination ${i + 1}</legend>
        <div class="livestream-dest-grid">
          <label class="livestream-field"><span>Platform</span><select data-field="platform">${(Object.keys(PLATFORM_PRESETS) as Platform[]).map(p => `<option value="${p}" ${p === row.platform ? 'selected' : ''}>${PLATFORM_PRESETS[p].label}</option>`).join('')}</select></label>
          <label class="livestream-field livestream-field-wide"><span>Server URL</span><input data-field="url" type="url" spellcheck="false" autocomplete="off" placeholder="${PLATFORM_PRESETS[row.platform].requireTls ? 'rtmps://server/app' : 'rtmps://server/app or rtmp://…'}" value="${escapeHtml(row.url)}"></label>
          <label class="livestream-field livestream-field-wide"><span>Stream key</span><input data-field="key" type="password" spellcheck="false" autocomplete="off" placeholder="${PLATFORM_PRESETS[row.platform].keyRequired ? 'Paste from the platform dashboard' : 'Optional for custom servers'}" value="${escapeHtml(row.key)}"></label>
        </div>
        <p class="field-hint" data-field="hint">${escapeHtml(PLATFORM_PRESETS[row.platform].hint)}${row.url.trim().startsWith('rtmp://') ? ' Plain rtmp:// is unencrypted; prefer rtmps:// when the platform offers it.' : ''}</p>
        ${this.rows.length > 1 ? `<button type="button" class="btn btn-quiet livestream-remove" data-remove="${i}">Remove</button>` : ''}
      </fieldset>`).join('');
    for (const fs of list.querySelectorAll<HTMLElement>('.livestream-dest')) {
      const i = Number(fs.dataset.index);
      const platform = fs.querySelector<HTMLSelectElement>('[data-field=platform]')!;
      platform.addEventListener('change', () => { const p = platform.value as Platform; this.rows[i] = { ...this.rows[i], platform: p, url: PLATFORM_PRESETS[p].url }; this.renderRows(); });
      fs.querySelector<HTMLInputElement>('[data-field=url]')!.addEventListener('input', e => { this.rows[i].url = (e.target as HTMLInputElement).value; });
      fs.querySelector<HTMLInputElement>('[data-field=key]')!.addEventListener('input', e => { this.rows[i].key = (e.target as HTMLInputElement).value; });
      fs.querySelector<HTMLButtonElement>('[data-remove]')?.addEventListener('click', () => { this.rows.splice(i, 1); this.renderRows(); });
    }
    (this.q('#livestream-add-dest') as HTMLButtonElement).disabled = locked || this.rows.length >= 4;
  }

  private renderStatus(): void {
    const s = this.state;
    const active = this.isActive();
    (this.q('#livestream-go') as HTMLButtonElement).style.display = active ? 'none' : 'inline-flex';
    (this.q('#livestream-end') as HTMLButtonElement).style.display = active ? 'inline-flex' : 'none';
    (this.q('#livestream-end') as HTMLButtonElement).disabled = s?.phase === 'stopping';
    (this.q('#livestream-preset') as HTMLSelectElement).disabled = active;
    this.renderRows();
    if (!active && !this.isOpen && this.compositor) this.stopPreview();
    const live = s?.phase === 'live';
    this.button.classList.toggle('is-live', live);
    this.button.textContent = live ? 'Live' : active ? 'Going live…' : 'Go live';
    const status = this.q('#livestream-status');
    status.dataset.phase = s?.phase ?? 'idle';
    status.dataset.mime = s?.mimeType ?? '';
    if (!s) { status.innerHTML = '<p class="field-hint">Not streaming. The preview above is exactly what viewers will see.</p>'; return; }
    const p = s.server?.progress;
    const bitrate = bitrateLabel(s.server);
    const stats = p && (s.phase === 'live' || s.phase === 'reconnecting')
      ? `<dl class="livestream-stats"><div><dt>Bitrate${bitrate.source ? ` <span class="livestream-stat-src">(${bitrate.source})</span>` : ''}</dt><dd data-bitrate>${escapeHtml(bitrate.value)}</dd></div><div><dt>Encoder</dt><dd>${p.fps.toFixed(0)} fps</dd></div><div><dt>Speed</dt><dd>${p.speed.toFixed(2)}×</dd></div><div><dt>Dropped</dt><dd>${p.dropFrames}</dd></div><div><dt>Duration</dt><dd>${formatDuration(p.outTimeSec)}</dd></div></dl>`
      : '';
    const dests = (s.server?.destinations ?? []).map((d, i) => `<li class="livestream-dest-state" data-index="${i}" data-state="${escapeHtml(d.state)}"><span class="status-dot"></span><strong>${escapeHtml(d.name)}</strong><span>${escapeHtml(d.server)}</span><span data-dest-line>${escapeHtml(destinationLine(d))}</span></li>`).join('');
    const congestion = s.congestion !== 'ok' ? `<p class="livestream-warning" role="alert">Network can't keep up: ${Math.round(s.bufferedBytes / 1024)} KB waiting to send. Lower the quality or close other uploads.</p>` : '';
    const warnings = (s.phase === 'live' || s.phase === 'reconnecting' || s.phase === 'starting') ? (s.server?.warnings ?? []) : [];
    const health = warnings.length ? `<ul id="livestream-warnings" class="livestream-warnings">${warnings.map(w => `<li class="livestream-warning">${escapeHtml(w)}</li>`).join('')}</ul>` : '';
    status.innerHTML = `<p class="livestream-phase"><span class="status-dot"></span>${escapeHtml(phaseLabel(s.phase))}</p><p class="field-hint">${escapeHtml(s.message)}</p>${congestion}${health}${stats}${dests ? `<ul class="livestream-dest-states">${dests}</ul>` : ''}`;
  }

  private skeleton(): string {
    return `
      <div class="drawer-panel livestream-drawer" role="dialog" aria-modal="true" aria-labelledby="livestream-heading">
        <div class="drawer-header"><div class="drawer-title-group"><h3 id="livestream-heading">Go live</h3></div><button id="livestream-close-btn" class="close-icon-btn" aria-label="Close livestream">×</button></div>
        <div class="drawer-content">
          <div class="drawer-section"><span class="section-label">Program preview</span><div id="livestream-preview" class="livestream-preview" aria-label="Livestream program preview"></div>
            <div class="livestream-layouts" role="group" aria-label="Livestream layout"><button type="button" class="tactile-pill active" aria-pressed="true" data-layout="grid">Grid</button><button type="button" class="tactile-pill" aria-pressed="false" data-layout="speaker">Speaker</button><button type="button" class="tactile-pill" aria-pressed="false" data-layout="side-by-side">Side by side</button></div>
            <label class="livestream-brand-row"><span>Branded layout <small>show name, logo and accent background</small></span><button type="button" role="switch" class="switch" id="livestream-branded" aria-checked="false" aria-label="Branded layout"><span></span></button></label>
            <div class="livestream-lt"><span class="section-label">Lower thirds</span><div id="livestream-lt-cues" class="livestream-lt-cues" role="group" aria-label="Show a lower third"></div><p id="livestream-lt-hint" class="field-hint"></p></div></div>
          <div id="livestream-status" class="livestream-status" role="status" aria-live="polite"></div>
          <div class="drawer-section"><label class="section-label" for="livestream-preset">Quality</label><select id="livestream-preset"><option value="720p30">720p30 · 3 Mbps</option><option value="1080p30">1080p30 · 4.5 Mbps</option></select><p class="field-hint">Encoded on this Mac with VideoToolbox. Make sure your upload speed is at least 1.5× the total of all destinations.</p></div>
          <div class="drawer-section"><div id="livestream-dest-list"></div><button type="button" id="livestream-add-dest" class="btn btn-secondary">Add destination</button><p class="field-hint">Stream keys stay in p2p.video's memory on this Mac for this stream only. They are never saved, logged or passed to another program, so they don't appear in the process list. Each destination reconnects on its own if its connection drops.</p></div>
          <p id="livestream-form-error" class="livestream-warning" role="alert"></p>
        </div>
        <div class="drawer-footer"><button type="button" id="livestream-go" class="btn btn-primary btn-block">Go live</button><button type="button" id="livestream-end" class="btn btn-secondary btn-block livestream-end" style="display:none">End livestream</button></div>
      </div>`;
  }
}

function phaseLabel(phase: LivestreamClientState['phase']): string {
  return { idle: 'Not streaming', starting: 'Starting', live: 'Live', reconnecting: 'Reconnecting', stopping: 'Ending', stopped: 'Stream ended', error: 'Stream stopped with an error' }[phase];
}

function formatDuration(sec: number): string {
  const s = Math.max(0, Math.floor(sec));
  return [Math.floor(s / 3600), Math.floor(s / 60) % 60, s % 60].map(n => String(n).padStart(2, '0')).join(':');
}

/**
 * Mounts the livestream toolbar button (hidden until setHost(true)) and drawer.
 * By default sources come from the #video-grid tiles and the session from the
 * page URL (?session=&token=), so main.ts needs no other wiring.
 */
export function mountLivestream(
  anchor: HTMLElement,
  getParticipants: () => CompositorSource[] = () => participantsFromGrid(document.getElementById('video-grid')),
  getSession: () => LivestreamSession = sessionFromUrl,
): LivestreamHandle {
  return new LivestreamPanel(anchor, getParticipants, getSession);
}
