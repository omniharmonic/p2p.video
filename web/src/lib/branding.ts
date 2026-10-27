// Branding and lower thirds [SL-P5-2]: the shared brand state (from the
// hub's "branding" message), lower-third text, and the pure layout and
// animation math the livestream compositor and the tile labels use. No DOM
// and no imports, so it is unit-tested in node.

export interface Rect { x: number; y: number; w: number; h: number; }
export type LowerThirdsMode = 'speaker' | 'manual' | 'off';

export interface LowerThirdEntry { participantId: string; name?: string; title?: string; }

export interface BrandState {
  showName: string;
  accent: string;
  tileLabels: boolean;
  lowerThirdsMode: LowerThirdsMode;
  logoVersion?: string;
  lowerThirds: LowerThirdEntry[];
  /** In a room's meeting, lower thirds are remembered for the next episode [SL-P5-4]. */
  rememberTitles?: boolean;
}

export const DEFAULT_ACCENT = '#bfdbe8';

export const emptyBrand = (): BrandState => ({ showName: '', accent: DEFAULT_ACCENT, tileLabels: false, lowerThirdsMode: 'speaker', lowerThirds: [] });

/** The current brand plus who "you" are (the local tile has no participant id). */
class BrandStore {
  private state: BrandState = emptyBrand();
  private listeners = new Set<(s: BrandState) => void>();
  selfId = '';
  get(): BrandState { return this.state; }
  set(next: BrandState): void {
    this.state = { ...emptyBrand(), ...next, accent: validAccent(next.accent) ? next.accent.toLowerCase() : DEFAULT_ACCENT, lowerThirds: Array.isArray(next.lowerThirds) ? next.lowerThirds : [] };
    for (const fn of this.listeners) fn(this.state);
  }
  subscribe(fn: (s: BrandState) => void): () => void { this.listeners.add(fn); return () => this.listeners.delete(fn); }
  /** Lower-third text for a participant (the local tile is resolved to selfId). */
  text(participantId: string, displayName: string): { name: string; title: string } {
    const id = participantId === 'local-tile' || participantId === 'local' ? this.selfId : participantId;
    return lowerThirdText(this.state, id, displayName);
  }
}

export const brandStore = new BrandStore();

export function validAccent(s: unknown): s is string {
  return typeof s === 'string' && /^#[0-9a-fA-F]{6}$/.test(s);
}

/** Name and title for a participant: an override, else the display name. */
export function lowerThirdText(brand: BrandState, participantId: string, displayName: string): { name: string; title: string } {
  const e = brand.lowerThirds.find(l => l.participantId === participantId);
  return { name: (e?.name || displayName || '').trim(), title: (e?.title || '').trim() };
}

function rgb(hex: string): [number, number, number] {
  const h = validAccent(hex) ? hex : DEFAULT_ACCENT;
  return [1, 3, 5].map(i => parseInt(h.slice(i, i + 2), 16)) as [number, number, number];
}

function hex([r, g, b]: number[]): string {
  return '#' + [r, g, b].map(v => Math.round(Math.min(255, Math.max(0, v))).toString(16).padStart(2, '0')).join('');
}

/** WCAG relative luminance of a #rrggbb colour. */
export function luminance(color: string): number {
  const [r, g, b] = rgb(color).map(v => { const c = v / 255; return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4; });
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

/** Text colour that reads on a background (graphite or white). */
export function textOn(background: string): string {
  const l = luminance(background);
  // Contrast against near-black text vs white text; pick the higher.
  return (l + 0.05) / (0.0074 + 0.05) >= 1.05 / (l + 0.05) ? '#0b0f13' : '#ffffff';
}

/** Linear mix of two colours, t=0 → a, t=1 → b. */
export function mixColor(a: string, b: string, t: number): string {
  const x = rgb(a), y = rgb(b);
  return hex(x.map((v, i) => v + (y[i] - v) * Math.min(1, Math.max(0, t))));
}

/**
 * Branded program frame: a top band for the show name and logo, and the
 * stage the tiles are laid out in. Unbranded, the stage is the whole canvas.
 */
export function brandedFrame(width: number, height: number, branded: boolean): { band: Rect | null; stage: Rect } {
  if (!branded) return { band: null, stage: { x: 0, y: 0, w: width, h: height } };
  const bandH = Math.round(height * 0.1);
  const bottom = Math.round(height * 0.035);
  const side = Math.round(width * 0.025);
  return { band: { x: 0, y: 0, w: width, h: bandH }, stage: { x: side, y: bandH, w: width - 2 * side, h: height - bandH - bottom } };
}

/**
 * Logo placement: fitted (aspect kept) into the top-right corner, inside the
 * band when branded, at most 14% of the width and 9% of the height.
 */
export function logoRect(width: number, height: number, imgW: number, imgH: number, band: Rect | null): Rect {
  if (imgW <= 0 || imgH <= 0 || width <= 0 || height <= 0) return { x: 0, y: 0, w: 0, h: 0 };
  const margin = Math.round(Math.min(width, height) * 0.025);
  const maxW = width * 0.14;
  const maxH = band ? band.h - 2 * Math.round(band.h * 0.18) : height * 0.09;
  const scale = Math.min(maxW / imgW, maxH / imgH);
  const w = Math.max(1, Math.round(imgW * scale)), h = Math.max(1, Math.round(imgH * scale));
  const y = band ? band.y + Math.round((band.h - h) / 2) : margin;
  return { x: width - margin - w, y, w, h };
}

export interface LowerThirdMetrics { nameFont: number; titleFont: number; pad: number; strip: number; margin: number; gap: number; }

/** Font sizes and spacing for a lower third in a tile of this size. */
export function lowerThirdMetrics(tile: Rect): LowerThirdMetrics {
  const nameFont = Math.round(Math.min(40, Math.max(14, tile.h / 15, Math.min(tile.w / 30, tile.h / 11))));
  return {
    nameFont,
    titleFont: Math.max(12, Math.round(nameFont * 0.66)),
    pad: Math.round(nameFont * 0.55),
    strip: Math.max(3, Math.round(nameFont * 0.22)),
    margin: Math.round(Math.max(8, Math.min(tile.w, tile.h) * 0.045)),
    gap: Math.round(nameFont * 0.18),
  };
}

export interface LowerThirdLayout {
  box: Rect;
  strip: Rect;
  nameX: number;
  nameY: number;
  titleY: number;
  /** Max text width (text is cut with an ellipsis beyond it). */
  textW: number;
  alpha: number;
}

/**
 * A lower third in the bottom-left of a tile: accent strip, name, title.
 * `progress` (0..1) slides it in from the left and fades it; measured text
 * widths size the box, capped at 80% of the tile. Pure.
 */
export function lowerThirdLayout(tile: Rect, m: LowerThirdMetrics, nameW: number, titleW: number, hasTitle: boolean, progress: number): LowerThirdLayout {
  const p = Math.min(1, Math.max(0, progress));
  const maxTextW = Math.max(0, tile.w * 0.8 - 2 * m.pad - m.strip);
  const textW = Math.min(maxTextW, Math.max(nameW, hasTitle ? titleW : 0));
  const h = m.pad * 2 + m.nameFont + (hasTitle ? m.gap + m.titleFont : 0);
  const w = m.strip + 2 * m.pad + textW;
  const slide = (1 - p) * Math.min(w * 0.35, 80);
  const x = Math.round(tile.x + m.margin - slide);
  const y = Math.round(tile.y + tile.h - m.margin - h);
  return {
    box: { x, y, w: Math.round(w), h: Math.round(h) },
    strip: { x, y, w: m.strip, h: Math.round(h) },
    nameX: x + m.strip + m.pad,
    nameY: y + m.pad,
    titleY: y + m.pad + m.nameFont + m.gap,
    textW,
    alpha: p,
  };
}

export const LT_IN_MS = 450;
export const LT_HOLD_MS = 6000;
export const LT_OUT_MS = 450;

/** Ease-out cubic. */
export function easeOut(t: number): number { const x = Math.min(1, Math.max(0, t)); return 1 - (1 - x) ** 3; }

/**
 * Animation progress of a lower third shown at startMs: eases in, holds,
 * eases out; null once it has finished. Pure.
 */
export function lowerThirdProgress(startMs: number, nowMs: number, holdMs = LT_HOLD_MS): number | null {
  const t = nowMs - startMs;
  if (t < 0) return 0;
  if (t < LT_IN_MS) return easeOut(t / LT_IN_MS);
  if (t < LT_IN_MS + holdMs) return 1;
  if (t < LT_IN_MS + holdMs + LT_OUT_MS) return 1 - easeOut((t - LT_IN_MS - holdMs) / LT_OUT_MS);
  return null;
}

/** Re-showing the same speaker's lower third waits this long. */
export const LT_COOLDOWN_MS = 45_000;

/**
 * Decides which lower third is on air: an on-demand cue always shows; in
 * "speaker" mode a new active speaker shows theirs unless it was shown in
 * the last LT_COOLDOWN_MS. One at a time; a new one replaces the old.
 */
export class LowerThirdDirector {
  current: { participantId: string; startMs: number } | null = null;
  private lastShown = new Map<string, number>();
  private lastSpeaker: string | null = null;

  cue(participantId: string, nowMs: number): void {
    this.current = { participantId, startMs: nowMs };
    this.lastShown.set(participantId, nowMs);
  }

  /** Feed the active speaker every frame; returns what is on air and its progress. */
  update(speakerId: string | null, mode: LowerThirdsMode, nowMs: number): { participantId: string; progress: number } | null {
    if (mode === 'off') { this.current = null; this.lastSpeaker = speakerId; return null; }
    if (mode === 'speaker' && speakerId && speakerId !== this.lastSpeaker) {
      const last = this.lastShown.get(speakerId);
      if (last === undefined || nowMs - last >= LT_COOLDOWN_MS) this.cue(speakerId, nowMs);
    }
    this.lastSpeaker = speakerId;
    if (!this.current) return null;
    const progress = lowerThirdProgress(this.current.startMs, nowMs);
    if (progress === null) { this.current = null; return null; }
    return { participantId: this.current.participantId, progress };
  }
}
