/**
 * Browser glue for connection quality [SL-P0-8]: every 2 s reads the call
 * PeerConnection's getStats(), classifies quality (netquality.ts), steps the
 * camera caps (rtc.ts tiers) and re-paces uploads (uploadpacer.ts), then
 * hands a net_stats report to the caller to publish. Recording is never read
 * or touched: only call senders and the upload scheduler react.
 */
import {
  classifyQuality, initialBandwidthStep, isUplinkStressed, nextBandwidthStep, QualityTracker, sampleFromStats,
  type BandwidthStepState, type NetCounters, type NetSample, type StatsLike,
} from './netquality';
import { NetStatsIntervalMs, type NetQuality, type NetStatsPayload } from './protocol';
import { CALL_TIERS, type CallEncodingPolicy, type CallNetworkInput } from './rtc';
import { uploadPolicy, type UploadScheduler } from './uploadpacer';

export interface NetMonitorHost {
  peer(): RTCPeerConnection | null;
  callPolicy(): CallEncodingPolicy & { tier: number };
  setNetworkAdaptation(net: CallNetworkInput): void;
  screenTrackIds(): ReadonlySet<string>;
  /** A take is arming/recording on this device. */
  recording(): boolean;
  scheduler: UploadScheduler;
  onReport(report: NetStatsPayload, level: NetQuality): void;
}

export class NetMonitor {
  private timer: ReturnType<typeof setInterval> | null = null;
  private tracker = new QualityTracker();
  private steps: BandwidthStepState = initialBandwidthStep();
  private counters: NetCounters | null = null;
  private pc: RTCPeerConnection | null = null;
  private connectedAt: number | null = null;
  private busy = false;
  private stressedLast = false;
  last: NetStatsPayload | null = null;

  constructor(private readonly host: NetMonitorHost, private readonly now: () => number = () => performance.now()) {}

  start(): void {
    if (this.timer !== null) return;
    this.timer = setInterval(() => void this.tick(), NetStatsIntervalMs);
  }

  stop(): void {
    if (this.timer !== null) clearInterval(this.timer);
    this.timer = null;
  }

  /** Re-evaluates now (e.g. on Stop so uploads speed up without waiting a tick). */
  async tick(): Promise<void> {
    if (this.busy) return;
    this.busy = true;
    try { await this.sample(); } catch { /* A closing peer; the next tick retries. */ } finally { this.busy = false; }
  }

  private async sample(): Promise<void> {
    const pc = this.host.peer();
    if (pc !== this.pc) {
      // New PeerConnection (join or media recovery): counters restart, and
      // Chrome's bandwidth estimate ramps again. The caps step is kept: the
      // network didn't change because we rebuilt the peer.
      this.pc = pc; this.counters = null; this.connectedAt = null;
    }
    const state = pc?.connectionState;
    if (state === 'connected' && this.connectedAt === null) this.connectedAt = this.now();
    if (state !== 'connected') this.connectedAt = null;

    let sample: NetSample = { path: 'none' };
    if (pc && state === 'connected') {
      const report = await pc.getStats();
      const entries: StatsLike[] = [];
      report.forEach(e => entries.push(e as StatsLike));
      const out = sampleFromStats(entries, this.counters, this.host.screenTrackIds());
      sample = out.sample; this.counters = out.counters;
    }
    const before = this.host.callPolicy();
    const sendCapBps = before.videoMaxBitrateBps + before.audioMaxBitrateBps;
    const verdict = classifyQuality(sample, { sendCapBps, connectionState: state ?? 'new' });
    const level = this.tracker.update(verdict);

    if (pc && state === 'connected') {
      const baseTier = before.tier - this.steps.step;
      this.steps = nextBandwidthStep(this.steps, {
        nowMs: this.now(),
        connectedForMs: this.now() - (this.connectedAt ?? this.now()),
        sample,
        sendCapBps,
        maxStep: Math.max(0, CALL_TIERS.length - 1 - baseTier),
      });
      this.host.setNetworkAdaptation({ path: sample.path, bandwidthStep: this.steps.step });
    }
    // Uploads follow the uplink itself, not the (slower) camera recovery: a
    // stressed sample throttles at once, two clean ones release it.
    const stressed = state === 'connected' && isUplinkStressed(sample, sendCapBps);
    const bandwidthLimited = stressed || this.stressedLast;
    this.stressedLast = stressed;
    this.host.scheduler.configure(uploadPolicy({
      recording: this.host.recording(), quality: level, bandwidthLimited, capsReduced: this.steps.step > 0, path: sample.path, availableOutgoingBps: sample.availableOutgoingBps,
    }));
    const applied = this.host.callPolicy();
    const report: NetStatsPayload = {
      quality: level,
      path: sample.path,
      capTier: applied.tier,
      sendMaxKbps: Math.round(applied.videoMaxBitrateBps / 1000),
      upload: this.host.scheduler.snapshot(),
    };
    if (sample.protocol) report.protocol = sample.protocol;
    if (sample.rttMs !== undefined) report.rttMs = sample.rttMs;
    if (sample.availableOutgoingBps !== undefined) report.availableOutgoingKbps = Math.round(sample.availableOutgoingBps / 1000);
    if (sample.outboundLossPct !== undefined) report.outboundLossPct = sample.outboundLossPct;
    if (sample.inboundLossPct !== undefined) report.inboundLossPct = sample.inboundLossPct;
    if (sample.jitterMs !== undefined) report.jitterMs = sample.jitterMs;
    if (sample.qualityLimitation) report.qualityLimitation = sample.qualityLimitation;
    if (sample.framesDroppedPct !== undefined) report.framesDroppedPct = sample.framesDroppedPct;
    const reasons = [...this.tracker.reasons];
    if (sample.path === 'relay') reasons.push('Relayed connection');
    if (reasons.length) report.reasons = reasons.slice(0, 4);
    this.last = report;
    this.host.onReport(report, level);
  }
}
