// Background blur / replace [SL-P1-7]: a picker in the green room and in the
// call's More menu. The effect is applied to what the call sends only; the
// local ISO recording keeps the untouched camera (ADR-006). The choice is
// remembered on this device; the segmentation runtime loads on first use.
import { BackgroundProcessor, EFFECTS, effectById, loadEffect, saveEffect, type EffectDef, type EffectId } from '../lib/segmentation';
import { ICONS } from './icons';
import '../effects.css';

export interface BackgroundEffectsHost {
  /** The raw camera track (the recording source). */
  camera(): MediaStreamTrack | null;
  /**
   * What the call should send changed (raw ↔ processed). The host swaps the
   * call sender with replaceTrack and refreshes self views.
   */
  outputChanged(previous: MediaStreamTrack | null, next: MediaStreamTrack | null): Promise<void>;
  toast(message: string): void;
}

function storage(): Storage | null {
  try { return window.localStorage; } catch { return null; }
}

function swatchStyle(e: EffectDef): string {
  if (e.gradient) {
    const stops = e.gradient.stops.join(', ');
    return e.gradient.type === 'radial' ? `background: radial-gradient(circle at 50% 42%, ${stops})` : `background: linear-gradient(135deg, ${stops})`;
  }
  return '';
}

export class BackgroundEffects {
  readonly menuButton: HTMLButtonElement;
  readonly panel: HTMLElement;
  readonly greenRoomRow: HTMLElement;
  private selected: EffectId;
  private processor: BackgroundProcessor | null = null;
  private loading: Promise<BackgroundProcessor | null> | null = null;
  private applied: MediaStreamTrack | null = null; // the processed track the call sends, if any
  private busy = false;
  private readonly preview: HTMLVideoElement;
  private readonly status: HTMLElement;
  private readonly greenStatus: HTMLElement;

  constructor(private readonly host: BackgroundEffectsHost) {
    this.selected = loadEffect(storage());

    this.menuButton = document.createElement('button');
    this.menuButton.type = 'button';
    this.menuButton.id = 'effects-btn';
    this.menuButton.className = 'btn collab-btn';
    this.menuButton.setAttribute('aria-expanded', 'false');
    this.menuButton.setAttribute('aria-controls', 'effects-panel');
    this.menuButton.innerHTML = `<span class="collab-icon" aria-hidden="true">${ICONS.effects}</span><span class="collab-label">Background effects</span>`;
    this.menuButton.addEventListener('click', () => this.toggle());

    this.panel = document.createElement('div');
    this.panel.id = 'effects-panel';
    this.panel.className = 'effects-popover';
    this.panel.setAttribute('role', 'dialog');
    this.panel.setAttribute('aria-labelledby', 'effects-heading');
    this.panel.hidden = true;
    this.panel.innerHTML = `<div class="effects-head"><h2 id="effects-heading">Background</h2><button type="button" class="icon-button" data-close aria-label="Close background effects">×</button></div>
      <div class="effects-preview"><video muted playsinline autoplay></video></div>
      ${this.swatches('call')}
      <p class="effects-note">Only changes what others see. Your recording keeps your original camera.</p>
      <p class="effects-status" role="status" aria-live="polite"></p>`;
    this.preview = this.panel.querySelector('video')!;
    this.status = this.panel.querySelector('.effects-status')!;
    this.panel.querySelector<HTMLButtonElement>('[data-close]')!.addEventListener('click', () => this.close(true));
    document.addEventListener('keydown', e => { if (e.key === 'Escape' && !this.panel.hidden) { e.stopPropagation(); this.close(true); } });
    document.addEventListener('pointerdown', e => {
      if (!this.panel.hidden && !this.panel.contains(e.target as Node) && !this.menuButton.contains(e.target as Node)) this.close(false);
    });

    this.greenRoomRow = document.createElement('div');
    this.greenRoomRow.className = 'effects-row';
    this.greenRoomRow.innerHTML = `<span class="field-label" id="effects-green-label">Background</span>${this.swatches('green')}
      <p class="effects-note">Only what others see in the call. Your recording keeps your original camera.</p><p class="effects-status" role="status" aria-live="polite"></p>`;
    this.greenStatus = this.greenRoomRow.querySelector('.effects-status')!;

    for (const root of [this.panel, this.greenRoomRow]) {
      for (const btn of root.querySelectorAll<HTMLButtonElement>('[data-effect]')) btn.addEventListener('click', () => void this.choose(btn.dataset.effect as EffectId));
      root.querySelector('[role=radiogroup]')!.addEventListener('keydown', e => this.arrowKeys(e as KeyboardEvent, root));
    }
    this.render();
    window.setInterval(() => this.renderStats(), 1000);
  }

  private swatches(where: 'call' | 'green'): string {
    return `<div class="effects-swatches" role="radiogroup" aria-label="Background effect">${EFFECTS.map(e => `
      <button type="button" role="radio" class="effects-swatch effects-swatch-${e.kind}" id="effect-${where}-${e.id}" data-effect="${e.id}" aria-checked="false" title="${e.label}">
        <span class="effects-swatch-art" style="${swatchStyle(e)}" aria-hidden="true">${e.kind === 'none' ? ICONS.close : ''}</span><span class="effects-swatch-label">${e.label}</span></button>`).join('')}</div>`;
  }

  /** The effect in use. */
  get effect(): EffectId { return this.selected; }
  get active(): boolean { return !!this.applied && this.applied.readyState === 'live'; }
  /** Processing stats for diagnostics and tests. */
  stats() { return this.processor?.stats() ?? null; }

  /** The video track the call should send for `raw` (processed when an effect is on). */
  sendTrack(raw: MediaStreamTrack | null | undefined): MediaStreamTrack | null {
    return this.active ? this.applied : raw ?? null;
  }

  /** `stream` with its video replaced by what the call sends. */
  callStream(stream: MediaStream): MediaStream {
    if (!this.active) return stream;
    return new MediaStream([this.applied!, ...stream.getAudioTracks()]);
  }

  /** The camera changed (preview started, device switch, rejoin): follow it. */
  async cameraChanged(): Promise<void> {
    if (this.selected === 'off') return;
    await this.apply(this.selected, false);
  }

  /** Leaving the call: stop processing (the choice is kept for rejoin). */
  async stop(): Promise<void> {
    const previous = this.applied;
    this.applied = null;
    this.processor?.stop();
    if (previous) await this.host.outputChanged(previous, this.host.camera());
    this.render();
  }

  private async choose(id: EffectId): Promise<void> {
    if (this.busy || id === this.selected && (id === 'off' || this.active)) return;
    await this.apply(id, true);
  }

  private async apply(id: EffectId, announce: boolean): Promise<void> {
    this.busy = true;
    const effect = effectById(id);
    const previous = this.applied;
    try {
      if (effect.kind === 'none') {
        this.applied = null;
        this.processor?.stop();
        this.selected = 'off';
        if (previous) await this.host.outputChanged(previous, this.host.camera());
      } else {
        const camera = this.host.camera();
        if (!camera || camera.readyState !== 'live') { this.selected = id; return; }
        this.setStatus('Loading background effects…');
        const processor = await this.load();
        if (!processor) {
          this.selected = 'off';
          this.applied = null;
          this.host.toast('Background effects aren’t available in this browser.');
          return;
        }
        processor.setEffect(effect);
        const next = processor.start(camera);
        this.applied = next;
        this.selected = id;
        // Also when the output is unchanged (new camera into the same track): self views refresh.
        await this.host.outputChanged(previous ?? camera, next);
        if (announce) this.host.toast(`${effect.label} on. Your recording keeps your original camera.`);
      }
      saveEffect(storage(), this.selected);
    } catch (err) {
      this.host.toast(`Couldn’t change the background: ${err instanceof Error ? err.message : String(err)}`);
    } finally {
      this.busy = false;
      this.setStatus('');
      this.render();
    }
  }

  private load(): Promise<BackgroundProcessor | null> {
    if (this.processor) return Promise.resolve(this.processor);
    // XNNPACK on the CPU: as fast as the GPU delegate on an M2 Max (≈7 ms a
    // frame at 256×144, measured) without a GPU→CPU mask readback stall, and
    // 4× faster where WebGL is software-rendered. Override for diagnostics:
    // localStorage studiolink.effectsDelegate = "GPU".
    let prefer: 'GPU' | 'CPU' = 'CPU';
    try { if (storage()?.getItem('studiolink.effectsDelegate') === 'GPU') prefer = 'GPU'; } catch { /* CPU */ }
    this.loading ??= BackgroundProcessor.load(undefined, prefer).then(p => {
      this.processor = p;
      p.onCollapse = fps => void this.collapsed(fps);
      return p;
    }).catch(err => {
      console.warn('Background effects unavailable', err);
      this.loading = null;
      return null;
    });
    return this.loading;
  }

  private async collapsed(fps: number): Promise<void> {
    if (this.selected === 'off') return;
    await this.apply('off', false);
    this.host.toast(`Background effect turned off: this device couldn’t keep up (${Math.round(fps)} fps). Your video is back to normal.`);
  }

  private toggle(): void {
    if (this.panel.hidden) this.open();
    else this.close(true);
  }

  private open(): void {
    this.panel.hidden = false;
    this.menuButton.setAttribute('aria-expanded', 'true');
    this.render();
    this.panel.querySelector<HTMLButtonElement>('[aria-checked="true"]')?.focus();
  }

  private close(restoreFocus: boolean): void {
    if (this.panel.hidden) return;
    this.panel.hidden = true;
    this.menuButton.setAttribute('aria-expanded', 'false');
    this.preview.srcObject = null;
    if (restoreFocus) document.getElementById('more-btn')?.focus();
  }

  private arrowKeys(e: KeyboardEvent, root: HTMLElement): void {
    const keys: Record<string, number> = { ArrowRight: 1, ArrowDown: 1, ArrowLeft: -1, ArrowUp: -1 };
    if (!(e.key in keys)) return;
    e.preventDefault();
    const buttons = [...root.querySelectorAll<HTMLButtonElement>('[data-effect]')];
    const i = buttons.indexOf(document.activeElement as HTMLButtonElement);
    const next = buttons[(Math.max(0, i) + keys[e.key] + buttons.length) % buttons.length];
    next.focus();
    void this.choose(next.dataset.effect as EffectId);
  }

  private setStatus(text: string): void {
    this.status.textContent = text;
    this.greenStatus.textContent = text;
  }

  private renderStats(): void {
    const s = this.stats();
    if (!this.active || !s || this.busy) return;
    if (!this.panel.hidden) this.status.textContent = `${effectById(this.selected).label} · ${Math.round(s.fps)} fps`;
    // Measurement hooks for tests and support: fps, per-frame cost, delegate.
    Object.assign(this.greenRoomRow.dataset, { fps: String(s.fps), processMs: String(s.processMs), segmentMs: String(s.segmentMs), delegate: s.delegate, pipeline: s.pipeline });
  }

  private render(): void {
    for (const btn of document.querySelectorAll<HTMLButtonElement>('.effects-swatches [data-effect]')) {
      const on = btn.dataset.effect === this.selected;
      btn.setAttribute('aria-checked', String(on));
      btn.tabIndex = on ? 0 : -1;
      btn.disabled = this.busy;
    }
    this.menuButton.classList.toggle('is-on', this.selected !== 'off');
    if (!this.panel.hidden) {
      const track = this.sendTrack(this.host.camera());
      const want = track ? new MediaStream([track]) : null;
      if ((this.preview.srcObject as MediaStream | null)?.getVideoTracks()[0] !== track) this.preview.srcObject = want;
    }
  }
}
