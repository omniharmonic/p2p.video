// Live-caption view state [SL-P4-2]. Pure and self-contained (no imports) so
// node unit tests can transpile and run it. Rendering lives in
// components/Captions.ts; recognition happens on the host's Mac.

export interface CaptionInput {
  id: string;
  seq: number;
  participantId: string;
  name: string;
  text: string;
  isFinal: boolean;
  startMs: number;
  endMs: number;
}

export interface CaptionLine {
  id: string;
  participantId: string;
  name: string;
  text: string;
  isFinal: boolean;
  seq: number;
  /** Seq when the line first appeared; lines keep this order. */
  order: number;
  /** Viewer clock (ms) of the last update; drives expiry. */
  updatedAt: number;
}

export interface CaptionBoardOptions {
  /** Lines shown at once (Meet shows 2–3). */
  maxLines: number;
  /** Characters kept per line; longer text shows its most recent words. */
  maxChars: number;
  /** A final line disappears this long after its last update. */
  finalTtlMs: number;
  /** A partial nobody finalized disappears after this (lost final). */
  partialTtlMs: number;
}

export const DEFAULT_CAPTION_BOARD: CaptionBoardOptions = { maxLines: 3, maxChars: 160, finalTtlMs: 8000, partialTtlMs: 15000 };

/**
 * The on-screen caption lines. A partial is replaced in place by later
 * partials and by its final (same id); a final with empty text withdraws it.
 * Out-of-order updates for a line (lower seq) are ignored. Only the newest
 * maxLines lines are kept, in order of first appearance.
 */
export class CaptionBoard {
  private items: CaptionLine[] = [];
  readonly options: CaptionBoardOptions;

  constructor(options: Partial<CaptionBoardOptions> = {}) {
    this.options = { ...DEFAULT_CAPTION_BOARD, ...options };
  }

  /** Applies a caption; returns true if the visible lines changed. */
  apply(c: CaptionInput, now: number): boolean {
    if (!c || typeof c.id !== 'string' || typeof c.text !== 'string') return false;
    const i = this.items.findIndex(l => l.id === c.id);
    if (i >= 0) {
      const line = this.items[i];
      if (c.seq <= line.seq) return false;
      if (c.isFinal && !c.text.trim()) { this.items.splice(i, 1); return true; }
      if (line.isFinal && !c.isFinal) return false; // a final is never reopened
      this.items[i] = { ...line, text: c.text, isFinal: c.isFinal, seq: c.seq, name: c.name || line.name, updatedAt: now };
      return true;
    }
    if (!c.text.trim()) return false;
    this.items.push({ id: c.id, participantId: c.participantId, name: c.name, text: c.text, isFinal: c.isFinal, seq: c.seq, order: c.seq, updatedAt: now });
    this.items.sort((a, b) => a.order - b.order);
    if (this.items.length > this.options.maxLines) this.items.splice(0, this.items.length - this.options.maxLines);
    return true;
  }

  /** Drops lines past their TTL; returns true if any were removed. */
  expire(now: number): boolean {
    const before = this.items.length;
    this.items = this.items.filter(l => now - l.updatedAt < (l.isFinal ? this.options.finalTtlMs : this.options.partialTtlMs));
    return this.items.length !== before;
  }

  clear(): void { this.items = []; }

  /** Lines oldest first, text trimmed to its most recent words. */
  lines(): CaptionLine[] {
    return this.items.map(l => ({ ...l, text: tailText(l.text, this.options.maxChars) }));
  }
}

/** The last ~max characters of text, cut at a word boundary, with a leading ellipsis. */
export function tailText(text: string, max: number): string {
  const t = text.trim().replace(/\s+/g, ' ');
  if (t.length <= max) return t;
  let tail = t.slice(t.length - max);
  const space = tail.indexOf(' ');
  if (space > 0 && space < max / 3) tail = tail.slice(space + 1);
  return '…' + tail;
}

/**
 * Binary caption audio frame (docs/integrations/CAPTIONS.md): version 1, key
 * length, ASCII key (the speaking participant's ID), then s16le mono PCM.
 */
export function encodeCaptionFrame(key: string, pcm: Int16Array): ArrayBuffer {
  if (!/^[A-Za-z0-9_.:-]{1,64}$/.test(key)) throw new Error('invalid caption stream key');
  const out = new Uint8Array(2 + key.length + pcm.length * 2);
  out[0] = 1;
  out[1] = key.length;
  for (let i = 0; i < key.length; i++) out[2 + i] = key.charCodeAt(i);
  const view = new DataView(out.buffer);
  for (let i = 0; i < pcm.length; i++) view.setInt16(2 + key.length + i * 2, pcm[i], true);
  return out.buffer;
}

/** True when every sample is exactly zero (a muted or silent track): such frames are skipped. */
export function isDigitalSilence(pcm: Int16Array): boolean {
  for (let i = 0; i < pcm.length; i++) if (pcm[i] !== 0) return false;
  return true;
}

// Per-viewer preference: show captions when the host has them on.
export const CAPTIONS_VISIBLE_KEY = 'studiolink.captions.visible';
export const CAPTIONS_LANGUAGE_KEY = 'studiolink.captions.language';

interface StorageLike { getItem(k: string): string | null; setItem(k: string, v: string): void }

export function readCaptionsVisible(storage: StorageLike | null | undefined): boolean {
  try { return storage?.getItem(CAPTIONS_VISIBLE_KEY) !== '0'; } catch { return true; }
}

export function writeCaptionsVisible(storage: StorageLike | null | undefined, visible: boolean): void {
  try { storage?.setItem(CAPTIONS_VISIBLE_KEY, visible ? '1' : '0'); } catch { /* private mode: not remembered */ }
}

/** Languages offered to the host; the Mac's recognizer decides what actually works. */
export const CAPTION_LANGUAGES: Array<{ tag: string; label: string }> = [
  { tag: 'en-US', label: 'English (US)' },
  { tag: 'en-GB', label: 'English (UK)' },
  { tag: 'en-AU', label: 'English (Australia)' },
  { tag: 'en-IN', label: 'English (India)' },
  { tag: 'es-ES', label: 'Español (España)' },
  { tag: 'es-MX', label: 'Español (México)' },
  { tag: 'fr-FR', label: 'Français' },
  { tag: 'de-DE', label: 'Deutsch' },
  { tag: 'it-IT', label: 'Italiano' },
  { tag: 'pt-BR', label: 'Português (Brasil)' },
  { tag: 'ja-JP', label: '日本語' },
  { tag: 'ko-KR', label: '한국어' },
  { tag: 'zh-CN', label: '中文 (普通话)' },
];

export function readCaptionLanguage(storage: StorageLike | null | undefined): string {
  try {
    const v = storage?.getItem(CAPTIONS_LANGUAGE_KEY);
    return v && CAPTION_LANGUAGES.some(l => l.tag === v) ? v : 'en-US';
  } catch { return 'en-US'; }
}
