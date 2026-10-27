// Call views (ROADMAP P1) for the #video-grid tiles.
//
// Modes:
//   split    "Grid"     equal tiles
//   speaker  "Speaker"  the active speaker (or pinned tile) large, others in a filmstrip
//   hero     "Focus"    manual: click a tile to bring it to the large slot
//   multicam "Switcher" preview/program tally selection
// Presentation: while anyone shares a screen (a .tile-screen exists), Grid and
// Speaker switch to the presentation layout: the screen on the stage, the
// presenter's camera first in the filmstrip. A pinned tile (local only) takes
// the stage in Speaker/presentation, and pinning from Grid switches to
// Speaker. Self view can be hidden (only when someone else is visible).
//
// The stage layout orders tiles with CSS `order` instead of moving them, so
// playing <video> elements are never detached.

import { ActiveSpeakerDetector, remoteAudioLevels } from '../lib/activespeaker';

export type LayoutMode = 'split' | 'speaker' | 'hero' | 'multicam';

export interface LayoutSwitcherOptions {
  /** Remote audio levels for the active speaker come from this PeerConnection's stats. */
  getPeerConnection?: () => RTCPeerConnection | null;
  /** Poll interval for audio levels. */
  levelIntervalMs?: number;
}

const MODE_LABELS: Array<[LayoutMode, string, string]> = [
  ['split', 'Grid', 'Everyone the same size'],
  ['speaker', 'Speaker', 'Active speaker large, others in a strip'],
  ['hero', 'Focus', 'Click a tile to make it large'],
  ['multicam', 'Switcher', 'Preview layout selection'],
];
const SPEAKING_LEVEL = 0.02;

export class LayoutSwitcher {
  private container: HTMLElement;
  private currentMode: LayoutMode = 'split';
  private videoGrid: HTMLElement;
  private activeProgramAngle = 'local';
  private onModeChange?: (mode: LayoutMode) => void;
  private pinnedId: string | null = null;
  private hideSelf = false;
  private activeSpeaker: string | null = null;
  private speaking = new Set<string>();
  private readonly detector = new ActiveSpeakerDetector();
  private polling = false;
  private selfBtn!: HTMLButtonElement;

  constructor(videoGrid: HTMLElement, onModeChange?: (mode: LayoutMode) => void, private readonly options: LayoutSwitcherOptions = {}) {
    this.videoGrid = videoGrid;
    this.onModeChange = onModeChange;
    this.container = document.createElement('div');
    this.container.className = 'layout-switcher-bar';
    this.render();
    this.applyLayout();
    // Tiles come and go with renegotiation and screen shares.
    if (typeof MutationObserver !== 'undefined') new MutationObserver(() => this.applyLayout()).observe(videoGrid, { childList: true });
    if (options.getPeerConnection) setInterval(() => void this.pollLevels(), options.levelIntervalMs ?? 300);
  }

  public getElement(): HTMLElement {
    return this.container;
  }

  public getMode(): LayoutMode { return this.currentMode; }

  public setMode(mode: LayoutMode): void {
    this.currentMode = mode;
    this.updateButtons();
    this.applyLayout();
    this.onModeChange?.(mode);
  }

  /** Pin a tile (by element id) to the stage, or unpin with null. Local only. */
  public pin(tileId: string | null): void {
    this.pinnedId = tileId;
    if (tileId && (this.currentMode === 'split' || this.currentMode === 'hero')) this.currentMode = 'speaker';
    this.updateButtons();
    this.applyLayout();
  }

  public getPinned(): string | null { return this.pinnedId; }

  public setHideSelf(hide: boolean): void {
    this.hideSelf = hide;
    this.updateButtons();
    this.applyLayout();
  }

  /** Overrides detection (tests, or an external detector). */
  public setActiveSpeaker(participantId: string | null): void {
    if (participantId === this.activeSpeaker) return;
    this.activeSpeaker = participantId;
    this.applyLayout();
  }

  private render(): void {
    this.container.innerHTML = `
      <div class="tactile-pill-group layout-toggles" role="group" aria-label="Call layout">
        ${MODE_LABELS.map(([mode, label, title]) => `<button type="button" class="tactile-btn ${this.currentMode === mode ? 'active' : ''}" data-layout="${mode}" title="${title}" aria-pressed="${this.currentMode === mode}">${label}</button>`).join('')}
      </div>
      <button type="button" class="tactile-btn self-view-toggle" data-self-view aria-pressed="false" title="Hide or show your own tile">Hide self</button>
    `;
    this.container.querySelectorAll<HTMLButtonElement>('[data-layout]').forEach((btn) => {
      btn.addEventListener('click', () => {
        const layout = btn.getAttribute('data-layout') as LayoutMode;
        if (layout) this.setMode(layout);
      });
    });
    this.selfBtn = this.container.querySelector<HTMLButtonElement>('[data-self-view]')!;
    this.selfBtn.addEventListener('click', () => this.setHideSelf(!this.hideSelf));
  }

  private updateButtons(): void {
    this.container.querySelectorAll<HTMLButtonElement>('[data-layout]').forEach((btn) => {
      const on = btn.getAttribute('data-layout') === this.currentMode;
      btn.classList.toggle('active', on);
      btn.setAttribute('aria-pressed', String(on));
    });
    this.selfBtn.textContent = this.hideSelf ? 'Show self' : 'Hide self';
    this.selfBtn.setAttribute('aria-pressed', String(this.hideSelf));
    this.selfBtn.classList.toggle('active', this.hideSelf);
  }

  private async pollLevels(): Promise<void> {
    const pc = this.options.getPeerConnection?.();
    if (this.polling || !pc || typeof pc.getStats !== 'function') return;
    this.polling = true;
    try {
      const owners = new Map<string, string>();
      for (const tile of this.videoGrid.querySelectorAll<HTMLElement>('.tile[data-participant-id]')) {
        const stream = tile.querySelector('video')?.srcObject;
        if (!(stream instanceof MediaStream)) continue;
        for (const track of stream.getAudioTracks()) owners.set(track.id, tile.dataset.participantId!);
      }
      const levels = await remoteAudioLevels(pc, owners);
      // The live sound ring [SL-UX-2]: each tile's ring follows its voice.
      for (const tile of this.videoGrid.querySelectorAll<HTMLElement>('.tile[data-participant-id]')) {
        const level = levels.get(tile.dataset.participantId!) ?? 0;
        tile.style.setProperty('--level', (level > SPEAKING_LEVEL ? Math.min(1, Math.sqrt(level) * 1.8) : 0).toFixed(2));
      }
      const speaking = new Set([...levels].filter(([, level]) => level > SPEAKING_LEVEL).map(([id]) => id));
      const speaker = this.detector.update(levels, performance.now());
      const ringChanged = speaking.size !== this.speaking.size || [...speaking].some(id => !this.speaking.has(id));
      this.speaking = speaking;
      if (speaker !== this.activeSpeaker || ringChanged) {
        this.activeSpeaker = speaker;
        this.applyLayout();
      }
    } catch {
      // Stats are best effort; the layout keeps its last speaker.
    } finally {
      this.polling = false;
    }
  }

  private tiles(): HTMLElement[] {
    return Array.from(this.videoGrid.querySelectorAll<HTMLElement>(':scope > .tile'));
  }

  private ensurePinButton(tile: HTMLElement): void {
    if (tile.querySelector(':scope > .tile-pin')) return;
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'tile-pin';
    btn.addEventListener('click', (event) => {
      event.stopPropagation();
      this.pin(this.pinnedId === tile.id ? null : tile.id);
    });
    tile.append(btn);
  }

  private label(tile: HTMLElement): string {
    return tile.querySelector('.tile-name')?.textContent?.trim() || 'this tile';
  }

  public applyLayout(): void {
    const tiles = this.tiles();
    if (this.pinnedId && !tiles.some(t => t.id === this.pinnedId)) this.pinnedId = null;
    const screen = tiles.find(t => t.classList.contains('tile-screen')) ?? null;
    const presenting = !!screen && (this.currentMode === 'split' || this.currentMode === 'speaker');
    const stageMode = presenting || this.currentMode === 'speaker';
    const local = tiles.find(t => t.id === 'local-tile') ?? null;
    const others = tiles.filter(t => t !== local && !t.classList.contains('tile-screen-local'));
    const selfHidden = this.hideSelf && !!local && others.length > 0;

    this.videoGrid.classList.remove('layout-split', 'layout-speaker', 'layout-hero', 'layout-multicam', 'layout-stage', 'layout-presentation', 'layout-stage-solo');
    this.videoGrid.classList.add(stageMode ? 'layout-stage' : `layout-${this.currentMode}`);
    if (presenting) this.videoGrid.classList.add('layout-presentation');
    if (this.activeSpeaker) this.videoGrid.dataset.activeSpeaker = this.activeSpeaker;
    else delete this.videoGrid.dataset.activeSpeaker;

    for (const t of tiles) {
      this.ensurePinButton(t);
      const pinned = t.id === this.pinnedId;
      t.classList.toggle('tile-pinned', pinned);
      const pinBtn = t.querySelector<HTMLButtonElement>(':scope > .tile-pin')!;
      pinBtn.setAttribute('aria-pressed', String(pinned));
      pinBtn.setAttribute('aria-label', `${pinned ? 'Unpin' : 'Pin'} ${this.label(t)}`);
      pinBtn.title = pinned ? 'Unpin' : 'Pin to the large view (only you see this)';
      pinBtn.textContent = pinned ? 'Unpin' : 'Pin';
      t.classList.toggle('tile-self-hidden', t === local && selfHidden);
      t.classList.toggle('tile-speaking', !!t.dataset.participantId && this.speaking.has(t.dataset.participantId));
      t.style.removeProperty('--tile-order');
      t.classList.remove('tile-stage');
      if (this.currentMode !== 'hero') t.classList.remove('tile-hero');
      if (this.currentMode !== 'hero' && !stageMode) t.classList.remove('tile-filmstrip');
      if (this.currentMode !== 'multicam') t.classList.remove('tally-program', 'tally-preview');
      if (this.currentMode !== 'hero' && this.currentMode !== 'multicam') t.onclick = null;
    }

    if (stageMode) {
      this.applyStage(tiles, screen, presenting, local, selfHidden);
    } else if (this.currentMode === 'hero') {
      tiles.forEach((t, index) => {
        t.classList.toggle('tile-hero', index === 0);
        t.classList.toggle('tile-filmstrip', index !== 0);
        t.onclick = () => {
          // Bring clicked tile to hero position
          this.videoGrid.prepend(t);
          this.applyLayout();
        };
      });
    } else if (this.currentMode === 'multicam') {
      tiles.forEach((t) => {
        const id = t.id || 'remote';
        const program = id === this.activeProgramAngle || (this.activeProgramAngle === 'local' && t.id === 'local-tile');
        t.classList.toggle('tally-program', program);
        t.classList.toggle('tally-preview', !program);
        t.onclick = () => {
          this.activeProgramAngle = t.id || 'remote';
          this.applyLayout();
        };
      });
    }
  }

  private applyStage(tiles: HTMLElement[], screen: HTMLElement | null, presenting: boolean, local: HTMLElement | null, selfHidden: boolean): void {
    const pinned = this.pinnedId ? tiles.find(t => t.id === this.pinnedId) ?? null : null;
    const speakerTile = this.activeSpeaker
      ? tiles.find(t => t.dataset.participantId === this.activeSpeaker) ?? null
      : null;
    const firstRemote = tiles.find(t => t.dataset.participantId) ?? null;
    const stage = pinned ?? (presenting ? screen : null) ?? speakerTile ?? firstRemote ?? local ?? tiles[0] ?? null;
    // The presenter's camera sits first in the filmstrip.
    const presenterId = screen?.dataset.screenOf ?? '';
    const presenterCam = presenting
      ? (screen?.classList.contains('tile-screen-local') ? local : tiles.find(t => t.dataset.participantId === presenterId) ?? null)
      : null;
    let order = 1;
    let film = 0;
    for (const t of tiles) {
      if (t === stage) {
        t.classList.add('tile-stage');
        t.classList.remove('tile-filmstrip');
        t.style.setProperty('--tile-order', '-2');
        continue;
      }
      t.classList.add('tile-filmstrip');
      t.style.setProperty('--tile-order', t === presenterCam ? '-1' : String(order++));
      if (!(t === local && selfHidden)) film++;
    }
    this.videoGrid.style.setProperty('--film-count', String(Math.max(1, film)));
    this.videoGrid.classList.toggle('layout-stage-solo', film === 0);
  }
}
