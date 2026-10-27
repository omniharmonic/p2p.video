/**
 * Upload pacing during a live take [SL-P0-8, ARCHITECTURE §4.4].
 *
 * Chunk uploads share the uplink with call media. While a take records they
 * run at concurrency 2 (1 when the call is relayed); when the call is
 * bandwidth-limited or poor they drop to concurrency 1 behind a token bucket
 * at a fraction of the sender bandwidth estimate. With no take recording they
 * catch up at concurrency 4. Backpressure only delays uploads: every chunk is
 * already journaled in OPFS before it is queued, so nothing is ever dropped.
 */
import type { NetPath, NetQuality, NetUploadStats, UploadMode } from './protocol';

export interface UploadPolicy { mode: Exclude<UploadMode, 'idle'>; concurrency: number; rateLimitBps: number | null }

export const UPLOAD_PACING = {
  liveConcurrency: 2,
  relayConcurrency: 1,
  drainConcurrency: 4,
  throttledConcurrency: 1,
  /** Share of the sender bandwidth estimate uploads may use while throttled. */
  fairShare: 0.25,
  poorShare: 0.15,
  /**
   * While the camera tier is still stepped down but the uplink is no longer
   * stressed (the call adapted, or is recovering): paced, with a larger share.
   * Releasing uploads to full speed here would congest the call again.
   */
  recoveringShare: 0.5,
  /** Floor so a take's backlog keeps moving even on a terrible uplink. */
  minRateBps: 256_000,
  maxRateBps: 8_000_000,
  /** Token bucket depth: one chunk-sized burst at most. */
  burstBytes: 256 * 1024,
} as const;

export interface UploadPolicyInput {
  /** A take is recording on this device (arming/recording). */
  recording: boolean;
  quality: NetQuality;
  /** The uplink is congested or lossy (isUplinkStressed, with one sample of hold). */
  bandwidthLimited: boolean;
  /** The camera sender is below its normal tier because of congestion. */
  capsReduced?: boolean;
  path?: NetPath;
  availableOutgoingBps?: number;
}

/** Pure: how uploads may use the uplink right now. */
export function uploadPolicy(i: UploadPolicyInput): UploadPolicy {
  const P = UPLOAD_PACING;
  if (!i.recording) return { mode: 'drain', concurrency: P.drainConcurrency, rateLimitBps: null };
  if (i.quality === 'poor' || i.bandwidthLimited || i.capsReduced) {
    const share = i.quality === 'poor' ? P.poorShare : i.bandwidthLimited ? P.fairShare : P.recoveringShare;
    const est = i.availableOutgoingBps ?? 0;
    const rate = Math.round(Math.min(P.maxRateBps, Math.max(P.minRateBps, est * share)));
    return { mode: 'throttled', concurrency: P.throttledConcurrency, rateLimitBps: rate };
  }
  return { mode: 'live', concurrency: i.path === 'relay' ? P.relayConcurrency : P.liveConcurrency, rateLimitBps: null };
}

/**
 * Debt-based token bucket: a chunk may start once the balance is not
 * negative, then its whole size is charged, so the long-run rate holds even
 * for chunks larger than the burst.
 */
export class TokenBucket {
  private tokens: number;
  private at: number;
  constructor(private rateBps: number, private readonly burstBytes: number, nowMs: number) {
    this.tokens = burstBytes; this.at = nowMs;
  }
  setRate(rateBps: number, nowMs: number): void { this.refill(nowMs); this.rateBps = rateBps; }
  private refill(nowMs: number): void {
    const dt = Math.max(0, nowMs - this.at) / 1000;
    this.tokens = Math.min(this.burstBytes, this.tokens + (dt * this.rateBps) / 8);
    this.at = nowMs;
  }
  /** ms until a chunk may start (0 = now). */
  waitMs(nowMs: number): number {
    this.refill(nowMs);
    if (this.tokens >= 0 || this.rateBps <= 0) return 0;
    return Math.ceil((-this.tokens * 8 * 1000) / this.rateBps);
  }
  charge(bytes: number, nowMs: number): void { this.refill(nowMs); this.tokens -= bytes; }
  get balance(): number { return this.tokens; }
}

/** Window for the acknowledged-rate estimate: long enough to span paced chunks. */
export const RATE_WINDOW_MS = 30_000;

/** Rate to estimate a backlog's ETA: measured, or the pacing limit when nothing was acked yet. */
export function etaRateBps(u: Pick<NetUploadStats, 'rateBps' | 'mode' | 'rateLimitBps'>): number {
  return u.rateBps > 0 ? u.rateBps : u.mode === 'throttled' ? u.rateLimitBps ?? 0 : 0;
}

export interface SchedulerClock { now(): number; sleep(ms: number): Promise<void> }
const realClock: SchedulerClock = { now: () => performance.now(), sleep: ms => new Promise(r => setTimeout(r, ms)) };

/**
 * Shared by every upload queue on the page: a global concurrency gate (FIFO)
 * plus the token bucket, re-configured every stats sample by NetMonitor.
 */
export class UploadScheduler {
  private policy: UploadPolicy = { mode: 'drain', concurrency: UPLOAD_PACING.drainConcurrency, rateLimitBps: null };
  private inflight = 0;
  private waiters: Array<() => void> = [];
  private bucket: TokenBucket | null = null;
  private pending = new Map<string, number>();
  private acked: Array<{ at: number; bytes: number }> = [];
  /** Highest number of uploads in flight at once since the last reset (for tests/diagnostics). */
  peakInflight = 0;

  constructor(private readonly clock: SchedulerClock = realClock) {}

  get mode(): UploadMode { return this.policy.mode === 'drain' && this.pendingBytes === 0 && this.inflight === 0 ? 'idle' : this.policy.mode; }
  get concurrency(): number { return this.policy.concurrency; }
  get rateLimitBps(): number | null { return this.policy.rateLimitBps; }
  get inFlight(): number { return this.inflight; }

  configure(policy: UploadPolicy): void {
    const now = this.clock.now();
    if (policy.rateLimitBps === null) this.bucket = null;
    else if (!this.bucket) this.bucket = new TokenBucket(policy.rateLimitBps, UPLOAD_PACING.burstBytes, now);
    else this.bucket.setRate(policy.rateLimitBps, now);
    if (policy.mode !== this.policy.mode) this.peakInflight = this.inflight;
    this.policy = { ...policy, concurrency: Math.max(1, Math.floor(policy.concurrency)) };
    this.grant();
  }

  private grant(): void {
    while (this.inflight < this.policy.concurrency && this.waiters.length) {
      this.inflight++;
      this.peakInflight = Math.max(this.peakInflight, this.inflight);
      this.waiters.shift()!();
    }
  }

  /**
   * Waits for an upload slot and for the token bucket, then returns release().
   * The pacing wait re-checks every 250 ms, so a switch to drain (Stop) speeds
   * up a throttled backlog at once.
   */
  async acquire(bytes: number): Promise<() => void> {
    await new Promise<void>(resolve => { this.waiters.push(resolve); this.grant(); });
    for (;;) {
      const bucket = this.bucket;
      if (!bucket) break;
      const wait = bucket.waitMs(this.clock.now());
      if (wait <= 0) { bucket.charge(bytes, this.clock.now()); break; }
      await this.clock.sleep(Math.min(wait, 250));
    }
    let released = false;
    return () => {
      if (released) return;
      released = true;
      this.inflight--;
      this.grant();
    };
  }

  /** An acknowledged upload, for the recent-rate estimate. */
  recordAcked(bytes: number): void {
    const now = this.clock.now();
    this.acked.push({ at: now, bytes });
    while (this.acked.length && now - this.acked[0].at > RATE_WINDOW_MS) this.acked.shift();
  }

  /** Recent acknowledged rate over RATE_WINDOW_MS, bits/s. */
  get rateBps(): number {
    const now = this.clock.now();
    while (this.acked.length && now - this.acked[0].at > RATE_WINDOW_MS) this.acked.shift();
    if (!this.acked.length) return 0;
    const bytes = this.acked.reduce((sum, a) => sum + a.bytes, 0);
    const span = Math.max(2_000, now - this.acked[0].at);
    return Math.round((bytes * 8 * 1000) / span);
  }

  /** Each queue reports its unacknowledged bytes under its own key. */
  setPending(key: string, bytes: number): void {
    if (bytes > 0) this.pending.set(key, bytes); else this.pending.delete(key);
  }
  get pendingBytes(): number { let sum = 0; for (const b of this.pending.values()) sum += b; return sum; }

  snapshot(): NetUploadStats {
    const out: NetUploadStats = { pendingBytes: this.pendingBytes, rateBps: this.rateBps, mode: this.mode, concurrency: this.policy.concurrency };
    if (this.policy.rateLimitBps !== null) out.rateLimitBps = this.policy.rateLimitBps;
    return out;
  }
}

/** "Uploading 18.0 MB · ~2 min left" (or "… · stalled" when nothing moves). */
export function formatBacklog(pendingBytes: number, rateBps: number): string {
  const mb = pendingBytes / (1024 * 1024);
  const size = mb >= 10 ? `${Math.round(mb)} MB` : `${mb.toFixed(1)} MB`;
  const sec = backlogSeconds(pendingBytes, rateBps);
  const eta = sec === null ? 'stalled' : sec < 60 ? `~${Math.max(1, sec)} s left` : `~${Math.ceil(sec / 60)} min left`;
  return `Uploading ${size} · ${eta}`;
}

/** Seconds to clear a backlog at the recent rate (null when nothing is moving). */
export function backlogSeconds(pendingBytes: number, rateBps: number): number | null {
  if (pendingBytes <= 0) return 0;
  if (!(rateBps > 0)) return null;
  return Math.ceil((pendingBytes * 8) / rateBps);
}
