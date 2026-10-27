// Livestream ingest client: MediaRecorder over a binary WebSocket to the host
// daemon, which re-encodes with ffmpeg and publishes to every RTMP(S)
// destination with its own RTMP client, reconnecting each one independently
// [SL-P3-3]. No imports, so the pure helpers are unit-testable in node.

export interface LivestreamDestination { name: string; url: string; key: string; }
export interface LivestreamStartRequest { preset: '720p30' | '1080p30'; destinations: LivestreamDestination[]; }

/** Mirrors host/internal/livestream.Status (keys are never included). */
export interface LivestreamServerStatus {
  state: 'idle' | 'waiting_ingest' | 'starting' | 'live' | 'reconnecting' | 'stopped' | 'failed';
  preset: string;
  startedAt?: string;
  bytesIn: number;
  ingestKbps: number;
  ingestConnected: boolean;
  /** Output rate from ffmpeg's total_size; 0 when unknown (tee muxer) [SL-P3-2]. */
  outputKbps?: number;
  /** What to show: output when known, else ingest [SL-P3-2]. */
  bitrateKbps?: number;
  bitrateSource?: 'output' | 'ingest' | '';
  restarts: number;
  progress: { frames: number; fps: number; bitrateKbps: number; totalSizeBytes?: number; outTimeSec: number; dupFrames: number; dropFrames: number; speed: number };
  destinations: LivestreamDestinationStatus[];
  /** Redacted health notes: encoder speed/drops, destinations reconnecting, failed or behind [SL-P3-3]. */
  warnings?: string[];
  lastError?: string;
}

export type DestinationState = 'pending' | 'connecting' | 'live' | 'reconnecting' | 'failed' | 'stopped';

/** Mirrors host/internal/livestream.DestinationStatus. `error` is redacted by the daemon. */
export interface LivestreamDestinationStatus {
  name: string;
  server: string;
  state: DestinationState;
  error?: string;
  reconnects?: number;
  attempt?: number;
  maxAttempts?: number;
  retryInSec?: number;
  kbps?: number;
  bytesOut?: number;
  backlogMs?: number;
  droppedFrames?: number;
  behind?: boolean;
}

export type Congestion = 'ok' | 'congested' | 'overflow';
export type ClientPhase = 'idle' | 'starting' | 'live' | 'reconnecting' | 'stopping' | 'stopped' | 'error';

export interface LivestreamClientState {
  phase: ClientPhase;
  congestion: Congestion;
  bufferedBytes: number;
  mimeType: string | null;
  message: string;
  server: LivestreamServerStatus | null;
}

export interface PlatformPreset {
  label: string;
  /** Prefilled server URL; empty when the platform issues one per stream. */
  url: string;
  /** The platform only accepts RTMPS. */
  requireTls: boolean;
  /** The platform always issues a separate stream key. */
  keyRequired: boolean;
  /** Where to find the key (and URL). */
  hint: string;
}

// Ingest URLs, RTMPS wherever the platform supports it (sources in
// docs/LIVESTREAM.md, checked 2026-10-26).
export const PLATFORM_PRESETS = {
  youtube: { label: 'YouTube', url: 'rtmps://a.rtmps.youtube.com:443/live2', requireTls: false, keyRequired: true, hint: 'YouTube Studio → Create → Go live → Stream key.' },
  twitch: { label: 'Twitch', url: 'rtmps://ingest.global-contribute.live-video.net/app', requireTls: false, keyRequired: true, hint: 'Creator Dashboard → Settings → Stream → Primary Stream key.' },
  facebook: { label: 'Facebook', url: 'rtmps://live-api-s.facebook.com:443/rtmp/', requireTls: true, keyRequired: true, hint: 'Live Producer → Streaming software. If Facebook shows a different server URL, use that one.' },
  linkedin: { label: 'LinkedIn', url: '', requireTls: true, keyRequired: true, hint: 'Live Studio → Prepare to go live → Get URL. Paste the Stream URL and the Stream key.' },
  custom: { label: 'Custom RTMP(S)', url: '', requireTls: false, keyRequired: false, hint: 'Owncast, a relay, nginx-rtmp… The key may also be the last part of the URL.' },
} as const satisfies Record<string, PlatformPreset>;
export type Platform = keyof typeof PLATFORM_PRESETS;

// The daemon transcodes anyway, so the container only has to arrive promptly.
// WebM first: Chromium's fragmented-MP4 MediaRecorder output reached ffmpeg in
// multi-second bursts (~5s extra latency, encoder speed 1.4x catch-up) in the
// e2e spike, while WebM streamed at ~1s granularity. MP4 remains the fallback
// for browsers without WebM recording (Safari).
export const MIME_PREFERENCES = [
  'video/webm;codecs=h264,opus',
  'video/webm;codecs=vp8,opus',
  'video/webm',
  'video/mp4;codecs=avc1.640028,mp4a.40.2',
  'video/mp4;codecs=avc1,mp4a',
];

export function pickMimeType(isSupported: (type: string) => boolean, preferences: string[] = MIME_PREFERENCES): string | null {
  for (const type of preferences) {
    try { if (isSupported(type)) return type; } catch { /* treat as unsupported */ }
  }
  return null;
}

/** Above `high` the uplink to the daemon is falling behind; above `max` we stop rather than buffer unboundedly. */
export const BUFFER_HIGH_BYTES = 4 * 1024 * 1024;
export const BUFFER_MAX_BYTES = 24 * 1024 * 1024;

export function congestionLevel(bufferedBytes: number, high = BUFFER_HIGH_BYTES, max = BUFFER_MAX_BYTES): Congestion {
  if (bufferedBytes >= max) return 'overflow';
  if (bufferedBytes >= high) return 'congested';
  return 'ok';
}

// Printable ASCII without spaces: what the daemon accepts in a URL or key.
const PRINTABLE = /^[\x21-\x7e]*$/;

function pathSegments(u: URL): string[] {
  return u.pathname.split('/').filter(Boolean);
}

/** Server URL validation for the form. Keys belong in the key field, not the URL. */
export function validateServerUrl(url: string, platform: Platform = 'custom'): string | null {
  const raw = url.trim();
  if (!raw && platform === 'linkedin') return 'Paste the Stream URL from LinkedIn Live Studio';
  let u: URL;
  try { u = new URL(raw); } catch { return 'Enter a full rtmp:// or rtmps:// server URL'; }
  if (u.protocol !== 'rtmp:' && u.protocol !== 'rtmps:') return 'Server URL must start with rtmp:// or rtmps://';
  if (!u.hostname) return 'Server URL needs a host';
  if (!PRINTABLE.test(raw)) return 'Server URL contains spaces or characters that are not allowed';
  if (PLATFORM_PRESETS[platform].requireTls && u.protocol !== 'rtmps:') return `${PLATFORM_PRESETS[platform].label} only accepts rtmps://`;
  if (pathSegments(u).length === 0) return 'Server URL needs an application path, e.g. rtmps://host/app';
  return null;
}

/** Validates one destination row; returns a message or null. */
export function validateDestination(platform: Platform, url: string, key: string): string | null {
  const urlError = validateServerUrl(url, platform);
  if (urlError) return urlError;
  const k = key.trim();
  if (!PRINTABLE.test(k)) return 'The stream key contains spaces or characters that are not allowed';
  if (!k && PLATFORM_PRESETS[platform].keyRequired) return `Paste the stream key from ${PLATFORM_PRESETS[platform].label}`;
  if (!k && pathSegments(new URL(url.trim())).length < 2) return 'Add the stream key, or put it at the end of the server URL';
  return null;
}

const DEST_STATE_LABEL: Record<DestinationState, string> = {
  pending: 'Waiting for video', connecting: 'Connecting', live: 'Live', reconnecting: 'Reconnecting', failed: 'Failed', stopped: 'Ended',
};

/** One line per destination for the panel: state, rate, retry and reason. */
export function destinationLine(d: LivestreamDestinationStatus): string {
  const parts: string[] = [DEST_STATE_LABEL[d.state] ?? d.state];
  if (d.state === 'live' && (d.kbps ?? 0) > 0) parts.push(formatKbps(d.kbps ?? 0));
  if (d.state === 'reconnecting') {
    if (d.attempt) parts.push(`attempt ${d.attempt}${d.maxAttempts ? ` of ${d.maxAttempts}` : ''}`);
    if ((d.retryInSec ?? 0) > 0) parts.push(`retry in ${Math.ceil(d.retryInSec ?? 0)} s`);
  }
  if (d.state === 'live' && d.behind) parts.push('skipping frames');
  else if (d.state === 'live' && (d.backlogMs ?? 0) > 1500) parts.push(`${((d.backlogMs ?? 0) / 1000).toFixed(1)} s behind`);
  if ((d.state === 'reconnecting' || d.state === 'failed') && d.error) parts.push(d.error);
  return parts.join(' · ');
}

function formatKbps(kbps: number): string {
  return kbps >= 1000 ? `${(kbps / 1000).toFixed(1)} Mbps` : `${Math.round(kbps)} kbps`;
}

export interface LivestreamClientOptions {
  sessionId: string;
  token: string;
  onState: (state: LivestreamClientState) => void;
  timesliceMs?: number;
  maxReconnects?: number;
  pollMs?: number;
}

export class LivestreamClient {
  private ws: WebSocket | null = null;
  private recorder: MediaRecorder | null = null;
  private media: MediaStream | null = null;
  private pollTimer: number | null = null;
  private reconnects = 0;
  private stopping = false;
  private state: LivestreamClientState = { phase: 'idle', congestion: 'ok', bufferedBytes: 0, mimeType: null, message: '', server: null };

  constructor(private opts: LivestreamClientOptions) {}

  private api(): string { return `/api/v1/sessions/${encodeURIComponent(this.opts.sessionId)}/livestream`; }
  private headers(): HeadersInit { return { Authorization: `Bearer ${this.opts.token}`, 'Content-Type': 'application/json' }; }
  private emit(patch: Partial<LivestreamClientState>): void { this.state = { ...this.state, ...patch }; this.opts.onState(this.state); }

  async start(req: LivestreamStartRequest, media: MediaStream): Promise<void> {
    const mimeType = pickMimeType(type => MediaRecorder.isTypeSupported(type));
    if (!mimeType) throw new Error('This browser cannot record the composited stream');
    this.media = media; this.stopping = false; this.reconnects = 0;
    this.emit({ phase: 'starting', mimeType, message: 'Starting livestream…', congestion: 'ok', bufferedBytes: 0 });
    const res = await fetch(this.api(), { method: 'POST', headers: this.headers(), body: JSON.stringify(req) });
    if (!res.ok) {
      const text = (await res.text()).trim();
      this.emit({ phase: 'error', message: text || `Host refused the livestream (${res.status})` });
      throw new Error(text || `HTTP ${res.status}`);
    }
    this.emit({ server: await res.json() as LivestreamServerStatus });
    this.connect();
    this.pollTimer = window.setInterval(() => void this.poll(), this.opts.pollMs ?? 2000);
  }

  async stop(message = 'Livestream ended'): Promise<void> {
    const failed = this.state.phase === 'error';
    this.stopping = true;
    this.emit({ phase: 'stopping', message: failed ? this.state.message : 'Ending livestream…' });
    this.teardownIngest();
    if (this.pollTimer !== null) { clearInterval(this.pollTimer); this.pollTimer = null; }
    try {
      const res = await fetch(this.api(), { method: 'DELETE', headers: this.headers() });
      if (res.ok) this.emit({ server: await res.json() as LivestreamServerStatus });
    } catch { /* host unreachable: ingest is already closed */ }
    this.emit({ phase: failed ? 'error' : 'stopped', message });
  }

  private connect(): void {
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const ws = new WebSocket(`${proto}//${location.host}${this.api()}/ingest?token=${encodeURIComponent(this.opts.token)}`);
    ws.binaryType = 'arraybuffer';
    this.ws = ws;
    ws.onopen = () => {
      if (!this.media || ws !== this.ws) return;
      const recorder = new MediaRecorder(this.media, { mimeType: this.state.mimeType ?? undefined, videoBitsPerSecond: 8_000_000, audioBitsPerSecond: 192_000 });
      recorder.ondataavailable = event => this.send(ws, event.data);
      recorder.onerror = () => void this.fail('The browser stopped recording the composited stream');
      recorder.start(this.opts.timesliceMs ?? 1000);
      this.recorder = recorder;
      this.emit({ phase: this.state.server?.state === 'live' ? 'live' : 'starting', message: 'Sending composited stream to the host…' });
    };
    ws.onclose = () => {
      if (ws !== this.ws || this.stopping) return;
      this.stopRecorder();
      if (this.reconnects >= (this.opts.maxReconnects ?? 3)) { void this.fail('Lost the connection to the host'); return; }
      this.reconnects++;
      this.emit({ phase: 'reconnecting', message: `Reconnecting to the host (attempt ${this.reconnects})…` });
      window.setTimeout(() => { if (!this.stopping) this.connect(); }, 1000);
    };
  }

  private send(ws: WebSocket, data: Blob): void {
    if (data.size === 0 || ws.readyState !== WebSocket.OPEN) return;
    const congestion = congestionLevel(ws.bufferedAmount + data.size);
    if (congestion === 'overflow') { void this.fail("Network can't keep up: the host is not accepting the stream fast enough"); return; }
    ws.send(data);
    if (congestion !== this.state.congestion || congestion === 'congested') {
      this.emit({ congestion, bufferedBytes: ws.bufferedAmount, message: congestion === 'congested' ? "Network can't keep up: stream data is queuing in the browser" : this.state.message });
    }
  }

  private async poll(): Promise<void> {
    try {
      const res = await fetch(this.api(), { headers: this.headers() });
      if (!res.ok || this.stopping) return;
      const server = await res.json() as LivestreamServerStatus;
      const patch: Partial<LivestreamClientState> = { server, bufferedBytes: this.ws?.bufferedAmount ?? 0 };
      if (server.state === 'live' && this.state.phase !== 'live') { patch.phase = 'live'; patch.message = 'Live'; }
      if (server.state === 'stopped' || server.state === 'failed') { void this.fail(server.lastError || 'The host ended the livestream'); return; }
      this.emit(patch);
    } catch { /* transient; the ingest socket reports hard failures */ }
  }

  private async fail(message: string): Promise<void> {
    if (this.stopping) return;
    this.emit({ phase: 'error', message });
    await this.stop(message);
  }

  private stopRecorder(): void {
    const r = this.recorder;
    this.recorder = null;
    if (r && r.state !== 'inactive') { r.ondataavailable = null; try { r.stop(); } catch { /* already stopped */ } }
  }

  private teardownIngest(): void {
    this.stopRecorder();
    const ws = this.ws;
    this.ws = null;
    if (ws && ws.readyState <= WebSocket.OPEN) ws.close(1000, 'livestream ended');
  }
}

/**
 * The bitrate line for the panel [SL-P3-2]: the daemon's measured encoder
 * output rate, or the ingest rate before there is output, and which one it
 * is. Older daemons only send ingestKbps.
 */
export function bitrateLabel(s: Pick<LivestreamServerStatus, 'ingestKbps' | 'bitrateKbps' | 'bitrateSource'> | null | undefined): { value: string; source: string } {
  if (!s) return { value: '—', source: '' };
  const kbps = s.bitrateKbps ?? s.ingestKbps ?? 0;
  const source = s.bitrateSource ?? (s.ingestKbps > 0 ? 'ingest' : '');
  if (!(kbps > 0)) return { value: '—', source: '' };
  const value = formatKbps(kbps);
  return { value, source: source === 'output' ? 'output' : source === 'ingest' ? 'ingest' : '' };
}
