// Active-speaker detection for the call UI (speaker view and the speaking
// ring). Levels come from the receiving side only (WebRTC getStats
// audioLevel of remote audio), never from recording. This module has no
// imports so the detector is unit-testable in node.

export interface ActiveSpeakerOptions {
  /** audioLevel (0..1, getStats scale) above which someone counts as talking. */
  threshold: number;
  /** A new speaker must stay the loudest this long before taking the stage. */
  dominanceMs: number;
  /** The current speaker keeps the stage at least this long (hysteresis). */
  holdMs: number;
}

export const DEFAULT_ACTIVE_SPEAKER: ActiveSpeakerOptions = { threshold: 0.02, dominanceMs: 400, holdMs: 1500 };

/**
 * Picks the active speaker from periodic level samples with hysteresis:
 * - the loudest participant above the threshold is the candidate;
 * - the first candidate takes the stage at once;
 * - a different candidate replaces the current speaker only after being the
 *   loudest continuously for dominanceMs AND the current speaker has held the
 *   stage for holdMs (so short interjections and blips don't flip the view);
 * - silence keeps the last speaker on stage (like Meet);
 * - a speaker who leaves (absent from the samples) is dropped.
 */
export class ActiveSpeakerDetector {
  private current: string | null = null;
  private currentSince = 0;
  private candidate: string | null = null;
  private candidateSince = 0;
  private readonly opts: ActiveSpeakerOptions;

  constructor(opts: Partial<ActiveSpeakerOptions> = {}) {
    this.opts = { ...DEFAULT_ACTIVE_SPEAKER, ...opts };
  }

  get speaker(): string | null { return this.current; }

  /** levels: participantId → audioLevel. now: monotonic ms. Returns the speaker. */
  update(levels: ReadonlyMap<string, number>, now: number): string | null {
    if (this.current !== null && !levels.has(this.current)) this.current = null;
    let loudest: string | null = null;
    let max = this.opts.threshold;
    for (const [id, level] of levels) {
      if (Number.isFinite(level) && level > max) { max = level; loudest = id; }
    }
    if (loudest === null) {
      this.candidate = null;
      return this.current;
    }
    if (this.current === null) {
      this.current = loudest;
      this.currentSince = now;
      this.candidate = null;
      return this.current;
    }
    if (loudest === this.current) {
      this.candidate = null;
      return this.current;
    }
    if (loudest !== this.candidate) {
      this.candidate = loudest;
      this.candidateSince = now;
    }
    if (now - this.candidateSince >= this.opts.dominanceMs && now - this.currentSince >= this.opts.holdMs) {
      this.current = loudest;
      this.currentSince = now;
      this.candidate = null;
    }
    return this.current;
  }

  reset(): void {
    this.current = this.candidate = null;
  }
}

/**
 * Reads remote audio levels from a PeerConnection's inbound-rtp stats and
 * maps them to participants through the remote stream that carries each
 * track (trackId → participantId, supplied by the caller from the tiles).
 */
export async function remoteAudioLevels(pc: RTCPeerConnection, trackOwners: ReadonlyMap<string, string>): Promise<Map<string, number>> {
  const levels = new Map<string, number>();
  for (const owner of trackOwners.values()) levels.set(owner, 0);
  const report = await pc.getStats();
  report.forEach(stat => {
    const s = stat as { type?: string; kind?: string; trackIdentifier?: string; audioLevel?: number };
    if (s.type !== 'inbound-rtp' || s.kind !== 'audio' || !s.trackIdentifier) return;
    const owner = trackOwners.get(s.trackIdentifier);
    if (owner && typeof s.audioLevel === 'number') levels.set(owner, Math.max(levels.get(owner) ?? 0, s.audioLevel));
  });
  return levels;
}
