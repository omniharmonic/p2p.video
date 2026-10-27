import type { NetQuality, NetStatsPayload } from '../lib/protocol';
import { etaRateBps, formatBacklog } from '../lib/uploadpacer';
import '../netquality.css';

// Meet-like signal bars for connection quality [SL-P0-8], on every call tile
// and in the people panel. Reports arrive as net_stats every 2 s.

const LABEL: Record<NetQuality, string> = { good: 'Good', fair: 'Fair', poor: 'Poor', unknown: 'Checking' };
const BARS: Record<NetQuality, number> = { good: 3, fair: 2, poor: 1, unknown: 0 };

/** One-line description for tooltips and screen readers. */
export function netDescription(p: NetStatsPayload | undefined): string {
  if (!p) return 'Connection: checking';
  const parts = [`Connection: ${LABEL[p.quality].toLowerCase()}`];
  if (p.path === 'relay') parts.push(`relayed${p.protocol ? ` over ${p.protocol.toUpperCase()}` : ''}`);
  else if (p.path === 'tailnet') parts.push('private network');
  if (p.rttMs) parts.push(`${p.rttMs} ms`);
  const loss = Math.max(p.outboundLossPct ?? 0, p.inboundLossPct ?? 0);
  if (loss >= 1) parts.push(`${Math.round(loss)}% loss`);
  if (p.reasons?.length && p.quality !== 'good') parts.push(p.reasons.slice(0, 2).join(', '));
  return parts.join(' · ');
}

export function signalBars(p: NetStatsPayload | undefined, className = 'net-bars'): HTMLElement {
  const el = document.createElement('span');
  el.className = className;
  for (let i = 0; i < 3; i++) el.appendChild(document.createElement('i'));
  updateBars(el, p);
  return el;
}

export function updateBars(el: HTMLElement, p: NetStatsPayload | undefined): void {
  const level = p?.quality ?? 'unknown';
  el.dataset.quality = level;
  el.dataset.capTier = String(p?.capTier ?? '');
  el.dataset.uploadMode = p?.upload?.mode ?? '';
  el.style.setProperty('--bars', String(BARS[level]));
  const text = netDescription(p);
  el.title = text;
  el.setAttribute('role', 'img');
  el.setAttribute('aria-label', text);
  el.querySelectorAll('i').forEach((bar, i) => bar.classList.toggle('on', i < BARS[level]));
}

/**
 * Keeps a .tile-net badge on each camera tile in the grid in step with the
 * latest reports. Tiles come and go with SFU renegotiation, so render() is
 * cheap and idempotent; main.ts calls it after each report and tile change.
 */
export class NetQualityView {
  private readonly stats = new Map<string, NetStatsPayload>();
  private local: NetStatsPayload | undefined;

  constructor(private readonly grid: HTMLElement, private readonly localTileId = 'local-tile') {}

  set(p: NetStatsPayload): void { if (p.participantId) { this.stats.set(p.participantId, p); this.render(); } }
  setLocal(p: NetStatsPayload): void { this.local = p; this.render(); }
  get(id: string): NetStatsPayload | undefined { return this.stats.get(id); }
  getLocal(): NetStatsPayload | undefined { return this.local; }
  clear(): void { this.stats.clear(); this.render(); }

  render(): void {
    for (const tile of this.grid.querySelectorAll<HTMLElement>('.tile[data-participant-id]')) {
      this.badge(tile, this.stats.get(tile.dataset.participantId || ''));
    }
    const local = document.getElementById(this.localTileId);
    if (local) this.badge(local, this.local);
  }

  private badge(tile: HTMLElement, p: NetStatsPayload | undefined): void {
    let el = tile.querySelector<HTMLElement>(':scope > .tile-net');
    if (!el) { el = signalBars(p, 'net-bars tile-net'); tile.appendChild(el); }
    else updateBars(el, p);
  }
}

/** People-panel chip for the host: what a participant still has to upload. */
export function backlogChip(p: NetStatsPayload | undefined): string | null {
  const u = p?.upload;
  if (!u || u.pendingBytes <= 0) return null;
  return formatBacklog(u.pendingBytes, etaRateBps(u));
}
