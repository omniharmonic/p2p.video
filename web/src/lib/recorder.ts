import type { ClockFit } from './clock';
import type { RecordingMicState } from './protocol';

export interface ChunkItem {
  seq: number;
  data: Uint8Array;
  perfTimeMs: number;
  sidecar?: SidecarData;
}

export interface SidecarData {
  trackId: string;
  kind: string;
  engine?: string;
  codec?: string;
  mimeType?: string;
  bitrate?: number;
  captureComplete?: boolean;
  replacesAudioOfTrackId?: string;
  /** gapFrames: silence standing in for capture frames dropped before delivery (track-processor engine, SL-QA-2). */
  pcm?: {format:'float32le';sampleRate:number;channels:number;frames:number;gapFrames?:number;sourceSettings?:MediaTrackSettings;timing?:string};
  clockFit: ClockFit;
  events: Array<{ type: string; perfTime: number; mediaTimeUs: number }>;
  /** Recording mic mode and the browser's measured processing [SL-P1-10]; copied into manifest.json. */
  micProcessing?: RecordingMicState;
}

/**
 * A timing-metadata checkpoint is journaled with the first fragment and then
 * every SIDECAR_CHECKPOINT_EVERY fragments, so a tab closed or crashed mid-take
 * can still be recovered and aligned from OPFS [SL-P0-7].
 */
export const SIDECAR_CHECKPOINT_EVERY = 30;

export interface RecorderOptions {
  engine?: 'webcodecs' | 'mediarecorder';
  videoBitrateBps?: number;
  videoCodec?: string;
  targetFps?: number;
  audioSampleRate?: number;
  /** How this track's microphone was processed (camera + mic recordings) [SL-P1-10]. */
  micProcessing?: RecordingMicState;
  onError?: (error: Error) => void;
}

// MediaRecorder owns container construction and keeps the source's audio with video.
// A WebCodecs implementation needs a tested streaming muxer before it can be enabled.
export class LocalTrackRecorder {
  private mediaRecorder: MediaRecorder | null = null;
  private pending = Promise.resolve();
  private failure: Error | null = null;
  private stopPromise: Promise<{ lastSeq: number; sidecar: SidecarData }> | null = null;
  private seq = 0;
  private pendingBytes = 0;
  private stopped: Promise<void> = Promise.resolve();
  private sidecar: SidecarData;

  constructor(
    public readonly trackId: string,
    public readonly kind: 'video' | 'audio',
    private onChunk: (chunk: ChunkItem) => void | Promise<void>,
    clockFit: ClockFit,
    private options: RecorderOptions = {}
  ) {
    this.sidecar = { trackId, kind, engine: 'mediarecorder', clockFit, events: [], ...(options.micProcessing ? { micProcessing: options.micProcessing } : {}) };
  }

  public static preferredMimeType(kind: 'video' | 'audio', options: RecorderOptions = {}): string {
    if (typeof MediaRecorder === 'undefined') throw new Error('MediaRecorder is unavailable');
    const candidates = kind === 'audio'
      ? ['audio/webm;codecs=opus', 'audio/mp4', 'audio/webm']
      : options.videoCodec === 'h264'
        ? ['video/mp4', 'video/webm;codecs=vp9,opus', 'video/webm']
        : ['video/webm;codecs=vp9,opus', 'video/webm', 'video/mp4'];
    const mime = candidates.find(value => MediaRecorder.isTypeSupported(value));
    if (!mime) throw new Error('No supported recording container');
    return mime;
  }

  public getEngine(): 'mediarecorder' { return 'mediarecorder'; }
  public getMimeType(): string { return this.mediaRecorder?.mimeType || ''; }

  private fail(reason: unknown): void {
    if (this.failure) return;
    this.failure = reason instanceof Error ? reason : new Error(String(reason));
    if (this.mediaRecorder?.state !== 'inactive') this.mediaRecorder?.stop();
    this.options.onError?.(this.failure);
  }

  public async start(stream: MediaStream): Promise<void> {
    if (this.mediaRecorder) throw new Error('Recorder cannot be reused');
    const mimeType = LocalTrackRecorder.preferredMimeType(this.kind, this.options);
    this.mediaRecorder = new MediaRecorder(stream, {
      mimeType,
      ...(this.kind === 'video' ? { videoBitsPerSecond: this.options.videoBitrateBps || 12_000_000 } : {}),
    });
    this.stopped = new Promise(resolve => { this.mediaRecorder!.onstop = () => resolve(); });
    this.sidecar.mimeType = this.mediaRecorder.mimeType;
    // MIME includes the actual container/codec declaration; do not claim requested codecs.
    this.sidecar.codec = this.mediaRecorder.mimeType;
    this.sidecar.bitrate = this.kind === 'video' ? this.mediaRecorder.videoBitsPerSecond : this.mediaRecorder.audioBitsPerSecond;
    this.mediaRecorder.ondataavailable = (event: BlobEvent) => {
      if (!event.data?.size || this.failure) return;
      if (this.pendingBytes + event.data.size > 64 * 1024 * 1024) {
        this.fail(new Error('Local recording buffer exceeded 64 MiB; storage cannot keep up'));
        return;
      }
      this.pendingBytes += event.data.size;
      const perfTime = performance.now();
      this.pending = this.pending.then(async () => {
        if (this.failure) return;
        const data = new Uint8Array(await event.data.arrayBuffer());
        // This is delivery time, not an observed capture sample/frame timestamp.
        this.sidecar.events.push({ type: 'dataavailable', perfTime, mediaTimeUs: Math.round(event.timecode * 1000) || 0 });
        const checkpoint = this.seq % SIDECAR_CHECKPOINT_EVERY === 0 ? structuredClone(this.sidecar) : undefined;
        await this.onChunk({ seq: this.seq, data, perfTimeMs: perfTime, ...(checkpoint ? { sidecar: checkpoint } : {}) });
        this.seq++;
      }).catch(error => this.fail(error)).finally(() => { this.pendingBytes -= event.data.size; });
    };
    this.mediaRecorder.onerror = () => this.fail(new Error('MediaRecorder failed'));
    this.mediaRecorder.start(1000);
    this.sidecar.events.push({ type: 'recorder-start', perfTime: performance.now(), mediaTimeUs: 0 });
  }

  public stop(): Promise<{ lastSeq: number; sidecar: SidecarData }> {
    if (this.stopPromise) return this.stopPromise;
    this.stopPromise = (async () => {
      const recorder = this.mediaRecorder;
      if (!recorder) throw new Error('Recorder has not started');
      if (recorder.state !== 'inactive') recorder.stop();
      await this.stopped;
      if (this.failure) throw this.failure;
      await this.pending;
      if (this.failure) throw this.failure;
      this.sidecar.events.push({ type: 'recorder-stop', perfTime: performance.now(), mediaTimeUs: 0 });
      return { lastSeq: this.seq - 1, sidecar: this.sidecar };
    })();
    return this.stopPromise;
  }
}
