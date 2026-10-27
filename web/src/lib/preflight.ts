/**
 * Pre-flight "ready to record" evaluation [SL-P0-5]. Pure functions only: the
 * browser probes live in components/Preflight.ts so these stay unit-testable.
 */
import type { StorageBackend } from './opfs';
import type { PreflightItem, PreflightPayload, PreflightStatus } from './protocol';

export const PREFLIGHT_PLANNED_MINUTES = 120;
/** MediaRecorder audio alongside the camera track (Opus/AAC, generous). */
export const PREFLIGHT_AUDIO_BPS = 256_000;
/** Recording needs this much headroom over the expected take size to be green. */
export const PREFLIGHT_STORAGE_HEADROOM = 1.25;

const GB = 1024 ** 3;
const fmtGB = (bytes: number): string => `${(bytes / GB).toFixed(bytes < 10 * GB ? 1 : 0)} GB`;
const fmtHours = (minutes: number): string => (minutes % 60 === 0 ? `${minutes / 60} h` : `${minutes} min`);

/** Bytes per second written to OPFS for one take on this device. */
export function expectedBytesPerSecond(videoBitrateBps: number, pcm?: { sampleRate: number; channels: number }): number {
  const video = Number.isFinite(videoBitrateBps) && videoBitrateBps > 0 ? videoBitrateBps : 12_000_000;
  const pcmBytes = pcm ? pcm.sampleRate * 4 * Math.max(1, pcm.channels) : 0; // Float32 WAV
  return (video + PREFLIGHT_AUDIO_BPS) / 8 + pcmBytes;
}

/**
 * Local fragments stay in OPFS until the take is complete (they are the
 * recovery journal), so the whole planned take must fit.
 */
export function evaluateStorage(est: { quota?: number; usage?: number } | null, bytesPerSecond: number, plannedMinutes = PREFLIGHT_PLANNED_MINUTES): PreflightItem {
  const label = 'Storage';
  if (!est || !Number.isFinite(est.quota) || !est.quota) return { id: 'storage', label, status: 'warn', detail: 'Browser storage estimate unavailable; capacity unknown' };
  const available = Math.max(0, est.quota - (est.usage || 0));
  const needed = bytesPerSecond * plannedMinutes * 60;
  const detail = `${fmtGB(available)} free · ${fmtHours(plannedMinutes)} take needs about ${fmtGB(needed)}`;
  if (available < needed) {
    const minutes = Math.floor(available / bytesPerSecond / 60);
    return { id: 'storage', label, status: 'fail', detail: `${detail} · room for about ${minutes} min` };
  }
  return { id: 'storage', label, status: available < needed * PREFLIGHT_STORAGE_HEADROOM ? 'warn' : 'ok', detail };
}

/**
 * backend (opfs.ts) refines the result [SL-P1-12]: 'opfs-worker' is OPFS
 * written from a worker (Safari 15.2–18), 'memory' is a private window with
 * no OPFS, where recording still works but nothing survives a closed tab.
 */
export function evaluateOPFS(available: boolean, error?: string, backend?: StorageBackend): PreflightItem {
  const label = 'Local recording storage';
  if (backend === 'memory') return { id: 'opfs', label, status: 'warn', detail: 'Private browsing: no local recording storage. Your recording uploads as it records, but closing this tab loses anything not yet uploaded. A normal (non-private) window keeps a local copy.' };
  return available
    ? { id: 'opfs', label, status: 'ok', detail: backend === 'opfs-worker' ? 'Origin private file system available (worker writes)' : 'Origin private file system available' }
    : { id: 'opfs', label, status: 'fail', detail: error ? `Unavailable: ${error}` : 'Unavailable in this browser; recording cannot start' };
}

export function evaluatePersist(persisted: boolean | null): PreflightItem {
  if (persisted === null) return { id: 'persist', label: 'Persistent storage', status: 'info', detail: 'Not supported by this browser' };
  return persisted
    ? { id: 'persist', label: 'Persistent storage', status: 'ok', detail: 'Granted: local recordings will not be evicted' }
    : { id: 'persist', label: 'Persistent storage', status: 'warn', detail: 'Not granted: the browser may evict local recordings under storage pressure' };
}

export function evaluateCamera(state: { hasTrack: boolean; live: boolean; enabled: boolean; framesAdvancing: boolean | null }): PreflightItem {
  const base = { id: 'camera', label: 'Camera' };
  if (!state.hasTrack || !state.live) return { ...base, status: 'fail', detail: 'No live camera track' };
  if (!state.enabled) return { ...base, status: 'warn', detail: 'Camera is turned off' };
  if (state.framesAdvancing === false) return { ...base, status: 'fail', detail: 'Camera is not delivering frames' };
  if (state.framesAdvancing === null) return { ...base, status: 'info', detail: 'Checking frames…' };
  return { ...base, status: 'ok', detail: 'Delivering frames' };
}

/** peakDb: loudest level seen during the sample window (dBFS), null if unmeasured. */
export function evaluateMic(state: { hasTrack: boolean; live: boolean; enabled: boolean; peakDb: number | null }): PreflightItem {
  const base = { id: 'mic', label: 'Microphone' };
  if (!state.hasTrack || !state.live) return { ...base, status: 'fail', detail: 'No live microphone track' };
  if (!state.enabled) return { ...base, status: 'warn', detail: 'Microphone is muted' };
  if (state.peakDb === null) return { ...base, status: 'info', detail: 'Level check starts after your first click' };
  if (state.peakDb < -70) return { ...base, status: 'fail', detail: 'No signal from microphone: speak, or pick another input' };
  if (state.peakDb < -50) return { ...base, status: 'warn', detail: `Very quiet (peak ${Math.round(state.peakDb)} dBFS)` };
  if (state.peakDb > -1) return { ...base, status: 'warn', detail: 'Clipping: lower input gain' };
  return { ...base, status: 'ok', detail: `Level OK (peak ${Math.round(state.peakDb)} dBFS)` };
}

export function evaluateResolution(settings: { width?: number; height?: number; frameRate?: number } | null): PreflightItem {
  const base = { id: 'resolution', label: 'Capture resolution' };
  if (!settings?.width || !settings?.height) return { ...base, status: 'info', detail: 'Camera settings unavailable' };
  const short = Math.min(settings.width, settings.height);
  const fps = settings.frameRate ? ` at ${Math.round(settings.frameRate)} fps` : '';
  const detail = `${settings.width} × ${settings.height}${fps}`;
  if (short >= 1080) return { ...base, status: 'ok', detail };
  if (short >= 720) return { ...base, status: 'warn', detail: `${detail} (below Full HD)` };
  return { ...base, status: 'fail', detail: `${detail} (below 720p)` };
}

const HEADPHONE_LABEL = /head(phone|set)|ear(phone|bud|pod)|airpods|\bbuds\b|beats|bose|jabra|plantronics|\bwh-|\bwf-|sennheiser|\bhd ?\d{3}\b|audio-technica|ath-|shure se|steelseries|hyperx|arctis/i;
const SPEAKER_LABEL = /speaker|built-in output|display audio|macbook.*(speaker|output)/i;

/**
 * Label heuristic only: browsers do not expose the transducer type. A
 * headset-looking output or input is a hint, never a guarantee.
 */
export function evaluateHeadphones(labels: { outputs: string[]; input?: string }): PreflightItem {
  const base = { id: 'headphones', label: 'Headphones' };
  const all = [...labels.outputs, labels.input ?? ''].filter(Boolean);
  const hit = all.find(l => HEADPHONE_LABEL.test(l));
  if (hit) return { ...base, status: 'ok', detail: `Looks like headphones: ${hit} (label hint)` };
  const speaker = labels.outputs.find(l => SPEAKER_LABEL.test(l));
  return { ...base, status: 'warn', detail: speaker ? `Output looks like speakers (${speaker}): use headphones to keep other voices out of your track` : 'No headphones detected (label hint only): use headphones to avoid echo' };
}

export type ConnectionPath = 'direct' | 'tailnet' | 'relay' | 'none' | 'unknown';

interface StatsLike { id: string; type: string; [k: string]: unknown }

/** True for Tailscale's CGNAT range 100.64.0.0/10 or its ULA fd7a:115c:a1e0::/48. */
export function isTailnetAddress(address: string | undefined): boolean {
  if (!address) return false;
  const v4 = /^100\.(\d+)\./.exec(address);
  if (v4) { const b = Number(v4[1]); return b >= 64 && b <= 127; }
  return /^fd7a:115c:a1e0:/i.test(address);
}

/** Classifies the selected ICE candidate pair from RTCPeerConnection.getStats(). */
export function connectionPathFromStats(entries: Iterable<StatsLike>): { path: ConnectionPath; protocol?: string; rttMs?: number } {
  const byId = new Map<string, StatsLike>();
  for (const e of entries) byId.set(e.id, e);
  let pair: StatsLike | undefined;
  for (const e of byId.values()) {
    if (e.type === 'transport' && typeof e.selectedCandidatePairId === 'string') pair = byId.get(e.selectedCandidatePairId);
  }
  if (!pair) pair = [...byId.values()].find(e => e.type === 'candidate-pair' && (e.selected === true || (e.nominated === true && e.state === 'succeeded')));
  if (!pair) return { path: 'none' };
  const local = byId.get(String(pair.localCandidateId));
  const remote = byId.get(String(pair.remoteCandidateId));
  if (!local) return { path: 'unknown' };
  const rttMs = typeof pair.currentRoundTripTime === 'number' ? Math.round(pair.currentRoundTripTime * 1000) : undefined;
  const protocol = typeof local.relayProtocol === 'string' ? `${local.relayProtocol}` : typeof local.protocol === 'string' ? local.protocol : undefined;
  if (local.candidateType === 'relay' || remote?.candidateType === 'relay') return { path: 'relay', protocol, rttMs };
  const addr = (c?: StatsLike) => (typeof c?.address === 'string' ? c.address : typeof c?.ip === 'string' ? c.ip : undefined);
  if (isTailnetAddress(addr(local)) || isTailnetAddress(addr(remote))) return { path: 'tailnet', protocol, rttMs };
  return { path: 'direct', protocol, rttMs };
}

export function evaluatePath(result: { path: ConnectionPath; protocol?: string; rttMs?: number; state?: string } | null, inCall: boolean): PreflightItem {
  const base = { id: 'path', label: 'Connection' };
  if (!inCall || !result) return { ...base, status: 'info', detail: 'Checked after you join the call' };
  const rtt = result.rttMs !== undefined ? ` · ${result.rttMs} ms RTT` : '';
  const proto = result.protocol ? result.protocol.toUpperCase() : 'UDP';
  switch (result.path) {
    case 'direct': return { ...base, status: 'ok', detail: `Direct ${proto}${rtt}` };
    case 'tailnet': return { ...base, status: 'ok', detail: `Tailnet ${proto}${rtt}` };
    case 'relay': return { ...base, status: 'warn', detail: `Relayed (TURN/${proto})${rtt}: call quality may drop; recording is unaffected` };
    case 'none':
      // Joining is not a problem; a failed or lost connection is.
      if (result.state === 'new' || result.state === 'connecting') return { ...base, status: 'info', detail: 'Connecting…' };
      return { ...base, status: 'fail', detail: `Call media not connected${result.state ? ` (${result.state})` : ''}` };
    default: return { ...base, status: 'info', detail: 'Connection path unknown' };
  }
}

export function evaluatePower(battery: { charging: boolean; level: number } | null, plannedMinutes = PREFLIGHT_PLANNED_MINUTES): PreflightItem {
  const base = { id: 'power', label: 'Power' };
  if (!battery) return { ...base, status: 'info', detail: 'Battery status unavailable: plug in for long takes' };
  const pct = Math.round(battery.level * 100);
  if (battery.charging) return { ...base, status: 'ok', detail: `Plugged in (${pct}%)` };
  // A hint, never red: the browser battery API may be absent or imprecise.
  if (battery.level < 0.2) return { ...base, status: 'warn', detail: `On battery at ${pct}%: plug in before recording` };
  return { ...base, status: 'warn', detail: `On battery at ${pct}%: plug in for a ${fmtHours(plannedMinutes)} take` };
}

const RANK: Record<PreflightStatus, number> = { info: 0, ok: 0, warn: 1, fail: 2 };

export function overallStatus(items: PreflightItem[]): 'ok' | 'warn' | 'fail' {
  const worst = items.reduce((w, it) => Math.max(w, RANK[it.status] ?? 0), 0);
  return (['ok', 'warn', 'fail'] as const)[worst];
}

/**
 * Worst checklist among the people a take records, for the host's chip. A
 * producer is never recorded [SL-P1-11], so its own red items (e.g. browser
 * storage) must not turn the room's chip red [SL-QA-1].
 */
export function remoteWorst(participants: Array<{ role?: string; preflight?: { overall?: string } }>): 'ok' | 'warn' | 'fail' {
  const worst = participants.reduce((w, p) => p.role === 'producer' ? w : Math.max(w, p.preflight?.overall === 'fail' ? 2 : p.preflight?.overall === 'warn' ? 1 : 0), 0);
  return (['ok', 'warn', 'fail'] as const)[worst];
}

export interface PreflightProblem { who: string; item: PreflightItem }

/** Red items across this device and every participant, for the record confirm. */
export function blockingProblems(local: PreflightItem[], remote: Array<{ name: string; preflight?: PreflightPayload }>): PreflightProblem[] {
  const out: PreflightProblem[] = local.filter(it => it.status === 'fail').map(item => ({ who: 'You', item }));
  for (const p of remote) for (const item of p.preflight?.items ?? []) if (item.status === 'fail') out.push({ who: p.name || 'Participant', item });
  return out;
}
