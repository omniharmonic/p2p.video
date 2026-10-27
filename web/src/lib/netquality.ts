/**
 * Call connection quality [SL-P0-8]. Pure functions over getStats() reports so
 * the policy is unit-testable; components/NetMonitor glue samples every 2 s.
 *
 * Nothing here touches recording: these numbers only steer the call sender
 * caps and the pacing of chunk uploads (the OPFS journal buffers the rest).
 */
import { connectionPathFromStats } from './preflight';
import type { NetPath, NetQuality } from './protocol';

export interface StatsLike { id: string; type: string; [k: string]: unknown }

/** Cumulative counters kept between samples to turn totals into rates. */
export interface NetCounters {
  inboundLost: number;
  inboundReceived: number;
  framesDropped: number;
  framesReceived: number;
}

export interface NetSample {
  path: NetPath;
  protocol?: string;
  rttMs?: number;
  /** Transport-wide sender bandwidth estimate (bits/s). */
  availableOutgoingBps?: number;
  /** Loss reported by the SFU for what we send (RTCP receiver reports), %. */
  outboundLossPct?: number;
  /** Loss on what we receive since the previous sample, %. */
  inboundLossPct?: number;
  /** Worst inbound jitter, ms. */
  jitterMs?: number;
  /** The camera encoder's qualityLimitationReason. */
  qualityLimitation?: string;
  /** Received video frames dropped before display since the previous sample, %. */
  framesDroppedPct?: number;
}

const num = (v: unknown): number | undefined => (typeof v === 'number' && Number.isFinite(v) ? v : undefined);
const round1 = (v: number): number => Math.round(v * 10) / 10;

/**
 * Builds one sample. screenTrackIds excludes screen-share senders, so the
 * qualityLimitationReason is the camera's (screen share is maintain-resolution
 * and reports 'bandwidth' by design).
 */
export function sampleFromStats(
  input: Iterable<StatsLike>,
  prev: NetCounters | null,
  screenTrackIds: ReadonlySet<string> = new Set(),
): { sample: NetSample; counters: NetCounters } {
  const entries = [...input];
  const byId = new Map(entries.map(e => [e.id, e] as const));
  const path = connectionPathFromStats(entries);
  const sample: NetSample = { path: path.path, protocol: path.protocol, rttMs: path.rttMs };

  // Selected pair: bandwidth estimate (and RTT when the pair has it).
  let pairId: string | undefined;
  for (const e of entries) if (e.type === 'transport' && typeof e.selectedCandidatePairId === 'string') pairId = e.selectedCandidatePairId;
  const pair = (pairId && byId.get(pairId)) || entries.find(e => e.type === 'candidate-pair' && (e.selected === true || (e.nominated === true && e.state === 'succeeded')));
  const aob = num(pair?.availableOutgoingBitrate);
  if (aob !== undefined && aob > 0) sample.availableOutgoingBps = Math.round(aob);

  const counters: NetCounters = { inboundLost: 0, inboundReceived: 0, framesDropped: 0, framesReceived: 0 };
  let outLoss: number | undefined;
  let jitter: number | undefined;
  let remoteRtt: number | undefined;
  let limitation: string | undefined;
  for (const e of entries) {
    if (e.type === 'remote-inbound-rtp') {
      const fl = num(e.fractionLost);
      if (fl !== undefined) outLoss = Math.max(outLoss ?? 0, fl * 100);
      const rtt = num(e.roundTripTime);
      if (rtt !== undefined) remoteRtt = Math.max(remoteRtt ?? 0, rtt * 1000);
    } else if (e.type === 'inbound-rtp') {
      counters.inboundLost += Math.max(0, num(e.packetsLost) ?? 0);
      counters.inboundReceived += num(e.packetsReceived) ?? 0;
      const j = num(e.jitter);
      if (j !== undefined) jitter = Math.max(jitter ?? 0, j * 1000);
      if (e.kind === 'video') {
        counters.framesDropped += num(e.framesDropped) ?? 0;
        counters.framesReceived += num(e.framesReceived) ?? num(e.framesDecoded) ?? 0;
      }
    } else if (e.type === 'outbound-rtp' && e.kind === 'video' && typeof e.qualityLimitationReason === 'string') {
      const source = typeof e.mediaSourceId === 'string' ? byId.get(e.mediaSourceId) : undefined;
      const trackId = typeof source?.trackIdentifier === 'string' ? source.trackIdentifier : undefined;
      if (trackId && screenTrackIds.has(trackId)) continue;
      // Several camera encodings (never today): the worst one wins.
      if (!limitation || limitation === 'none') limitation = e.qualityLimitationReason;
    }
  }
  if (sample.rttMs === undefined && remoteRtt !== undefined) sample.rttMs = Math.round(remoteRtt);
  if (outLoss !== undefined) sample.outboundLossPct = round1(outLoss);
  if (jitter !== undefined) sample.jitterMs = Math.round(jitter);
  if (limitation) sample.qualityLimitation = limitation;
  if (prev) {
    const lost = counters.inboundLost - prev.inboundLost;
    const received = counters.inboundReceived - prev.inboundReceived;
    if (lost >= 0 && received >= 0 && lost + received > 0) sample.inboundLossPct = round1((lost / (lost + received)) * 100);
    const dropped = counters.framesDropped - prev.framesDropped;
    const frames = counters.framesReceived - prev.framesReceived;
    if (dropped >= 0 && frames > 0) sample.framesDroppedPct = round1(Math.min(100, (dropped / frames) * 100));
  }
  return { sample, counters };
}

/** Thresholds for classifyQuality. Poor is checked before fair. */
export const NET_QUALITY_THRESHOLDS = {
  poor: { rttMs: 500, lossPct: 10, availableKbps: 200, framesDroppedPct: 30 },
  fair: { rttMs: 250, lossPct: 3, availableKbps: 500, framesDroppedPct: 10, jitterMs: 40 },
} as const;

/**
 * True when the camera is held back by the network, not by our own cap: the
 * encoder says 'bandwidth' and the estimate doesn't clear the cap (Chrome also
 * says 'bandwidth' while a deliberate maxBitrate cap is what limits it).
 */
export function isBandwidthLimited(sample: NetSample, sendCapBps?: number): boolean {
  if (sample.qualityLimitation !== 'bandwidth') return false;
  if (sample.availableOutgoingBps === undefined || !sendCapBps) return true;
  return sample.availableOutgoingBps < sendCapBps * 1.1;
}

export interface QualityVerdict { level: NetQuality; reasons: string[] }

/**
 * One sample → good/fair/poor with human reasons. connectionState lets a
 * dead or still-connecting peer be poor/unknown instead of "good".
 */
export function classifyQuality(sample: NetSample, opts: { sendCapBps?: number; connectionState?: string } = {}): QualityVerdict {
  const state = opts.connectionState;
  if (state === 'failed' || state === 'disconnected' || state === 'closed') return { level: 'poor', reasons: ['Call media disconnected'] };
  if (sample.path === 'none' || sample.path === 'unknown') {
    return state === 'connected' ? { level: 'unknown', reasons: [] } : { level: 'unknown', reasons: ['Connecting'] };
  }
  const P = NET_QUALITY_THRESHOLDS.poor, F = NET_QUALITY_THRESHOLDS.fair;
  const loss = Math.max(sample.outboundLossPct ?? 0, sample.inboundLossPct ?? 0);
  const kbps = sample.availableOutgoingBps !== undefined ? sample.availableOutgoingBps / 1000 : undefined;
  const poor: string[] = [], fair: string[] = [];
  const rtt = sample.rttMs;
  if (rtt !== undefined && rtt >= P.rttMs) poor.push(`High latency (${rtt} ms)`);
  else if (rtt !== undefined && rtt >= F.rttMs) fair.push(`Latency ${rtt} ms`);
  if (loss >= P.lossPct) poor.push(`Packet loss ${round1(loss)}%`);
  else if (loss >= F.lossPct) fair.push(`Packet loss ${round1(loss)}%`);
  if (kbps !== undefined && kbps < P.availableKbps) poor.push(`Upload ${Math.round(kbps)} kbps`);
  else if (kbps !== undefined && kbps < F.availableKbps) fair.push(`Upload ${Math.round(kbps)} kbps`);
  const dropped = sample.framesDroppedPct ?? 0;
  if (dropped >= P.framesDroppedPct) poor.push(`Dropping ${Math.round(dropped)}% of frames`);
  else if (dropped >= F.framesDroppedPct) fair.push(`Dropping ${Math.round(dropped)}% of frames`);
  if ((sample.jitterMs ?? 0) >= F.jitterMs) fair.push(`Jitter ${sample.jitterMs} ms`);
  if (isBandwidthLimited(sample, opts.sendCapBps)) fair.push('Bandwidth-limited');
  if (poor.length) return { level: 'poor', reasons: [...poor, ...fair] };
  if (fair.length) return { level: 'fair', reasons: fair };
  return { level: 'good', reasons: [] };
}

const SEVERITY: Record<NetQuality, number> = { unknown: -1, good: 0, fair: 1, poor: 2 };

/**
 * Hysteresis over classifyQuality: a worse level must hold for `worsenAfter`
 * consecutive samples (one blip never flips the bars) and a better one for
 * `improveAfter` (recovery is shown only once it is sustained). Unknown
 * passes straight through, and the first known sample is taken as is.
 */
export class QualityTracker {
  level: NetQuality = 'unknown';
  reasons: string[] = [];
  private history: QualityVerdict[] = [];

  constructor(private readonly opts: { worsenAfter?: number; improveAfter?: number } = {}) {}

  update(v: QualityVerdict): NetQuality {
    const worsenAfter = this.opts.worsenAfter ?? 2, improveAfter = this.opts.improveAfter ?? 3;
    if (v.level === 'unknown') { this.history = []; this.level = 'unknown'; this.reasons = v.reasons; return this.level; }
    this.history.push(v);
    if (this.history.length > Math.max(worsenAfter, improveAfter)) this.history.shift();
    if (this.level === 'unknown') { this.level = v.level; this.reasons = v.reasons; return this.level; }
    const cur = SEVERITY[this.level];
    const lastN = (n: number) => (this.history.length >= n ? this.history.slice(-n) : null);
    const worse = lastN(worsenAfter);
    if (worse && worse.every(h => SEVERITY[h.level] > cur)) {
      // Move to the mildest of the recent levels: sustained, not the worst blip.
      const target = worse.reduce((a, b) => (SEVERITY[b.level] < SEVERITY[a.level] ? b : a));
      this.level = target.level; this.reasons = v.reasons;
      return this.level;
    }
    const better = lastN(improveAfter);
    if (better && better.every(h => SEVERITY[h.level] < cur)) {
      const target = better.reduce((a, b) => (SEVERITY[b.level] > SEVERITY[a.level] ? b : a));
      this.level = target.level; this.reasons = v.reasons;
      return this.level;
    }
    if (SEVERITY[v.level] === cur) this.reasons = v.reasons;
    return this.level;
  }
}

/** Adaptive camera caps: state for nextBandwidthStep. */
export interface BandwidthStepState {
  step: number;
  congestedSamples: number;
  /** When the current run of uncongested samples started (ms), or null. */
  clearSinceMs: number | null;
  lastChangeMs: number;
  lastDownMs: number;
  lastUpMs: number;
  /** How long conditions must stay clear before stepping back up. */
  upHoldMs: number;
}

export const BANDWIDTH_STEP = {
  maxStep: 3,
  /** Congested samples (2 s apart) before stepping down. */
  downAfterSamples: 2,
  /** Minimum time between two changes, so the encoder settles. */
  minChangeIntervalMs: 4_000,
  /** Sustained clear conditions before stepping back up (doubles on a failed probe). */
  baseUpHoldMs: 10_000,
  maxUpHoldMs: 120_000,
  /** A step down this soon after a step up means the probe failed. */
  failedProbeWindowMs: 30_000,
  /**
   * The estimate must at least cover the current cap (when known) before
   * stepping up. Not more: an app-limited sender's estimate only grows to a
   * small margin above what it sends (measured ~1.15x in Chromium), so a
   * higher bar would never let the call recover. A failed probe is caught by
   * the step-down rule and doubles the hold instead.
   */
  upHeadroom: 1.0,
  /** Chrome's estimate ramps up after (re)connecting: ignore it this long. */
  startupGraceMs: 8_000,
  /** Outbound loss that counts as congestion even without a 'bandwidth' verdict. */
  congestionLossPct: 5,
} as const;

export function initialBandwidthStep(): BandwidthStepState {
  return { step: 0, congestedSamples: 0, clearSinceMs: null, lastChangeMs: -Infinity, lastDownMs: -Infinity, lastUpMs: -Infinity, upHoldMs: BANDWIDTH_STEP.baseUpHoldMs };
}

export interface BandwidthInput {
  nowMs: number;
  /** ms since the PeerConnection connected. */
  connectedForMs: number;
  sample: NetSample;
  /** Current camera video cap + audio cap, bits/s. */
  sendCapBps: number;
  /** Steps that still change the tier (the bottom tier can't go lower). */
  maxStep?: number;
}

/** Whether a sample shows network congestion on our uplink. */
export function isCongested(sample: NetSample, sendCapBps: number): boolean {
  return isBandwidthLimited(sample, sendCapBps) || (sample.outboundLossPct ?? 0) >= BANDWIDTH_STEP.congestionLossPct;
}

/**
 * Whether chunk uploads should back off to protect the call [SL-P0-8]: the
 * uplink is congested now, losing packets, or its estimate is below the fair
 * threshold. High latency alone (a distant guest on a fat pipe) is not.
 */
export function isUplinkStressed(sample: NetSample, sendCapBps: number): boolean {
  if (isCongested(sample, sendCapBps)) return true;
  if ((sample.outboundLossPct ?? 0) >= NET_QUALITY_THRESHOLDS.fair.lossPct) return true;
  return sample.availableOutgoingBps !== undefined && sample.availableOutgoingBps < NET_QUALITY_THRESHOLDS.fair.availableKbps * 1000;
}

/**
 * Pure reducer: steps the camera tier down after sustained congestion and
 * back up after sustained clear conditions, with hysteresis and a backoff
 * that doubles the hold after a failed upward probe (no oscillation).
 */
export function nextBandwidthStep(state: BandwidthStepState, input: BandwidthInput): BandwidthStepState {
  const S = BANDWIDTH_STEP;
  const s = { ...state };
  if (input.connectedForMs < S.startupGraceMs) { s.congestedSamples = 0; s.clearSinceMs = null; return s; }
  const now = input.nowMs;
  if (isCongested(input.sample, input.sendCapBps)) {
    s.clearSinceMs = null;
    s.congestedSamples++;
    if (s.congestedSamples >= S.downAfterSamples && s.step < Math.min(S.maxStep, input.maxStep ?? S.maxStep) && now - s.lastChangeMs >= S.minChangeIntervalMs) {
      if (now - s.lastUpMs <= S.failedProbeWindowMs) s.upHoldMs = Math.min(S.maxUpHoldMs, s.upHoldMs * 2);
      s.step++; s.congestedSamples = 0; s.lastChangeMs = s.lastDownMs = now;
    }
    return s;
  }
  s.congestedSamples = 0;
  if (s.clearSinceMs === null) s.clearSinceMs = now;
  const aob = input.sample.availableOutgoingBps;
  const headroom = aob === undefined || aob >= input.sendCapBps * S.upHeadroom;
  if (s.step > 0 && now - s.clearSinceMs >= s.upHoldMs && now - s.lastChangeMs >= S.minChangeIntervalMs && headroom) {
    s.step--; s.clearSinceMs = now; s.lastChangeMs = s.lastUpMs = now;
    // Long stable since the last step down: forget earlier failed probes.
    if (now - s.lastDownMs >= S.maxUpHoldMs) s.upHoldMs = S.baseUpHoldMs;
  }
  return s;
}
