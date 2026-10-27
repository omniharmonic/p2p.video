import type { ICEServer } from './protocol';

/**
 * Call-path sender limits. Recording uses the full-resolution capture
 * (ADR-006: one capture); only what we send to the SFU is downscaled, so the
 * call never competes with chunk uploads for the uplink.
 */
export interface CallEncodingPolicy {
  /** Short side of the sent video, in pixels (720 for 1280×720 or 720×1280). */
  maxShortSide: number;
  videoMaxBitrateBps: number;
  videoMaxFramerate: number;
  audioMaxBitrateBps: number;
  /** What the encoder gives up first under congestion. Default: maintain-framerate. */
  degradationPreference?: 'maintain-framerate' | 'maintain-resolution';
}

export const CALL_AUDIO_MAX_BITRATE_BPS = 64_000;
/** Shared tab/system audio is often music or video; give it more than speech. */
export const SCREEN_AUDIO_MAX_BITRATE_BPS = 96_000;
/** A PeerConnection that stays `disconnected` this long is treated as failed. */
export const MEDIA_DISCONNECT_GRACE_MS = 5_000;
/** A client renegotiation offer unanswered this long triggers media recovery. */
export const RENEGOTIATION_TIMEOUT_MS = 10_000;

/**
 * Screen-share sender limits (participantCount includes us). Text must stay
 * legible, so resolution is kept (maintain-resolution, contentHint "detail")
 * and frame rate/bitrate give way instead. The capture used by the local
 * screen recorder is never touched.
 */
export function screenEncodingPolicy(participantCount: number): CallEncodingPolicy {
  const count = Number.isFinite(participantCount) ? Math.max(1, Math.floor(participantCount)) : 1;
  const base = { audioMaxBitrateBps: SCREEN_AUDIO_MAX_BITRATE_BPS, degradationPreference: 'maintain-resolution' as const };
  if (count <= 4) return { ...base, maxShortSide: 1080, videoMaxBitrateBps: 2_500_000, videoMaxFramerate: 15 };
  if (count <= 8) return { ...base, maxShortSide: 1080, videoMaxBitrateBps: 1_500_000, videoMaxFramerate: 10 };
  return { ...base, maxShortSide: 720, videoMaxBitrateBps: 1_000_000, videoMaxFramerate: 8 };
}

/**
 * Camera sender tiers, best first [SL-P0-8]. Participant count picks the base
 * tier (1–2 → 0, 3–4 → 1, 5+ → 2); a relayed path and measured congestion
 * each move further down. Only the call sender is capped, never the capture.
 */
export const CALL_TIERS: ReadonlyArray<Omit<CallEncodingPolicy, 'audioMaxBitrateBps'>> = [
  { maxShortSide: 720, videoMaxBitrateBps: 1_500_000, videoMaxFramerate: 30 },
  { maxShortSide: 540, videoMaxBitrateBps: 900_000, videoMaxFramerate: 30 },
  { maxShortSide: 360, videoMaxBitrateBps: 500_000, videoMaxFramerate: 24 },
  { maxShortSide: 270, videoMaxBitrateBps: 300_000, videoMaxFramerate: 20 },
  { maxShortSide: 180, videoMaxBitrateBps: 150_000, videoMaxFramerate: 15 },
];
export const MAX_CALL_TIER = CALL_TIERS.length - 1;
/** Under heavy congestion speech still has to get through. */
export const CALL_AUDIO_MIN_BITRATE_BPS = 32_000;

/** Network inputs to the camera policy (from NetMonitor, every 2 s). */
export interface CallNetworkInput {
  /** Selected ICE path; 'relay' (TURN, usually TCP through Funnel) caps one tier lower. */
  path?: string;
  /** Extra tiers down from measured congestion (nextBandwidthStep in netquality.ts). */
  bandwidthStep?: number;
}

/** Index into CALL_TIERS. participantCount includes the local participant. */
export function callTierIndex(participantCount: number, net: CallNetworkInput = {}): number {
  const count = Number.isFinite(participantCount) ? Math.max(1, Math.floor(participantCount)) : 1;
  const base = count <= 2 ? 0 : count <= 4 ? 1 : 2;
  const relay = net.path === 'relay' ? 1 : 0;
  const step = Number.isFinite(net.bandwidthStep) ? Math.max(0, Math.floor(net.bandwidthStep as number)) : 0;
  return Math.min(MAX_CALL_TIER, base + relay + step);
}

/** participantCount includes the local participant. */
export function callEncodingPolicy(participantCount: number, net: CallNetworkInput = {}): CallEncodingPolicy {
  const tier = callTierIndex(participantCount, net);
  // The bottom tier means the uplink is barely coping: give Opus less too.
  const audio = tier >= MAX_CALL_TIER ? CALL_AUDIO_MIN_BITRATE_BPS : CALL_AUDIO_MAX_BITRATE_BPS;
  return { ...CALL_TIERS[tier], audioMaxBitrateBps: audio };
}

/** scaleResolutionDownBy that brings the capture's short side down to maxShortSide (never upscales). */
export function scaleDownFactor(width: number | undefined, height: number | undefined, maxShortSide: number): number {
  const shortSide = Math.min(width || 0, height || 0);
  if (!shortSide || !maxShortSide || shortSide <= maxShortSide) return 1;
  return Math.round((shortSide / maxShortSide) * 1000) / 1000;
}

type SendParameters = RTCRtpSendParameters & { degradationPreference?: string };

/**
 * Pure: the codec list with VP8 first, otherwise in the browser's order [SL-P1-12].
 * The SFU forwards each publisher's RTP as is, so every publisher must send
 * a codec every subscriber decodes. VP8 is the one all three engines do:
 * Safari offers H.264 High (640c1f) first, which Firefox's OpenH264 (Constrained
 * Baseline only) can't decode, and Firefox builds without OpenH264 have no
 * H.264 at all.
 */
export function universalVideoFirst<T extends { mimeType: string }>(codecs: readonly T[]): T[] {
  const rank = (c: T) => (/^video\/vp8$/i.test(c.mimeType) ? 0 : 1);
  return [...codecs].sort((a, b) => rank(a) - rank(b));
}

/** Applies universalVideoFirst to every video transceiver (where setCodecPreferences exists). */
export function preferUniversalVideo(pc: RTCPeerConnection): void {
  const caps = typeof RTCRtpReceiver !== 'undefined' && typeof RTCRtpReceiver.getCapabilities === 'function' ? RTCRtpReceiver.getCapabilities('video')?.codecs : undefined;
  if (!caps?.some(c => /^video\/vp8$/i.test(c.mimeType)) || typeof pc.getTransceivers !== 'function') return;
  const ordered = universalVideoFirst(caps);
  for (const t of pc.getTransceivers()) {
    const kind = t.sender.track?.kind ?? t.receiver.track?.kind;
    if (kind !== 'video' || typeof t.setCodecPreferences !== 'function' || t.currentDirection === 'stopped') continue;
    try { t.setCodecPreferences(ordered); } catch { /* Engine refuses this list; keep its default order. */ }
  }
}

/**
 * Rejection for an SDP/ICE/sender operation whose PeerConnection was closed or
 * replaced while it was in flight [SL-P0-7]. Chromium leaves such promises
 * pending forever (closing a peer mid-setRemoteDescription/createAnswer), which
 * used to wedge the client's serialized signaling chain after a daemon restart:
 * admission and offers on the new socket were never processed.
 */
export class PeerClosedError extends Error {
  constructor() { super('PeerConnection closed during operation'); this.name = 'PeerClosedError'; }
}

/**
 * Pure: returns the sender parameters for the call policy. Encodings are
 * edited in place (their count must not change across setParameters).
 */
export function applyCallParameters(
  params: SendParameters,
  kind: string,
  policy: CallEncodingPolicy,
  capture?: { width?: number; height?: number },
): SendParameters {
  const encodings = params.encodings ?? [];
  for (const encoding of encodings) {
    if (kind === 'video') {
      encoding.maxBitrate = policy.videoMaxBitrateBps;
      encoding.maxFramerate = policy.videoMaxFramerate;
      encoding.scaleResolutionDownBy = scaleDownFactor(capture?.width, capture?.height, policy.maxShortSide);
    } else if (kind === 'audio') {
      encoding.maxBitrate = policy.audioMaxBitrateBps;
    }
  }
  // Camera: keep motion smooth and let the encoder drop resolution under
  // congestion instead of frame rate. Screen: the opposite (policy says so).
  if (kind === 'video') params.degradationPreference = policy.degradationPreference ?? 'maintain-framerate';
  return params;
}

export class WebRTCClient {
  public pc: RTCPeerConnection | null = null;
  private onRemoteTrack: (track: MediaStreamTrack, stream: MediaStream) => void;
  public onIceCandidate?: (candidate: RTCIceCandidateInit) => void;
  public onConnectionStateChange?: (state: RTCPeerConnectionState) => void;
  /** Fired once per PeerConnection when it fails or stays disconnected past the grace period. */
  public onMediaFailure?: () => void;
  private pendingCandidates: RTCIceCandidateInit[] = [];
  private disconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private failureReported = false;
  private participantCount = 1;
  private network: CallNetworkInput = {};
  private policyChain: Promise<void> = Promise.resolve();
  private closeSignals = new WeakMap<RTCPeerConnection, { closed: Promise<never>; abort: () => void }>();
  /** Sends a client renegotiation offer (sdp_offer with renegotiate: true). */
  public onRenegotiationOffer?: (sdp: string) => void;
  // Screen share tracks on this PeerConnection and their senders.
  private screenTracks = new Set<MediaStreamTrack>();
  private screenSenders: RTCRtpSender[] = [];
  // Client-initiated renegotiation (perfect negotiation, client = polite peer):
  // wanted = a change is waiting to be offered; pending = our offer is out.
  private renegotiationWanted = false;
  private renegotiationPending = false;
  private renegotiationTimer: ReturnType<typeof setTimeout> | null = null;

  constructor(
    onRemoteTrack: (track: MediaStreamTrack, stream: MediaStream) => void,
    onIceCandidate?: (candidate: RTCIceCandidateInit) => void
  ) {
    this.onRemoteTrack = onRemoteTrack;
    this.onIceCandidate = onIceCandidate;
  }

  public init(iceServers: ICEServer[] = []): void {
    this.close();
    const config: RTCConfiguration = {
      iceServers: iceServers.map((s) => ({
        urls: s.urls,
        username: s.username,
        credential: s.credential,
      })),
      iceCandidatePoolSize: 2,
    };

    this.pc = new RTCPeerConnection(config);
    const pc = this.pc;
    let abort!: () => void;
    const closed = new Promise<never>((_, reject) => { abort = () => reject(new PeerClosedError()); });
    closed.catch(() => { /* Observed through guard(). */ });
    this.closeSignals.set(pc, { closed, abort });
    pc.addTransceiver('audio', { direction: 'recvonly' });
    pc.addTransceiver('video', { direction: 'recvonly' });
    this.pendingCandidates = [];
    this.failureReported = false;
    this.screenTracks.clear();
    this.screenSenders = [];
    this.resetRenegotiation();

    this.pc.ontrack = (event) => {
      if (this.pc === pc) {
        const stream = event.streams[0] ?? new MediaStream([event.track]);
        this.onRemoteTrack(event.track, stream);
      }
    };

    this.pc.onicecandidate = (event) => {
      if (this.pc === pc && event.candidate && this.onIceCandidate) {
        this.onIceCandidate(event.candidate.toJSON());
      }
    };

    this.pc.onconnectionstatechange = () => {
      if (this.pc !== pc) return;
      const state = pc.connectionState;
      this.watchConnection(pc, state);
      this.onConnectionStateChange?.(state);
    };
  }

  private watchConnection(pc: RTCPeerConnection, state: RTCPeerConnectionState): void {
    if (state === 'disconnected') {
      if (this.disconnectTimer === null) {
        this.disconnectTimer = setTimeout(() => {
          this.disconnectTimer = null;
          if (this.pc === pc && (pc.connectionState === 'disconnected' || pc.connectionState === 'failed')) this.reportFailure();
        }, MEDIA_DISCONNECT_GRACE_MS);
      }
      return;
    }
    this.clearDisconnectTimer();
    if (state === 'failed') this.reportFailure();
  }

  /** Settles with op, or rejects with PeerClosedError once pc is closed by us. */
  private guard<T>(pc: RTCPeerConnection, op: Promise<T>): Promise<T> {
    const signal = this.closeSignals.get(pc);
    if (!signal) return op;
    if (this.pc !== pc) return Promise.reject(new PeerClosedError());
    return Promise.race([op, signal.closed]);
  }

  private reportFailure(): void {
    if (this.failureReported) return;
    this.failureReported = true;
    this.onMediaFailure?.();
  }

  private clearDisconnectTimer(): void {
    if (this.disconnectTimer !== null) {
      clearTimeout(this.disconnectTimer);
      this.disconnectTimer = null;
    }
  }

  public addTracks(stream: MediaStream): void {
    if (!this.pc) return;
    stream.getTracks().forEach((track) => {
      this.pc?.addTrack(track, stream);
    });
    void this.applyCallPolicy();
  }

  /**
   * Publishes a screen share on the existing PeerConnection. With
   * negotiate=false the caller is about to send a full offer anyway (join or
   * media recovery); otherwise a client renegotiation is requested.
   */
  public addScreen(stream: MediaStream, negotiate = true): void {
    const pc = this.pc;
    if (!pc) return;
    for (const track of stream.getTracks()) {
      if (this.screenTracks.has(track)) continue;
      this.screenTracks.add(track);
      // Always a fresh transceiver: addTrack would reuse an idle transceiver
      // the SFU created for one of our subscriptions, and the SFU does not
      // receive on m-lines it offered. sendrecv (like the camera's) lets the
      // SFU later reuse this m-line for a new subscription, as it does today.
      this.screenSenders.push(pc.addTransceiver(track, { direction: 'sendrecv', streams: [stream] }).sender);
    }
    if (negotiate) this.requestRenegotiation();
    void this.applyCallPolicy();
  }

  /** Withdraws the screen share's senders and renegotiates. */
  public removeScreen(): void {
    const pc = this.pc;
    const senders = this.screenSenders;
    this.screenSenders = [];
    this.screenTracks.clear();
    if (!pc || senders.length === 0) return;
    // removeTrack makes the m-line recvonly; the SFU then stops the matching
    // receiver and unpublishes the forwarded track.
    for (const sender of senders) {
      try { pc.removeTrack(sender); } catch { /* Already gone with a closed peer. */ }
    }
    this.requestRenegotiation();
  }

  public get hasScreen(): boolean { return this.screenSenders.length > 0; }

  /** MediaStreamTrack IDs of the published screen share (for getStats() attribution). */
  public get screenTrackIds(): Set<string> { return new Set([...this.screenTracks].map(t => t.id)); }

  /**
   * Swaps a published track (device switch) without renegotiation. Returns
   * false when no sender carries oldTrack.
   */
  public async replaceSenderTrack(oldTrack: MediaStreamTrack | null | undefined, newTrack: MediaStreamTrack): Promise<boolean> {
    const pc = this.pc;
    if (!pc || !oldTrack) return false;
    const sender = pc.getSenders().find(s => s.track === oldTrack);
    if (!sender) return false;
    await sender.replaceTrack(newTrack);
    void this.applyCallPolicy();
    return true;
  }

  /** Ask for a client renegotiation; coalesced and deferred until stable. */
  public requestRenegotiation(): void {
    this.renegotiationWanted = true;
    void this.maybeRenegotiate();
  }

  private resetRenegotiation(): void {
    this.renegotiationWanted = false;
    this.renegotiationPending = false;
    if (this.renegotiationTimer !== null) { clearTimeout(this.renegotiationTimer); this.renegotiationTimer = null; }
  }

  private async maybeRenegotiate(): Promise<void> {
    const pc = this.pc;
    if (!pc || !this.renegotiationWanted || this.renegotiationPending) return;
    // Before the first answer (or mid server offer) wait: handleAnswer /
    // handleOffer call back here once the connection is stable again.
    if (!pc.remoteDescription || (pc.signalingState && pc.signalingState !== 'stable')) return;
    this.renegotiationWanted = false;
    this.renegotiationPending = true;
    try {
      preferUniversalVideo(pc);
      const offer = await this.guard(pc, pc.createOffer());
      if (this.pc !== pc) return;
      await this.guard(pc, pc.setLocalDescription(offer));
    } catch {
      if (this.pc !== pc) return;
      this.renegotiationPending = false;
      this.reportFailure();
      return;
    }
    if (this.pc !== pc) return;
    this.onRenegotiationOffer?.(pc.localDescription?.sdp ?? '');
    this.renegotiationTimer = setTimeout(() => {
      this.renegotiationTimer = null;
      // No answer (lost, or rejected by the server): rebuild the media path,
      // which republishes everything including the screen.
      if (this.pc === pc && this.renegotiationPending) this.reportFailure();
    }, RENEGOTIATION_TIMEOUT_MS);
  }

  /**
   * Caps call senders for the given participant count (including us). Safe to
   * call repeatedly; only the call senders are touched, never the capture
   * tracks used by the recorders.
   */
  public setParticipantCount(count: number): Promise<void> {
    this.participantCount = count;
    return this.applyCallPolicy();
  }

  /**
   * Network adaptation [SL-P0-8]: relay path and measured congestion lower the
   * camera tier. Re-applies the sender caps only when the tier changes.
   */
  public setNetworkAdaptation(net: CallNetworkInput): Promise<void> {
    const before = callTierIndex(this.participantCount, this.network);
    this.network = { path: net.path, bandwidthStep: net.bandwidthStep };
    if (callTierIndex(this.participantCount, this.network) === before) return this.policyChain;
    return this.applyCallPolicy();
  }

  /** The camera policy currently applied to the call sender. */
  public get callPolicy(): CallEncodingPolicy & { tier: number } {
    return { ...callEncodingPolicy(this.participantCount, this.network), tier: callTierIndex(this.participantCount, this.network) };
  }

  public applyCallPolicy(): Promise<void> {
    const run = async (): Promise<void> => {
      const pc = this.pc;
      if (!pc || typeof pc.getSenders !== 'function') return;
      const camera = callEncodingPolicy(this.participantCount, this.network);
      const screen = screenEncodingPolicy(this.participantCount);
      for (const sender of pc.getSenders()) {
        const track = sender.track;
        if (!track || this.pc !== pc) continue;
        const policy = this.screenTracks.has(track) ? screen : camera;
        const params = sender.getParameters() as SendParameters;
        // Some browsers report no encodings until negotiation; retry after the answer.
        if (!params.encodings || params.encodings.length === 0) continue;
        const settings = track.kind === 'video' ? track.getSettings() : undefined;
        applyCallParameters(params, track.kind, policy, settings);
        try {
          await this.guard(pc, sender.setParameters(params));
        } catch (err) {
          if (err instanceof PeerClosedError) return;
          // Older engines reject degradationPreference; keep the bitrate/resolution caps.
          delete params.degradationPreference;
          try { await this.guard(pc, sender.setParameters(params)); } catch { /* Sender closed or renegotiating. */ }
        }
      }
    };
    this.policyChain = this.policyChain.then(run, run);
    return this.policyChain;
  }

  public async createOffer(): Promise<string> {
    const pc = this.pc;
    if (!pc) throw new Error('PeerConnection not initialized');
    preferUniversalVideo(pc);
    const offer = await this.guard(pc, pc.createOffer());
    await this.guard(pc, pc.setLocalDescription(offer));
    return pc.localDescription?.sdp ?? '';
  }

  public async handleAnswer(answerSdp: string): Promise<void> {
    const pc = this.pc;
    if (!pc) return;
    // An answer only belongs to an outstanding local offer.
    if (pc.signalingState && pc.signalingState !== 'have-local-offer') return;
    await this.guard(pc, pc.setRemoteDescription({ type: 'answer', sdp: answerSdp }));
    if (this.pc !== pc) return;
    if (this.renegotiationPending) {
      this.renegotiationPending = false;
      if (this.renegotiationTimer !== null) { clearTimeout(this.renegotiationTimer); this.renegotiationTimer = null; }
    }
    await this.drainCandidates(pc);
    void this.applyCallPolicy();
    void this.maybeRenegotiate();
  }

  /**
   * Answers a server (SFU) subscription offer. Returns null when the offer is
   * stale: during media recovery our own offer is pending and the server is
   * replacing the peer this offer was created for.
   *
   * Glare with our own renegotiation offer (screen share): the server is the
   * impolite peer and ignores our offer, so we roll ours back, answer theirs,
   * and re-offer once stable (after the caller has sent this answer).
   */
  public async handleOffer(sdp: string): Promise<string | null> {
    const pc = this.pc;
    if (!pc) throw new Error('PeerConnection not initialized');
    if (pc.signalingState && pc.signalingState !== 'stable') {
      if (!(this.renegotiationPending && pc.signalingState === 'have-local-offer')) return null;
      await this.guard(pc, pc.setLocalDescription({ type: 'rollback' }));
      if (this.pc !== pc) return null;
      this.renegotiationPending = false;
      this.renegotiationWanted = true;
      if (this.renegotiationTimer !== null) { clearTimeout(this.renegotiationTimer); this.renegotiationTimer = null; }
    }
    await this.guard(pc, pc.setRemoteDescription({ type: 'offer', sdp }));
    await this.drainCandidates(pc);
    const answer = await this.guard(pc, pc.createAnswer());
    await this.guard(pc, pc.setLocalDescription(answer));
    // Re-offer pending changes after the caller has sent this answer.
    if (this.renegotiationWanted) setTimeout(() => void this.maybeRenegotiate(), 0);
    return pc.localDescription?.sdp ?? '';
  }

  private async drainCandidates(pc: RTCPeerConnection): Promise<void> {
    if (this.pc !== pc) return;
    const candidates = this.pendingCandidates;
    this.pendingCandidates = [];
    for (const candidate of candidates) await this.guard(pc, pc.addIceCandidate(candidate));
  }

  public async addIceCandidate(candidate: RTCIceCandidateInit): Promise<void> {
    const pc = this.pc;
    if (!pc) return;
    if (pc.remoteDescription && pc.remoteDescription.type) {
      await this.guard(pc, pc.addIceCandidate(candidate));
    } else {
      this.pendingCandidates.push(candidate);
    }
  }

  public close(): void {
    this.clearDisconnectTimer();
    this.resetRenegotiation();
    this.screenTracks.clear();
    this.screenSenders = [];
    if (this.pc) {
      this.closeSignals.get(this.pc)?.abort();
      this.pc.ontrack = null;
      this.pc.onicecandidate = null;
      this.pc.onconnectionstatechange = null;
      this.pc.close();
      this.pc = null;
      this.pendingCandidates = [];
    }
  }
}
