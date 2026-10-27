// Teleprompter model [SL-P5-1]: script parsing, scroll math and the
// visibility rules the hub enforces. No DOM, no imports: unit-tested in node.

export type PrompterRole = 'host' | 'producer' | 'guest' | '';
export type Visibility = 'host' | 'crew' | 'everyone';

export interface ScriptBlock { kind: 'h1' | 'h2' | 'p' | 'gap'; text: string; }

export const FONT_MIN = 24;
export const FONT_MAX = 96;
export const FONT_STEP = 4;
export const FONT_DEFAULT = 44;
export const SPEED_MIN = 1;
export const SPEED_MAX = 10;
export const SPEED_DEFAULT = 3;
/** Reader line height (CSS line-height of the prompter text). */
export const LINE_HEIGHT = 1.45;
/** Longest frame step the auto-scroll applies, so a stalled tab doesn't jump. */
export const MAX_STEP_MS = 250;
/** How often a sharing host sends its position while it changes. */
export const SHARE_INTERVAL_MS = 250;

/**
 * Splits a script into reader blocks: "# " is a section heading, "## " (or
 * deeper) a sub-heading, blank lines become one gap, every other line is
 * its own paragraph (line breaks are the writer's pacing). Text stays plain:
 * callers render it with textContent.
 */
export function parseScript(text: string): ScriptBlock[] {
  const out: ScriptBlock[] = [];
  for (const raw of text.replace(/\r\n?/g, '\n').split('\n')) {
    const line = raw.trim();
    if (!line) {
      if (out.length && out[out.length - 1].kind !== 'gap') out.push({ kind: 'gap', text: '' });
      continue;
    }
    const h = /^(#{1,6})\s+(.*)$/.exec(line);
    if (h) out.push({ kind: h[1].length === 1 ? 'h1' : 'h2', text: h[2].trim() });
    else out.push({ kind: 'p', text: line });
  }
  while (out.length && out[out.length - 1].kind === 'gap') out.pop();
  return out;
}

export function clampFont(px: number): number {
  if (!Number.isFinite(px)) return FONT_DEFAULT;
  return Math.min(FONT_MAX, Math.max(FONT_MIN, Math.round(px)));
}

export function clampSpeed(speed: number): number {
  if (!Number.isFinite(speed)) return SPEED_DEFAULT;
  return Math.min(SPEED_MAX, Math.max(SPEED_MIN, Math.round(speed)));
}

/**
 * Auto-scroll rate in CSS px per second. Speed is "lines per 10 s" so it
 * reads the same at every font size: speed 3 is ~0.3 lines/s, ~110 words a
 * minute at six words a line; speed 10 is a line a second.
 */
export function pixelsPerSecond(speed: number, fontPx: number): number {
  return (clampSpeed(speed) / 10) * clampFont(fontPx) * LINE_HEIGHT;
}

/** Largest scrollTop of a scroller. */
export function maxScroll(scrollHeight: number, clientHeight: number): number {
  return Math.max(0, scrollHeight - clientHeight);
}

/** Next scrollTop after dtMs of auto-scroll, clamped to [0, max]. */
export function advance(scrollTop: number, dtMs: number, speed: number, fontPx: number, max: number): number {
  const dt = Math.min(MAX_STEP_MS, Math.max(0, dtMs)) / 1000;
  return Math.min(max, Math.max(0, scrollTop + pixelsPerSecond(speed, fontPx) * dt));
}

/** One reading line up (-1) or down (+1). */
export function nudge(scrollTop: number, direction: -1 | 1, fontPx: number, max: number): number {
  return Math.min(max, Math.max(0, scrollTop + direction * clampFont(fontPx) * LINE_HEIGHT));
}

/** Scroll position as a 0..1 fraction (what the host shares). */
export function toFraction(scrollTop: number, max: number): number {
  if (max <= 0 || !Number.isFinite(scrollTop)) return 0;
  return Math.min(1, Math.max(0, scrollTop / max));
}

/** A shared fraction as this reader's scrollTop. */
export function fromFraction(fraction: number, max: number): number {
  if (!Number.isFinite(fraction)) return 0;
  return Math.min(1, Math.max(0, fraction)) * Math.max(0, max);
}

/** Whether the reader reached the end (auto-scroll then pauses). */
export function atEnd(scrollTop: number, max: number): boolean {
  return max <= 0 || scrollTop >= max - 0.5;
}

/** Mirrors the hub: who may read a script with this visibility. */
export function canSee(role: PrompterRole, visibility: Visibility): boolean {
  if (role === 'host') return true;
  if (role === 'producer') return visibility === 'crew' || visibility === 'everyone';
  if (role === 'guest') return visibility === 'everyone';
  return false;
}

/** Mirrors the hub: who may edit a script with this visibility. */
export function canEdit(role: PrompterRole, visibility: Visibility): boolean {
  return role === 'host' || (role === 'producer' && visibility !== 'host');
}

/** UTF-8 size of the script (the hub caps it at PrompterMaxBytes). */
export function utf8Bytes(text: string): number {
  return new TextEncoder().encode(text).length;
}

/**
 * Whether a sharing host should send its position now: on a play/pause or
 * speed change at once, otherwise at most every SHARE_INTERVAL_MS and only
 * when the position moved.
 */
export function shouldShare(last: { at: number; position: number; playing: boolean; speed: number } | null, now: number, position: number, playing: boolean, speed: number): boolean {
  if (!last) return true;
  if (last.playing !== playing || last.speed !== speed) return true;
  if (now - last.at < SHARE_INTERVAL_MS) return false;
  return Math.abs(last.position - position) > 0.0005;
}

export const VISIBILITY_LABELS: Record<Visibility, string> = {
  host: 'Only me',
  crew: 'Host + producers',
  everyone: 'Everyone in the call',
};
