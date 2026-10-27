// Background blur / replace for the call sender [SL-P1-7].
//
//   camera track (full res) ──► recorder (untouched, ADR-006)
//        └──► BackgroundProcessor ──► processed track (≤720p) ──► call sender
//
// Segmentation runs on this device with MediaPipe's selfie segmenter. The
// WASM runtime and the model are files on our own origin (/mediapipe/), never
// a CDN; compiling the WASM needs CSP script-src 'wasm-unsafe-eval'.
//
// Pure helpers (effect list, compositor parameters, mask → alpha, fps
// monitor, saved choice) are unit-tested in web/tests/effects.test.mjs.

export type EffectId = 'off' | 'blur-light' | 'blur-strong' | 'bg-graphite' | 'bg-dusk' | 'bg-ocean';

export interface EffectDef {
  id: EffectId;
  label: string;
  kind: 'none' | 'blur' | 'image';
  /** Blur radius at 720p output height (scaled with the output). */
  blurPx?: number;
  /** Generated background: CSS-like colour stops, top-left to bottom-right (no images). */
  gradient?: { type: 'linear' | 'radial'; stops: string[] };
}

export const EFFECTS: readonly EffectDef[] = [
  { id: 'off', label: 'Off', kind: 'none' },
  { id: 'blur-light', label: 'Light blur', kind: 'blur', blurPx: 8 },
  { id: 'blur-strong', label: 'Strong blur', kind: 'blur', blurPx: 22 },
  { id: 'bg-graphite', label: 'Graphite', kind: 'image', gradient: { type: 'radial', stops: ['#2f3943', '#161c22', '#0b0f13'] } },
  { id: 'bg-dusk', label: 'Dusk', kind: 'image', gradient: { type: 'linear', stops: ['#2a2140', '#6c3d5c', '#d4906c'] } },
  { id: 'bg-ocean', label: 'Ocean', kind: 'image', gradient: { type: 'linear', stops: ['#0a2533', '#1b5868', '#86b9b6'] } },
];

export function effectById(id: string | null | undefined): EffectDef {
  return EFFECTS.find(e => e.id === id) ?? EFFECTS[0];
}

export const EFFECT_STORAGE_KEY = 'studiolink.backgroundEffect';
type KV = Pick<Storage, 'getItem' | 'setItem'>;
export function loadEffect(storage: KV | null | undefined): EffectId {
  try { return effectById(storage?.getItem(EFFECT_STORAGE_KEY)).id; } catch { return 'off'; }
}
export function saveEffect(storage: KV | null | undefined, id: EffectId): void {
  try { storage?.setItem(EFFECT_STORAGE_KEY, id); } catch { /* Private mode: this page only. */ }
}

/** Short side of the processed call track: the call sender never sends more. */
export const EFFECT_MAX_SHORT_SIDE = 720;
/** Width of the image the segmenter sees (the selfie model works at 256²). */
export const MASK_WIDTH = 256;

export interface CompositorParams {
  out: { width: number; height: number };
  mask: { width: number; height: number };
  /** Background blur drawn on a quarter-size canvas: radius there, in px. */
  blurPx: number;
  /** Downscale of the background blur canvas. */
  blurScale: number;
}

const even = (n: number) => Math.max(2, Math.round(n / 2) * 2);

/** Sizes for one input geometry and effect. Never upscales the camera. */
export function compositorParams(effect: EffectDef, width: number, height: number): CompositorParams {
  const w = width > 0 ? width : 1280, h = height > 0 ? height : 720;
  const scale = Math.min(1, EFFECT_MAX_SHORT_SIDE / Math.min(w, h));
  const out = { width: even(w * scale), height: even(h * scale) };
  const maskW = Math.min(MASK_WIDTH, out.width);
  const mask = { width: even(maskW), height: even((maskW * out.height) / out.width) };
  const blurScale = 4;
  const blurPx = effect.kind === 'blur' ? Math.max(1, ((effect.blurPx ?? 0) * Math.min(out.width, out.height)) / EFFECT_MAX_SHORT_SIDE / blurScale) : 0;
  return { out, mask, blurPx: Math.round(blurPx * 10) / 10, blurScale };
}

/**
 * Person confidence → mask alpha (white RGB). A smoothstep between `lo` and
 * `hi` gives soft edges; `prev` (last frame's alpha) damps flicker.
 */
export function maskToAlpha(conf: Float32Array, rgba: Uint8ClampedArray, prev?: Uint8ClampedArray, lo = 0.3, hi = 0.75, keep = 0.35): void {
  const span = hi - lo;
  for (let i = 0, j = 0; i < conf.length; i++, j += 4) {
    const t = Math.min(1, Math.max(0, (conf[i] - lo) / span));
    let a = t * t * (3 - 2 * t) * 255;
    if (prev) a = prev[j + 3] * keep + a * (1 - keep);
    rgba[j] = rgba[j + 1] = rgba[j + 2] = 255;
    rgba[j + 3] = a;
  }
}

/** Which confidence mask is the person: by model label, else the only/second one. */
export function personMaskIndex(labels: string[], maskCount: number): number {
  const i = labels.findIndex(l => /person|selfie|foreground|human/i.test(l));
  if (i >= 0 && i < maskCount) return i;
  return maskCount > 1 ? 1 : 0;
}

/**
 * Rolling frame rate with collapse detection: after a warm-up, sustained
 * output below `minFps` for `windowMs` means the device can't keep up.
 */
export class FpsMonitor {
  private times: number[] = [];
  private startedAt: number | null = null;
  private lowSince: number | null = null;
  constructor(readonly minFps = 15, readonly windowMs = 5000, readonly warmupMs = 4000) {}

  /** A frame was produced at `now` (ms). */
  frame(now: number): void {
    if (this.startedAt === null) this.startedAt = now;
    this.times.push(now);
    while (this.times.length && this.times[0] < now - 2000) this.times.shift();
  }

  /** Frames per second over the last 2 s. */
  fps(now: number): number {
    const recent = this.times.filter(t => t >= now - 2000);
    if (recent.length < 2) return recent.length / 2;
    return ((recent.length - 1) * 1000) / Math.max(1, recent[recent.length - 1] - recent[0]);
  }

  /** True once output stayed below minFps for windowMs after the warm-up. */
  collapsed(now: number): boolean {
    if (this.startedAt === null || now - this.startedAt < this.warmupMs) return false;
    if (this.fps(now) >= this.minFps) { this.lowSince = null; return false; }
    if (this.lowSince === null) this.lowSince = now;
    return now - this.lowSince >= this.windowMs;
  }

  /** Forget history (camera turned off, input switched). */
  reset(): void { this.times = []; this.startedAt = null; this.lowSince = null; }
}

// ------------------------------------------------------------------ runtime

export const MEDIAPIPE_BASE = '/mediapipe';

/** Where MediaPipe Tasks posts usage logs (every 60 s while a task exists). */
export const MEDIAPIPE_TELEMETRY_URL = /^https:\/\/odml\.pa\.googleapis\.com\//;
let telemetryBlocked = false;

/**
 * MediaPipe Tasks (tasks-vision 1.0.1) sends usage logs to Google with fetch
 * once a minute. p2p.video contacts no hosted service (AGENTS.md); the CSP
 * already refuses the request, but the attempt still happens (and Firefox
 * logs it as a console error, seen by the Safari/Firefox suite, SL-P1-12).
 * Refusing it here, before any network: the logger's send fails, it marks
 * itself failed and stops its timer. Every other fetch passes through.
 */
export function blockMediaPipeTelemetry(): void {
  if (telemetryBlocked || typeof globalThis.fetch !== 'function') return;
  telemetryBlocked = true;
  const original = globalThis.fetch.bind(globalThis);
  globalThis.fetch = (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    if (MEDIAPIPE_TELEMETRY_URL.test(url)) return Promise.reject(new TypeError('p2p.video does not send MediaPipe usage logs'));
    return original(input, init);
  };
}

interface Mask { getAsFloat32Array(): Float32Array; width: number; height: number }
interface SegResult { confidenceMasks?: Mask[] }
interface Segmenter {
  segmentForVideo(image: TexImageSource, timestamp: number, callback: (r: SegResult) => void): void;
  getLabels(): string[];
  close(): void;
}

declare class MediaStreamTrackProcessor { constructor(init: { track: MediaStreamTrack }); readonly readable: ReadableStream<VideoFrame> }
declare class MediaStreamTrackGenerator extends MediaStreamTrack { constructor(init: { kind: 'video' }); readonly writable: WritableStream<VideoFrame> }

export interface ProcessorStats { fps: number; processMs: number; segmentMs: number; delegate: 'GPU' | 'CPU'; pipeline: 'insertable-streams' | 'canvas' }

type Canvas2D = CanvasRenderingContext2D;
function canvas(w: number, h: number): [HTMLCanvasElement, Canvas2D] {
  const c = document.createElement('canvas');
  c.width = w; c.height = h;
  return [c, c.getContext('2d', { willReadFrequently: false })!];
}

/**
 * Turns a camera track into a processed track for the call. One instance per
 * page; the input can change (device switch) without changing the output.
 */
export class BackgroundProcessor {
  onCollapse?: (fps: number) => void;
  private effect: EffectDef = EFFECTS[0];
  private input: MediaStreamTrack | null = null;
  private output: MediaStreamTrack | null = null;
  private generation = 0;
  private params: CompositorParams | null = null;
  private outCanvas: HTMLCanvasElement | null = null;
  private outCtx: Canvas2D | null = null;
  private small!: [HTMLCanvasElement, Canvas2D];
  private maskC!: [HTMLCanvasElement, Canvas2D];
  private blurC!: [HTMLCanvasElement, Canvas2D];
  private personC!: [HTMLCanvasElement, Canvas2D];
  private bgImage: HTMLCanvasElement | null = null;
  private maskData: ImageData | null = null;
  private prevAlpha: Uint8ClampedArray | null = null;
  private personIndex = 0;
  private readonly monitor = new FpsMonitor();
  private processMs = 0;
  private segmentMs = 0;
  private lastTs = 0;
  private video: HTMLVideoElement | null = null;
  private pumping: Promise<void> = Promise.resolve();

  private constructor(private readonly segmenter: Segmenter, readonly delegate: 'GPU' | 'CPU') {
    this.personIndex = personMaskIndex(segmenter.getLabels?.() ?? [], 1);
  }

  /** Loads the runtime and model from our origin. Throws when unavailable. */
  static async load(base = MEDIAPIPE_BASE, prefer: 'GPU' | 'CPU' = 'CPU'): Promise<BackgroundProcessor> {
    blockMediaPipeTelemetry();
    const vision = await import('@mediapipe/tasks-vision');
    // Only the SIMD build is shipped (every current browser has WASM SIMD).
    const fileset = { wasmLoaderPath: `${base}/vision_wasm_internal.js`, wasmBinaryPath: `${base}/vision_wasm_internal.wasm` };
    const options = (delegate: 'GPU' | 'CPU') => ({
      baseOptions: { modelAssetPath: `${base}/selfie_segmenter.tflite`, delegate },
      runningMode: 'VIDEO' as const, outputCategoryMask: false, outputConfidenceMasks: true,
    });
    const other = prefer === 'GPU' ? 'CPU' : 'GPU';
    try {
      return new BackgroundProcessor(await vision.ImageSegmenter.createFromOptions(fileset, options(prefer)) as unknown as Segmenter, prefer);
    } catch {
      return new BackgroundProcessor(await vision.ImageSegmenter.createFromOptions(fileset, options(other)) as unknown as Segmenter, other);
    }
  }

  get pipeline(): ProcessorStats['pipeline'] {
    return typeof MediaStreamTrackProcessor !== 'undefined' && typeof MediaStreamTrackGenerator !== 'undefined' ? 'insertable-streams' : 'canvas';
  }

  stats(): ProcessorStats {
    return { fps: Math.round(this.monitor.fps(performance.now()) * 10) / 10, processMs: Math.round(this.processMs * 10) / 10, segmentMs: Math.round(this.segmentMs * 10) / 10, delegate: this.delegate, pipeline: this.pipeline };
  }

  get effectId(): EffectId { return this.effect.id; }
  get track(): MediaStreamTrack | null { return this.output; }

  setEffect(effect: EffectDef): void {
    this.effect = effect;
    this.bgImage = null;
    // Blur radius depends on the effect; canvas sizes don't.
    if (this.params) this.params = { ...this.params, blurPx: compositorParams(effect, this.params.out.width, this.params.out.height).blurPx };
  }

  /** Starts (or retargets) processing of `input`; returns the processed track. */
  start(input: MediaStreamTrack): MediaStreamTrack {
    if (this.output && this.output.readyState === 'live' && this.input === input) return this.output;
    this.configure(input);
    const gen = ++this.generation;
    this.monitor.reset();
    if (this.pipeline === 'insertable-streams') {
      if (!this.output || this.output.readyState !== 'live' || !(this.output instanceof MediaStreamTrackGenerator)) {
        this.output?.stop();
        this.output = new MediaStreamTrackGenerator({ kind: 'video' });
      }
      // Same output across input switches: the next reader takes over the
      // writer once the previous one has let go of it.
      const generator = this.output as MediaStreamTrackGenerator;
      this.pumping = this.pumping.then(() => this.pump(gen, input, generator)).catch(() => {});
    } else {
      if (!this.output || this.output.readyState !== 'live') {
        const stream = this.outCanvas!.captureStream(30);
        this.output = stream.getVideoTracks()[0];
      }
      void this.canvasLoop(gen, input);
    }
    this.input = input;
    return this.output!;
  }

  /** Stops processing and the processed track (the camera is not touched). */
  stop(): void {
    this.generation++;
    this.output?.stop();
    this.output = null;
    this.input = null;
    if (this.video) { this.video.srcObject = null; this.video = null; }
    this.monitor.reset();
  }

  /** Releases the segmenter. */
  close(): void { this.stop(); try { this.segmenter.close(); } catch { /* Already closed. */ } }

  private configure(input: MediaStreamTrack): void {
    const s = input.getSettings();
    this.resize(s.width ?? 1280, s.height ?? 720);
  }

  /** (Re)allocates canvases for a camera geometry; cheap when nothing changed. */
  private resize(width: number, height: number): void {
    const params = compositorParams(this.effect, width, height);
    const same = this.params && this.outCanvas && this.params.out.width === params.out.width && this.params.out.height === params.out.height;
    this.params = params;
    if (same) return;
    if (!this.outCanvas) [this.outCanvas, this.outCtx] = canvas(params.out.width, params.out.height);
    else { this.outCanvas.width = params.out.width; this.outCanvas.height = params.out.height; }
    this.small = canvas(params.mask.width, params.mask.height);
    this.maskC = canvas(params.mask.width, params.mask.height);
    this.personC = canvas(params.out.width, params.out.height);
    this.blurC = canvas(Math.max(2, Math.round(params.out.width / params.blurScale)), Math.max(2, Math.round(params.out.height / params.blurScale)));
    this.maskData = new ImageData(params.mask.width, params.mask.height);
    this.prevAlpha = null;
    this.bgImage = null;
  }

  private background(): HTMLCanvasElement | null {
    const g = this.effect.gradient;
    if (!g || !this.params) return null;
    if (this.bgImage) return this.bgImage;
    const { width: w, height: h } = this.params.out;
    const [c, ctx] = canvas(w, h);
    const grad = g.type === 'radial'
      ? ctx.createRadialGradient(w * 0.5, h * 0.42, 0, w * 0.5, h * 0.5, Math.hypot(w, h) * 0.6)
      : ctx.createLinearGradient(0, 0, w, h);
    g.stops.forEach((stop, i) => grad.addColorStop(i / Math.max(1, g.stops.length - 1), stop));
    ctx.fillStyle = grad;
    ctx.fillRect(0, 0, w, h);
    this.bgImage = c;
    return c;
  }

  /** Draws one composited frame from `source` into the output canvas. */
  private composite(source: CanvasImageSource): void {
    const p = this.params!, out = this.outCtx!;
    const { width: W, height: H } = p.out;
    const t0 = performance.now();
    // 1. Segment a small copy of the frame.
    const [smallCanvas, smallCtx] = this.small;
    smallCtx.drawImage(source, 0, 0, p.mask.width, p.mask.height);
    const ts = Math.max(this.lastTs + 1, performance.now());
    this.lastTs = ts;
    let gotMask = false;
    this.segmenter.segmentForVideo(smallCanvas, ts, result => {
      const masks = result.confidenceMasks ?? [];
      const mask = masks[Math.min(this.personIndex, masks.length - 1)];
      if (!mask || !this.maskData) return;
      const conf = mask.getAsFloat32Array();
      if (conf.length !== this.maskData.width * this.maskData.height) return;
      maskToAlpha(conf, this.maskData.data, this.prevAlpha ?? undefined);
      this.prevAlpha = new Uint8ClampedArray(this.maskData.data);
      gotMask = true;
    });
    if (gotMask) this.maskC[1].putImageData(this.maskData!, 0, 0);
    this.segmentMs = this.segmentMs * 0.9 + (performance.now() - t0) * 0.1;
    // 2. Background: blurred frame (on a quarter-size canvas) or a generated image.
    const bg = this.background();
    if (bg) out.drawImage(bg, 0, 0, W, H);
    else {
      const [blurCanvas, blurCtx] = this.blurC;
      blurCtx.filter = `blur(${p.blurPx}px)`;
      blurCtx.drawImage(source, 0, 0, blurCanvas.width, blurCanvas.height);
      blurCtx.filter = 'none';
      out.imageSmoothingQuality = 'low';
      out.drawImage(blurCanvas, 0, 0, W, H);
    }
    // 3. Person: the upscaled (feathered) mask keeps only the person of the full frame.
    const [personCanvas, personCtx] = this.personC;
    personCtx.globalCompositeOperation = 'copy';
    personCtx.imageSmoothingQuality = 'high';
    personCtx.drawImage(this.maskC[0], 0, 0, W, H);
    personCtx.globalCompositeOperation = 'source-in';
    personCtx.drawImage(source, 0, 0, W, H);
    personCtx.globalCompositeOperation = 'source-over';
    out.drawImage(personCanvas, 0, 0);
    this.processMs = this.processMs * 0.9 + (performance.now() - t0) * 0.1;
  }

  private tick(input: MediaStreamTrack): void {
    const now = performance.now();
    // A disabled camera delivers black frames slowly: that's not a collapse.
    if (!input.enabled || input.muted) { this.monitor.reset(); return; }
    this.monitor.frame(now);
    if (this.monitor.collapsed(now)) {
      const fps = this.monitor.fps(now);
      this.monitor.reset();
      this.onCollapse?.(fps);
    }
  }

  private async pump(gen: number, input: MediaStreamTrack, generator: MediaStreamTrackGenerator): Promise<void> {
    if (gen !== this.generation || generator.readyState !== 'live') return;
    const writer = generator.writable.getWriter();
    const reader = new MediaStreamTrackProcessor({ track: input }).readable.getReader();
    try {
      while (gen === this.generation) {
        const { value: frame, done } = await reader.read();
        if (done || !frame) break;
        if (gen !== this.generation) { frame.close(); break; }
        try {
          // The camera's actual frames decide the geometry (settings can lag a switch).
          this.resize(frame.displayWidth, frame.displayHeight);
          this.composite(frame);
          const timestamp = frame.timestamp;
          frame.close();
          await writer.write(new VideoFrame(this.outCanvas!, { timestamp }));
          this.tick(input);
        } catch (err) {
          frame.close();
          throw err;
        }
      }
    } finally {
      reader.cancel().catch(() => {});
      writer.releaseLock();
    }
  }

  // Fallback (Safari, Firefox): a hidden video element paced by
  // requestVideoFrameCallback, output through canvas.captureStream().
  private async canvasLoop(gen: number, input: MediaStreamTrack): Promise<void> {
    const video = this.video ?? document.createElement('video');
    this.video = video;
    video.muted = true;
    video.playsInline = true;
    video.srcObject = new MediaStream([input]);
    await video.play().catch(() => {});
    const step = () => {
      if (gen !== this.generation) return;
      if (video.videoWidth > 0) {
        this.resize(video.videoWidth, video.videoHeight);
        this.composite(video);
        this.tick(input);
      }
      if ('requestVideoFrameCallback' in video) (video as HTMLVideoElement & { requestVideoFrameCallback(cb: () => void): number }).requestVideoFrameCallback(step);
      else requestAnimationFrame(step);
    };
    step();
  }
}
