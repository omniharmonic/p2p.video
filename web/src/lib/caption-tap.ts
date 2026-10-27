import { encodeCaptionFrame, isDigitalSilence } from './captions';

// Host-only live-captions audio feed [SL-P4-2]. Taps each participant's call
// audio (remote WebRTC tracks, plus the host's own mic), downsamples it to
// 16 kHz mono Int16 in an AudioWorklet and streams it to the daemon's
// host-only ingest socket, keyed by participant ID. Separate from recording:
// it never touches recorders, OPFS or uploads.

export type CaptionTapState = 'idle' | 'connecting' | 'streaming' | 'error';

interface Tap {
  track: MediaStreamTrack;
  source: MediaStreamAudioSourceNode;
  node: AudioWorkletNode;
}

export class CaptionTap {
  private ctx: AudioContext | null = null;
  private sink: GainNode | null = null;
  private ready: Promise<void> | null = null;
  private ws: WebSocket | null = null;
  private running = false;
  private retry: number | null = null;
  private taps = new Map<string, Tap>();
  /** Frames sent per participant (diagnostics and tests). */
  readonly sent = new Map<string, number>();

  constructor(private readonly opts: { url: () => string; onState?: (s: CaptionTapState) => void }) {}

  get active(): boolean { return this.running; }

  async start(): Promise<void> {
    if (this.running) return;
    this.running = true;
    this.ctx = new AudioContext();
    // A silent sink keeps the worklets pulled by the render graph.
    this.sink = this.ctx.createGain();
    this.sink.gain.value = 0;
    this.sink.connect(this.ctx.destination);
    this.ready = this.ctx.audioWorklet.addModule(new URL('./caption-worklet.js', import.meta.url).href);
    void this.ctx.resume().catch(() => undefined);
    this.connect();
    await this.ready;
  }

  stop(): void {
    this.running = false;
    if (this.retry !== null) { clearTimeout(this.retry); this.retry = null; }
    for (const id of [...this.taps.keys()]) this.untap(id);
    const ws = this.ws;
    this.ws = null;
    if (ws) { ws.onclose = null; ws.close(); }
    void this.ctx?.close().catch(() => undefined);
    this.ctx = null;
    this.sink = null;
    this.ready = null;
    this.opts.onState?.('idle');
  }

  /** Taps exactly these participants' audio tracks. */
  async setSources(sources: Map<string, MediaStreamTrack>): Promise<void> {
    if (!this.running || !this.ready) return;
    try { await this.ready; } catch { this.opts.onState?.('error'); return; }
    for (const [id, tap] of this.taps) {
      const next = sources.get(id);
      if (!next || next !== tap.track || next.readyState !== 'live') this.untap(id);
    }
    for (const [id, track] of sources) {
      if (!this.taps.has(id) && track.readyState === 'live') this.tap(id, track);
    }
  }

  private tap(id: string, track: MediaStreamTrack): void {
    const ctx = this.ctx;
    if (!ctx || !this.sink) return;
    const source = ctx.createMediaStreamSource(new MediaStream([track]));
    const node = new AudioWorkletNode(ctx, 'studiolink-caption-tap', { numberOfInputs: 1, numberOfOutputs: 1, channelCount: 2, channelCountMode: 'clamped-max' });
    node.port.onmessage = (e: MessageEvent<Int16Array>) => this.send(id, e.data);
    source.connect(node);
    node.connect(this.sink);
    this.taps.set(id, { track, source, node });
  }

  private untap(id: string): void {
    const tap = this.taps.get(id);
    if (!tap) return;
    tap.node.port.postMessage('stop');
    tap.node.port.onmessage = null;
    tap.source.disconnect();
    tap.node.disconnect();
    this.taps.delete(id);
  }

  private send(id: string, pcm: Int16Array): void {
    // Digital silence (a muted track) is not sent; the daemon restarts the
    // stream when speech resumes.
    if (this.ws?.readyState !== WebSocket.OPEN || isDigitalSilence(pcm)) return;
    // Back-pressure: drop rather than queue if the socket is congested.
    if (this.ws.bufferedAmount > 512 * 1024) return;
    this.ws.send(encodeCaptionFrame(id, pcm));
    this.sent.set(id, (this.sent.get(id) ?? 0) + 1);
  }

  private connect(): void {
    if (!this.running) return;
    this.opts.onState?.('connecting');
    const ws = new WebSocket(this.opts.url());
    ws.binaryType = 'arraybuffer';
    this.ws = ws;
    ws.onopen = () => { if (this.ws === ws) this.opts.onState?.('streaming'); };
    ws.onclose = () => {
      if (this.ws !== ws || !this.running) return;
      this.ws = null;
      this.opts.onState?.('error');
      this.retry = window.setTimeout(() => { this.retry = null; this.connect(); }, 2000);
    };
  }
}
