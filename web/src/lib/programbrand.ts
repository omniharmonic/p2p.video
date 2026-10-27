// Branded livestream program [SL-P5-2]: background, show-name band, logo bug
// and animated lower thirds, painted through the compositor's decorator hook.
// Brand data comes from brandStore (the hub's "branding" message); the logo
// is an ImageBitmap the host's panel loaded from our own origin.
import type { CompositorSource, ProgramDecorator, Rect } from './compositor';
import { brandStore, brandedFrame, logoRect, lowerThirdLayout, lowerThirdMetrics, LowerThirdDirector, mixColor, textOn } from './branding';

const FONT = '"Helvetica Neue", Helvetica, Arial, sans-serif';
const CANVAS_BG = '#0c1014';

/** Cuts text with an ellipsis to fit maxW at the context's current font. */
function fit(ctx: CanvasRenderingContext2D, text: string, maxW: number): string {
  if (ctx.measureText(text).width <= maxW) return text;
  let lo = 0, hi = text.length;
  while (lo < hi) {
    const mid = (lo + hi + 1) >> 1;
    if (ctx.measureText(text.slice(0, mid) + '…').width <= maxW) lo = mid; else hi = mid - 1;
  }
  return text.slice(0, lo).trimEnd() + '…';
}

export class BrandDecorator implements ProgramDecorator {
  /** Branded layout variant: background, band with show name, logo. */
  branded = false;
  logo: ImageBitmap | null = null;
  readonly director = new LowerThirdDirector();
  private air: { participantId: string; progress: number } | null = null;

  stage(width: number, height: number): Rect { return brandedFrame(width, height, this.branded).stage; }

  background(ctx: CanvasRenderingContext2D, width: number, height: number): void {
    if (!this.branded) { ctx.fillStyle = CANVAS_BG; ctx.fillRect(0, 0, width, height); return; }
    const accent = brandStore.get().accent;
    const g = ctx.createLinearGradient(0, 0, width, height);
    g.addColorStop(0, mixColor(accent, CANVAS_BG, 0.55));
    g.addColorStop(0.55, mixColor(accent, CANVAS_BG, 0.85));
    g.addColorStop(1, CANVAS_BG);
    ctx.fillStyle = g;
    ctx.fillRect(0, 0, width, height);
  }

  wantsSpeaker(): boolean { return brandStore.get().lowerThirdsMode === 'speaker'; }

  onAir(speakerId: string | null, nowMs: number): string | null {
    this.air = this.director.update(speakerId, brandStore.get().lowerThirdsMode, nowMs);
    return this.air?.participantId ?? null;
  }

  /** Shows a participant's lower third now (on demand). */
  cue(participantId: string): void { this.director.cue(participantId, performance.now()); }

  overlay(ctx: CanvasRenderingContext2D, frame: { width: number; height: number; tiles: Array<{ source: CompositorSource; rect: Rect }>; onAir: string | null }): void {
    const brand = brandStore.get();
    if (this.branded) this.drawBand(ctx, frame.width, frame.height, brand.showName, brand.accent);
    const tile = this.air && frame.tiles.find(t => t.source.participantId === this.air!.participantId);
    if (tile && this.air) this.drawLowerThird(ctx, tile.rect, brandStore.text(tile.source.participantId, tile.source.name), brand.accent, this.air.progress);
  }

  private drawBand(ctx: CanvasRenderingContext2D, width: number, height: number, showName: string, accent: string): void {
    const { band } = brandedFrame(width, height, true);
    if (!band) return;
    ctx.save();
    const logo = this.logo;
    const lr = logo ? logoRect(width, height, logo.width, logo.height, band) : null;
    if (showName) {
      const size = Math.round(band.h * 0.36);
      const x = Math.round(width * 0.025);
      ctx.font = `600 ${size}px ${FONT}`;
      ctx.textBaseline = 'middle';
      ctx.textAlign = 'left';
      const maxW = (lr ? lr.x - x * 2 : width - x * 2);
      const text = fit(ctx, showName, Math.max(0, maxW - size * 0.7));
      // Accent tick before the name, then the name in light text.
      ctx.fillStyle = accent;
      ctx.fillRect(x, band.y + band.h / 2 - size * 0.5, Math.max(3, Math.round(size * 0.16)), size);
      ctx.fillStyle = '#f2f6f8';
      ctx.fillText(text, x + size * 0.45, band.y + band.h / 2);
    }
    if (logo && lr && lr.w > 0) ctx.drawImage(logo, lr.x, lr.y, lr.w, lr.h);
    ctx.restore();
  }

  private drawLowerThird(ctx: CanvasRenderingContext2D, tile: Rect, text: { name: string; title: string }, accent: string, progress: number): void {
    if (!text.name) return;
    const m = lowerThirdMetrics(tile);
    ctx.save();
    ctx.beginPath();
    ctx.rect(tile.x, tile.y, tile.w, tile.h);
    ctx.clip();
    ctx.textBaseline = 'top';
    ctx.textAlign = 'left';
    ctx.font = `600 ${m.nameFont}px ${FONT}`;
    const nameW = ctx.measureText(text.name).width;
    ctx.font = `400 ${m.titleFont}px ${FONT}`;
    const titleW = text.title ? ctx.measureText(text.title).width : 0;
    const l = lowerThirdLayout(tile, m, nameW, titleW, !!text.title, progress);
    ctx.globalAlpha = l.alpha;
    ctx.fillStyle = 'rgba(11,15,19,0.86)';
    ctx.fillRect(l.box.x, l.box.y, l.box.w, l.box.h);
    ctx.fillStyle = accent;
    ctx.fillRect(l.strip.x, l.strip.y, l.strip.w, l.strip.h);
    ctx.font = `600 ${m.nameFont}px ${FONT}`;
    ctx.fillStyle = '#f2f6f8';
    ctx.fillText(fit(ctx, text.name, l.textW), l.nameX, l.nameY);
    if (text.title) {
      ctx.font = `400 ${m.titleFont}px ${FONT}`;
      ctx.fillStyle = textOn(accent) === '#ffffff' ? '#c9d4db' : mixColor(accent, '#ffffff', 0.35);
      ctx.fillText(fit(ctx, text.title, l.textW), l.nameX, l.titleY);
    }
    ctx.restore();
  }
}

/**
 * Loads the session logo for the host's compositor: a fetch with the host
 * token (never in a URL), decoded to an ImageBitmap, so the canvas draws it
 * under the page CSP (img-src 'self' data: blob:) without an <img> element.
 */
export async function loadLogo(sessionId: string, token: string): Promise<ImageBitmap | null> {
  if (!sessionId || !token) return null;
  const res = await fetch(`/api/v1/sessions/${encodeURIComponent(sessionId)}/branding/logo`, { headers: { Authorization: `Bearer ${token}` }, cache: 'no-store' });
  if (!res.ok) return null;
  const type = res.headers.get('Content-Type') || '';
  if (type !== 'image/png' && type !== 'image/jpeg') return null;
  return createImageBitmap(await res.blob());
}
