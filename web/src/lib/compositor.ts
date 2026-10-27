// Livestream compositor: draws participant videos into a canvas (WYSIWYG
// layouts with name labels) and mixes every participant's audio with Web
// Audio. The resulting MediaStream feeds MediaRecorder for the livestream.
// This module has no imports so its pure layout math is unit-testable in node.

export type LayoutMode = 'grid' | 'speaker' | 'side-by-side';

export interface Rect { x: number; y: number; w: number; h: number; }

export interface CompositorSource {
  participantId: string;
  name: string;
  videoEl: HTMLVideoElement | null;
  audioTrack: MediaStreamTrack | null;
}

export interface LayoutOptions { gap?: number; }

const ASPECT = 16 / 9;

/**
 * Tile rectangles for `count` sources on a width×height canvas. Pure.
 * - grid: near-square grid of equal cells; a short last row is centered.
 * - speaker: index 0 fills the stage, the rest form a centered filmstrip below.
 * - side-by-side: one row of 16:9 tiles, vertically centered.
 */
export function computeLayout(count: number, width: number, height: number, mode: LayoutMode, opts: LayoutOptions = {}): Rect[] {
  const n = Math.max(0, Math.floor(count));
  if (n === 0 || width <= 0 || height <= 0) return [];
  const gap = Math.max(0, opts.gap ?? Math.round(Math.min(width, height) / 90));
  const r = (x: number, y: number, w: number, h: number): Rect => ({ x: Math.round(x), y: Math.round(y), w: Math.max(1, Math.round(w)), h: Math.max(1, Math.round(h)) });
  if (n === 1) return [r(0, 0, width, height)];

  if (mode === 'speaker') {
    const stripH = (height - 3 * gap) * 0.2;
    const mainH = height - 3 * gap - stripH;
    const rects = [r(gap, gap, width - 2 * gap, mainH)];
    const thumbs = n - 1;
    const thumbW = Math.min((width - gap * (thumbs + 1)) / thumbs, stripH * ASPECT);
    const rowW = thumbs * thumbW + (thumbs - 1) * gap;
    const x0 = (width - rowW) / 2;
    const y = gap * 2 + mainH;
    for (let i = 0; i < thumbs; i++) rects.push(r(x0 + i * (thumbW + gap), y, thumbW, stripH));
    return rects;
  }

  if (mode === 'side-by-side') {
    let tileW = (width - gap * (n + 1)) / n;
    let tileH = tileW / ASPECT;
    if (tileH > height - 2 * gap) { tileH = height - 2 * gap; tileW = tileH * ASPECT; }
    const rowW = n * tileW + (n - 1) * gap;
    const x0 = (width - rowW) / 2;
    const y = (height - tileH) / 2;
    return Array.from({ length: n }, (_, i) => r(x0 + i * (tileW + gap), y, tileW, tileH));
  }

  const cols = Math.ceil(Math.sqrt(n));
  const rows = Math.ceil(n / cols);
  const cellW = (width - gap * (cols + 1)) / cols;
  const cellH = (height - gap * (rows + 1)) / rows;
  const rects: Rect[] = [];
  for (let i = 0; i < n; i++) {
    const row = Math.floor(i / cols);
    const inRow = row === rows - 1 ? n - row * cols : cols;
    const col = i - row * cols;
    const rowW = inRow * cellW + (inRow - 1) * gap;
    const x0 = (width - rowW) / 2;
    rects.push(r(x0 + col * (cellW + gap), gap + row * (cellH + gap), cellW, cellH));
  }
  return rects;
}

/** Source crop for drawing a srcW×srcH frame into dst with object-fit: cover. Pure. */
export function coverCrop(srcW: number, srcH: number, dst: Rect): Rect {
  if (srcW <= 0 || srcH <= 0) return { x: 0, y: 0, w: 0, h: 0 };
  const scale = Math.max(dst.w / srcW, dst.h / srcH);
  const w = dst.w / scale, h = dst.h / scale;
  return { x: (srcW - w) / 2, y: (srcH - h) / 2, w, h };
}

/**
 * Active-speaker choice with hysteresis: switch only to a source louder than
 * `threshold` after the current speaker has held the stage for `holdMs`. Pure.
 */
export function pickSpeaker(levels: Map<string, number>, current: string | null, heldSinceMs: number, nowMs: number, threshold = 0.02, holdMs = 1500): string | null {
  let loudest: string | null = null, max = threshold;
  for (const [id, level] of levels) if (level > max) { max = level; loudest = id; }
  if (current && !levels.has(current)) return loudest ?? levels.keys().next().value ?? null;
  if (!loudest || loudest === current) return current ?? levels.keys().next().value ?? null;
  if (current && nowMs - heldSinceMs < holdMs) return current;
  return loudest;
}

/** Orders sources for drawing: in speaker mode the speaker moves to index 0. Pure. */
export function orderForLayout<T extends { participantId: string }>(sources: T[], mode: LayoutMode, speakerId: string | null): T[] {
  if (mode !== 'speaker' || !speakerId) return sources.slice();
  const i = sources.findIndex(s => s.participantId === speakerId);
  if (i <= 0) return sources.slice();
  return [sources[i], ...sources.slice(0, i), ...sources.slice(i + 1)];
}

function initials(name: string): string {
  const parts = name.trim().split(/\s+/).filter(Boolean);
  return (parts.length ? parts.slice(0, 2).map(p => p[0]).join('') : '?').toUpperCase();
}

interface AudioNodeEntry { source: MediaStreamAudioSourceNode; analyser: AnalyserNode; trackId: string; }

/**
 * Optional program decoration (branding, lower thirds) [SL-P5-2], kept
 * behind an interface so this module stays import-free. The compositor asks
 * where the tiles go, lets the decorator paint the background, tells it the
 * active speaker, skips the plain name label of the tile whose lower third
 * is on air, and lets it paint on top of the tiles.
 */
export interface ProgramDecorator {
  /** Where tiles are laid out on a width×height canvas. */
  stage(width: number, height: number): Rect;
  background(ctx: CanvasRenderingContext2D, width: number, height: number): void;
  /** True when the decorator needs the active speaker (speaker-driven lower thirds). */
  wantsSpeaker(): boolean;
  /** The participant whose lower third is on air this frame, if any. */
  onAir(speakerId: string | null, nowMs: number): string | null;
  /** Paints over the tiles (lower third, band, logo). */
  overlay(ctx: CanvasRenderingContext2D, frame: { width: number; height: number; tiles: Array<{ source: CompositorSource; rect: Rect }>; onAir: string | null; nowMs: number }): void;
}

export interface CompositorOptions { width: number; height: number; fps: number; layout?: LayoutMode; decorator?: ProgramDecorator; }

/** Canvas + Web Audio compositor. Call start() from a user gesture (AudioContext). */
export class Compositor {
  readonly canvas: HTMLCanvasElement;
  private ctx2d: CanvasRenderingContext2D;
  private layout: LayoutMode;
  private audio: AudioContext | null = null;
  private mixDest: MediaStreamAudioDestinationNode | null = null;
  private nodes = new Map<string, AudioNodeEntry>();
  private speaker: string | null = null;
  private speakerSince = 0;
  private ticker: Worker | null = null;
  private interval: number | null = null;
  private stream: MediaStream | null = null;
  private levelBuf = new Float32Array(1024);

  constructor(private getSources: () => CompositorSource[], private opts: CompositorOptions) {
    this.layout = opts.layout ?? 'grid';
    this.canvas = document.createElement('canvas');
    this.canvas.width = opts.width;
    this.canvas.height = opts.height;
    const ctx = this.canvas.getContext('2d', { alpha: false });
    if (!ctx) throw new Error('Canvas 2D is unavailable');
    this.ctx2d = ctx;
  }

  setLayout(mode: LayoutMode): void { this.layout = mode; this.draw(); }
  getLayout(): LayoutMode { return this.layout; }

  /** Starts drawing and returns canvas video + mixed audio (always one audio track). */
  start(): MediaStream {
    if (this.stream) return this.stream;
    this.audio = new AudioContext({ sampleRate: 48000 });
    void this.audio.resume();
    this.mixDest = this.audio.createMediaStreamDestination();
    this.draw();
    const video = this.canvas.captureStream(this.opts.fps).getVideoTracks()[0];
    this.stream = new MediaStream([video, ...this.mixDest.stream.getAudioTracks()]);
    this.startTicker();
    return this.stream;
  }

  stop(): void {
    this.ticker?.terminate(); this.ticker = null;
    if (this.interval !== null) { clearInterval(this.interval); this.interval = null; }
    for (const entry of this.nodes.values()) { entry.source.disconnect(); entry.analyser.disconnect(); }
    this.nodes.clear();
    this.stream?.getVideoTracks().forEach(t => t.stop());
    this.stream = null;
    void this.audio?.close();
    this.audio = null; this.mixDest = null;
  }

  // Background tabs throttle rAF and main-thread timers; a worker timer keeps
  // frames flowing while the host looks at another tab.
  private startTicker(): void {
    const period = Math.max(10, Math.round(1000 / this.opts.fps));
    try {
      const url = URL.createObjectURL(new Blob([`setInterval(()=>postMessage(0),${period});`], { type: 'text/javascript' }));
      this.ticker = new Worker(url);
      URL.revokeObjectURL(url);
      this.ticker.onmessage = () => this.tick();
    } catch {
      this.interval = window.setInterval(() => this.tick(), period);
    }
  }

  private tick(): void {
    this.syncAudio();
    this.draw();
  }

  private syncAudio(): void {
    if (!this.audio || !this.mixDest) return;
    const wanted = new Map<string, MediaStreamTrack>();
    for (const s of this.getSources()) if (s.audioTrack && s.audioTrack.readyState === 'live') wanted.set(s.participantId, s.audioTrack);
    for (const [id, entry] of this.nodes) {
      if (wanted.get(id)?.id !== entry.trackId) { entry.source.disconnect(); entry.analyser.disconnect(); this.nodes.delete(id); }
    }
    for (const [id, track] of wanted) {
      if (this.nodes.has(id)) continue;
      const source = this.audio.createMediaStreamSource(new MediaStream([track]));
      const analyser = this.audio.createAnalyser();
      analyser.fftSize = 1024;
      source.connect(this.mixDest);
      source.connect(analyser);
      this.nodes.set(id, { source, analyser, trackId: track.id });
    }
  }

  private levels(): Map<string, number> {
    const out = new Map<string, number>();
    for (const [id, entry] of this.nodes) {
      entry.analyser.getFloatTimeDomainData(this.levelBuf);
      let sum = 0;
      for (const v of this.levelBuf) sum += v * v;
      out.set(id, Math.sqrt(sum / this.levelBuf.length));
    }
    return out;
  }

  /** The active speaker the compositor currently follows (speaker layout or decorator). */
  activeSpeaker(): string | null { return this.speaker; }

  private draw(): void {
    const { width, height } = this.canvas;
    const ctx = this.ctx2d;
    const deco = this.opts.decorator;
    if (deco) deco.background(ctx, width, height);
    else { ctx.fillStyle = '#0c1014'; ctx.fillRect(0, 0, width, height); }
    let sources = this.getSources();
    const now = performance.now();
    if (this.layout === 'speaker' || deco?.wantsSpeaker()) {
      const next = pickSpeaker(this.levels(), this.speaker, this.speakerSince, now);
      if (next !== this.speaker) { this.speaker = next; this.speakerSince = now; }
    }
    if (this.layout === 'speaker') sources = orderForLayout(sources, 'speaker', this.speaker);
    const stage = deco ? deco.stage(width, height) : { x: 0, y: 0, w: width, h: height };
    const rects = computeLayout(sources.length, stage.w, stage.h, this.layout).map(r => ({ ...r, x: r.x + stage.x, y: r.y + stage.y }));
    const onAir = deco ? deco.onAir(this.speaker, now) : null;
    sources.forEach((s, i) => this.drawTile(s, rects[i], s.participantId !== onAir));
    deco?.overlay(ctx, { width, height, tiles: sources.map((source, i) => ({ source, rect: rects[i] })), onAir, nowMs: now });
  }

  private drawTile(s: CompositorSource, rect: Rect, label = true): void {
    const ctx = this.ctx2d;
    ctx.save();
    ctx.beginPath();
    ctx.rect(rect.x, rect.y, rect.w, rect.h);
    ctx.clip();
    const v = s.videoEl;
    if (v && v.readyState >= 2 && v.videoWidth > 0) {
      const crop = coverCrop(v.videoWidth, v.videoHeight, rect);
      ctx.drawImage(v, crop.x, crop.y, crop.w, crop.h, rect.x, rect.y, rect.w, rect.h);
    } else {
      ctx.fillStyle = '#141a20';
      ctx.fillRect(rect.x, rect.y, rect.w, rect.h);
      ctx.fillStyle = '#98a6b2';
      ctx.font = `500 ${Math.round(Math.min(rect.w, rect.h) / 4)}px "Helvetica Neue", Helvetica, Arial, sans-serif`;
      ctx.textAlign = 'center'; ctx.textBaseline = 'middle';
      ctx.fillText(initials(s.name), rect.x + rect.w / 2, rect.y + rect.h / 2);
    }
    if (s.name && label) {
      const size = Math.max(12, Math.min(28, Math.round(rect.h / 16)));
      ctx.font = `500 ${size}px "Helvetica Neue", Helvetica, Arial, sans-serif`;
      ctx.textAlign = 'left'; ctx.textBaseline = 'middle';
      const pad = Math.round(size * 0.5);
      const label = s.name.length > 40 ? s.name.slice(0, 39) + '…' : s.name;
      const w = Math.min(rect.w - 2 * pad, ctx.measureText(label).width + 2 * pad);
      const h = size + pad * 1.5;
      const x = rect.x + pad, y = rect.y + rect.h - h - pad;
      ctx.fillStyle = 'rgba(12,16,20,0.72)';
      ctx.fillRect(x, y, w, h);
      ctx.fillStyle = '#e6edf0';
      ctx.fillText(label, x + pad, y + h / 2, w - 2 * pad);
    }
    ctx.restore();
  }
}
