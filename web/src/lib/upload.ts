import { OPFSStorage } from './opfs';
import {floatWavHeader} from './wav';
import type { OPFSTrackMeta } from './opfs';
import type { ChunkItem, SidecarData } from './recorder';
import type { UploadScheduler } from './uploadpacer';

export interface UploadProgress {
  bufferedBytes: number;
  uploadedBytes: number;
  lastSeq: number;
  isComplete: boolean;
  recovering?: boolean;
  error?: string;
  partialCapture?: boolean;
}
export interface TrackStatusResponse { have: Array<[number, number]> }
type Storage = Pick<OPFSStorage, 'getMeta' | 'saveMeta' | 'saveChunk' | 'getChunk' | 'listLocalSeqs'> & Partial<Pick<OPFSStorage, 'releaseUploaded'>>;
export interface UploadQueueOptions {
  storage?: Storage;
  fetch?: typeof fetch;
  sleep?: (ms: number) => Promise<void>;
  kind?: 'video' | 'audio';
  deferredWavHeader?: boolean;
  /**
   * Shared pacing across this page's queues [SL-P0-8]. Without one a queue
   * uploads one chunk at a time, as before.
   */
  scheduler?: UploadScheduler;
}

/** Per-request timeout floor, plus time for the body at UPLOAD_MIN_TIMEOUT_RATE_BPS. */
export const UPLOAD_REQUEST_TIMEOUT_MS = 20_000;
export const UPLOAD_MIN_TIMEOUT_RATE_BPS = 256_000;

/** Upload workers per queue when a scheduler is set; the scheduler gates the total. */
export const MAX_QUEUE_WORKERS = 4;

export class ProgressiveUploadQueue {
  private opfs: Storage;
  private request: typeof fetch;
  private sleep: (ms: number) => Promise<void>;
  private meta!: OPFSTrackMeta;
  private writes: Promise<void>;
  private worker: Promise<void> | null = null;
  // Unacknowledged bytes remain on disk; only sequence numbers are queued in memory.
  private queue: number[] = [];
  private uploadedBytes = 0;
  private bufferedBytes = 0;
  private lastSeqUploaded = -1;
  private failure: Error | null = null;
  private sealed = false;
  private releaseLock: () => void = () => {};
  private scheduler: UploadScheduler | null;
  // Pooled mode (scheduler set): sequence numbers being uploaded right now.
  private inflight = new Set<number>();
  private activeWorkers = 0;
  private poolError: unknown = null;
  private poolDone: () => void = () => {};

  constructor(
    private takeId: string,
    private trackId: string,
    private token: string,
    private onProgress: (p: UploadProgress) => void,
    options: UploadQueueOptions = {}
  ) {
    this.opfs = options.storage || OPFSStorage.getInstance();
    this.request = options.fetch || fetch.bind(globalThis);
    this.sleep = options.sleep || (ms => new Promise(resolve => setTimeout(resolve, ms)));
    this.scheduler = options.scheduler || null;
    this.writes = this.initialize(options.kind || 'video',!!options.deferredWavHeader);
    // Keep initialization rejection observable via ready/enqueue, without an unhandled promise.
    void this.writes.catch(error => this.fail(error));
  }

  private async initialize(kind: 'video' | 'audio',deferredWavHeader:boolean): Promise<void> {
    const existing = await this.opfs.getMeta(this.takeId, this.trackId);
    if (existing) {
      this.meta = existing;
      return;
    }
    // A new track is a live recording in this tab: claim it [SL-P0-7].
    this.releaseLock = holdTrackLock(this.takeId, this.trackId);
    this.meta = { takeId: this.takeId, trackId: this.trackId, uploadToken: this.token,
      kind, lastSeq: deferredWavHeader ? 0 : -1, headerPending:deferredWavHeader, isComplete: false, createdAt: Date.now(), chunks: {} };
    await this.opfs.saveMeta(this.takeId, this.trackId, this.meta);
  }

  public ready(): Promise<void> { return this.writes; }
  private fail(error: unknown): void {
    this.failure = error instanceof Error ? error : new Error(String(error));
    this.report();
  }
  private report(isComplete = false, recovering = false): void {
    this.scheduler?.setPending(`${this.takeId}/${this.trackId}`, isComplete ? 0 : this.bufferedBytes);
    this.onProgress({ bufferedBytes: this.bufferedBytes, uploadedBytes: this.uploadedBytes,
      lastSeq: this.lastSeqUploaded, isComplete, recovering, partialCapture:!!(this.meta?.sidecar as SidecarData)?.pcm && (this.meta?.sidecar as SidecarData)?.captureComplete===false,error: this.failure?.message });
  }
  private async hash(data: Uint8Array): Promise<string> {
    const digest = await crypto.subtle.digest('SHA-256', data as unknown as BufferSource);
    return Array.from(new Uint8Array(digest), b => b.toString(16).padStart(2, '0')).join('');
  }

  public enqueue(chunk: ChunkItem): Promise<void> {
    if (this.sealed) return Promise.reject(new Error('Track already stopped'));
    const write = this.writes.then(async () => {
      // Network failure must not stop local recording; disk failure must.
      const finalHeader = chunk.seq === 0 && this.meta.headerPending === true && this.meta.lastSeq >= 1;
      if (!finalHeader && chunk.seq !== this.meta.lastSeq + 1) throw new Error('Non-contiguous recording chunks');
      const sha256 = await this.hash(chunk.data);
      await this.opfs.saveChunk(this.takeId, this.trackId, chunk.seq, chunk.data);
      this.meta.chunks ||= {};
      this.meta.chunks[chunk.seq] = { size: chunk.data.byteLength, sha256 };
      this.meta.lastSeq = Math.max(this.meta.lastSeq,chunk.seq);
      if(finalHeader)this.meta.headerPending=false;
      if(chunk.sidecar)this.meta.sidecar=structuredClone(chunk.sidecar);
      await this.opfs.saveMeta(this.takeId, this.trackId, this.meta);
      this.queue.push(chunk.seq);
      this.bufferedBytes += chunk.data.byteLength;
      this.report();
      this.pump();
    });
    this.writes = write;
    void write.catch(error => this.fail(error));
    return write;
  }

  private pump(): void {
    if (this.failure || !this.queue.length) return;
    if (this.scheduler) { this.pumpPooled(this.scheduler); return; }
    if (this.worker) return;
    this.worker = this.drain().catch(error => this.fail(error)).finally(() => {
      this.worker = null;
      if (!this.failure && this.queue.length) this.pump();
    });
  }
  private async drain(): Promise<void> {
    while (this.queue.length) {
      const seq = this.queue[0];
      await this.uploadSeq(seq);
      this.queue.shift();
    }
  }

  /**
   * Pooled mode [SL-P0-8]: up to MAX_QUEUE_WORKERS uploads of this track at
   * once, each holding a slot of the shared scheduler. A failed chunk stays
   * queued (and on disk); this.worker settles when every worker has stopped.
   */
  private pumpPooled(scheduler: UploadScheduler): void {
    // A pool whose last worker just stopped settles asynchronously: start a
    // new one rather than adding workers the settled promise won't wait for.
    if (this.poolError) return; // the settling pool reports it as this.failure
    if (!this.worker || this.activeWorkers === 0) {
      const pool: Promise<void> = new Promise<void>(resolve => { this.poolDone = resolve; }).then(() => {
        if (this.worker !== pool) return;
        this.worker = null;
        if (this.poolError) { const error = this.poolError; this.poolError = null; this.fail(error); }
        else if (!this.failure && this.queue.length) this.pump();
      });
      this.worker = pool;
    }
    const unassigned = this.queue.filter(seq => !this.inflight.has(seq)).length;
    while (this.activeWorkers < MAX_QUEUE_WORKERS && this.activeWorkers < unassigned) {
      this.activeWorkers++;
      void this.poolWorker(scheduler).finally(() => { if (--this.activeWorkers === 0) this.poolDone(); });
    }
  }
  private async poolWorker(scheduler: UploadScheduler): Promise<void> {
    while (!this.failure && !this.poolError) {
      const seq = this.queue.find(q => !this.inflight.has(q));
      if (seq === undefined) return;
      this.inflight.add(seq);
      try {
        const release = await scheduler.acquire(this.meta.chunks?.[seq]?.size ?? 0);
        try {
          if (this.failure || this.poolError) return;
          const bytes = await this.uploadSeq(seq);
          scheduler.recordAcked(bytes);
        } finally { release(); }
        const at = this.queue.indexOf(seq);
        if (at >= 0) this.queue.splice(at, 1);
      } catch (error) {
        this.poolError ??= error;
        return;
      } finally { this.inflight.delete(seq); }
    }
  }

  /** Reads, verifies against the journal and uploads one chunk; returns its size. */
  private async uploadSeq(seq: number): Promise<number> {
    const bytes = await this.opfs.getChunk(this.takeId, this.trackId, seq);
    if (!bytes) throw new Error(`Local fragment ${seq} is missing`);
    const hash = await this.hash(bytes);
    const journal = this.meta.chunks?.[seq];
    if (journal && (journal.sha256 !== hash || journal.size !== bytes.byteLength)) throw new Error(`Local fragment ${seq} is corrupt`);
    await this.withRetry(`${this.base()}/chunks/${seq}`, {
      method: 'PUT', headers: { Authorization: `Bearer ${this.token}`, 'X-Chunk-SHA256': hash,
        'Content-Type': 'application/octet-stream' }, body: bytes as unknown as BodyInit,
    }, bytes.byteLength);
    this.uploadedBytes += bytes.byteLength;
    this.bufferedBytes -= bytes.byteLength;
    this.lastSeqUploaded = Math.max(this.lastSeqUploaded, seq);
    // Memory-only storage (private browsing) frees acknowledged fragments [SL-P1-12].
    await this.opfs.releaseUploaded?.(this.takeId, this.trackId, seq);
    this.report();
    return bytes.byteLength;
  }
  private base(): string { return `/api/v1/takes/${encodeURIComponent(this.takeId)}/tracks/${encodeURIComponent(this.trackId)}`; }
  private async withRetry(url: string, init: RequestInit, bodyBytes = 0): Promise<Response> {
    // A stalled request is bounded, but a big chunk on a slow uplink (the call
    // takes most of it) must not time out: allow for 256 kbit/s [SL-P0-8].
    const timeoutMs = UPLOAD_REQUEST_TIMEOUT_MS + Math.ceil((bodyBytes * 8) / UPLOAD_MIN_TIMEOUT_RATE_BPS) * 1000;
    let lastError: unknown;
    for (let attempt = 0; attempt < 5; attempt++) {
      try {
        // Bound a stalled request as well as the number of failed requests.
        const response = await this.request(url, { ...init, signal: AbortSignal.timeout(timeoutMs) });
        if (response.ok) return response;
        const error = new Error(`Upload request failed (HTTP ${response.status})`);
        if (response.status !== 408 && response.status !== 429 && response.status < 500) {
          // A host limit (413: fragment too big, track over its storage
          // quota) is final: say why, so the user knows the recording is
          // still safe on this device rather than seeing only a code.
          const reason = (await response.text().catch(() => '')).trim().slice(0, 200);
          throw new TerminalUploadError(reason ? `${error.message}: ${reason}` : error.message);
        }
        lastError = error;
      } catch (error) {
        if (error instanceof TerminalUploadError) throw error;
        lastError = error;
      }
      if (attempt < 4) await this.sleep(Math.min(1000 * 2 ** attempt, 8000) + Math.random() * 250);
    }
    throw lastError instanceof Error ? lastError : new Error('Upload retries exhausted');
  }

  public async prepareRecovery():Promise<{lastSeq:number;sidecar:SidecarData}> {
    await this.writes;
    const sidecar=structuredClone(this.meta.sidecar) as SidecarData | undefined;
    if(!sidecar)throw new Error('Recording has no timing metadata for recovery');
    // A compressed capture interrupted mid-take (tab closed or crashed) keeps
    // its committed fragments; it is marked partial, never silently complete.
    if(!sidecar.pcm&&!sidecar.events?.some(e=>e.type==='recorder-stop')&&sidecar.captureComplete!==false){
      sidecar.captureComplete=false;
      sidecar.events=[...(sidecar.events||[]),{type:'capture-interrupted',perfTime:performance.now(),mediaTimeUs:0}];
    }
    if(this.meta.headerPending){
      const pcm=sidecar.pcm;
      if(!pcm||pcm.format!=='float32le'||this.meta.lastSeq<1)throw new Error('PCM capture has no committed sample prefix');
      const seqs=await this.opfs.listLocalSeqs(this.takeId,this.trackId);
      if(seqs.some(seq=>seq>this.meta.lastSeq))throw new Error('Uncommitted PCM fragments require review');
      let dataBytes=0;
      for(let seq=1;seq<=this.meta.lastSeq;seq++){
        const bytes=await this.opfs.getChunk(this.takeId,this.trackId,seq),journal=this.meta.chunks?.[seq];
        if(!bytes||!journal||journal.size!==bytes.byteLength||journal.sha256!==await this.hash(bytes))throw new Error(`PCM fragment ${seq} is missing or corrupt`);
        dataBytes+=bytes.byteLength;
      }
      const frames=dataBytes/(pcm.channels*4);
      if(!Number.isSafeInteger(frames)||frames!==pcm.frames)throw new Error('PCM prefix journal disagrees with committed samples');
      sidecar.captureComplete=false;
      sidecar.events.push({type:'pcm-prefix-recovered',perfTime:performance.now(),mediaTimeUs:Math.round(frames/pcm.sampleRate*1e6)});
      await this.enqueue({seq:0,data:floatWavHeader(pcm.sampleRate,pcm.channels,frames),perfTimeMs:performance.now(),sidecar});
      await this.waitForUploads();
    }
    return {lastSeq:this.meta.lastSeq,sidecar};
  }

  public async waitForUploads(): Promise<void> {
    await this.writes;
    this.pump();
    while(this.worker)await this.worker;
    if(this.failure)throw this.failure;
  }

  public async syncWithHostStatus(): Promise<{ missing: number[]; totalServerChunks: number }> {
    await this.writes;
    if (this.worker || this.queue.length) throw new Error('Cannot reconcile an active upload queue');
    const response = await this.withRetry(`${this.base()}/status`, { headers: { Authorization: `Bearer ${this.token}` } });
    const data: TrackStatusResponse = await response.json();
    if (!Array.isArray(data.have) || data.have.some(range => !Array.isArray(range) || range.length !== 2 ||
      !Number.isSafeInteger(range[0]) || !Number.isSafeInteger(range[1]) || range[0] < 0 || range[1] < range[0])) {
      throw new Error('Invalid host fragment ranges');
    }
    const seqs = await this.opfs.listLocalSeqs(this.takeId, this.trackId);
    const missing: number[] = [];
    this.bufferedBytes = 0; this.uploadedBytes = 0;
    for (let seq = 0; seq <= this.meta.lastSeq; seq++) {
      if (!seqs.includes(seq)) throw new Error(`Local fragment ${seq} is missing`);
      const bytes = await this.opfs.getChunk(this.takeId, this.trackId, seq);
      if (!bytes) throw new Error(`Local fragment ${seq} is unreadable`);
      if (data.have.some(([start,end]) => seq >= start && seq <= end)) {
        this.uploadedBytes += bytes.byteLength;
        this.lastSeqUploaded = Math.max(this.lastSeqUploaded, seq);
      } else { missing.push(seq); this.bufferedBytes += bytes.byteLength; }
    }
    // A closed fragment whose metadata was not committed is not silently finalized.
    if (seqs.some(seq => seq > this.meta.lastSeq)) throw new Error('Uncommitted local fragments require review');
    this.queue = missing.slice();
    this.failure = null;
    this.report(false, true);
    this.pump();
    return { missing, totalServerChunks: data.have.reduce((sum,[start,end]) => sum + end-start+1,0) };
  }

  public async complete(lastSeq: number, sidecar: SidecarData): Promise<boolean> {
    this.sealed = true;
    try {
      await this.writes;
      if(this.meta.headerPending)throw new Error('Final WAV header is missing');
      if (lastSeq < 0 || lastSeq !== this.meta.lastSeq) throw new Error('Recording is empty or has missing fragments');
      this.meta.sidecar = sidecar;
      this.meta.kind = sidecar.kind === 'audio' ? 'audio' : 'video';
      this.meta.uploadToken = this.token;
      await this.opfs.saveMeta(this.takeId, this.trackId, this.meta);
      this.pump();
      while (this.worker) await this.worker;
      if (this.failure || this.queue.length) return false;
      const response = await this.withRetry(`${this.base()}/complete`, {
        method: 'POST', headers: { Authorization: `Bearer ${this.token}`, 'Content-Type': 'application/json' },
        body: JSON.stringify({ lastSeq, totalBytes: this.uploadedBytes, sidecar }),
      });
      const receipt = await response.json();
      if (receipt.status !== 'assembled' || receipt.totalBytes !== this.uploadedBytes ||
        typeof receipt.sha256 !== 'string' || !/^[a-f0-9]{64}$/.test(receipt.sha256)) {
        throw new Error('Host did not return a valid assembly receipt');
      }
      this.meta.receipt = { totalBytes: receipt.totalBytes, sha256: receipt.sha256 };
      this.meta.isComplete = true;
      await this.opfs.saveMeta(this.takeId, this.trackId, this.meta);
      this.releaseLock();
      this.report(true);
      return true;
    } catch (error) { this.fail(error); return false; }
  }
}
class TerminalUploadError extends Error {}

/** Web Lock held by the tab that is recording a track, released when it closes. */
export function trackLockName(takeId: string, trackId: string): string { return `studiolink-track:${takeId}:${trackId}`; }

type LockManagerLike = { request: (name: string, options: { ifAvailable?: boolean }, cb: (lock: unknown) => Promise<unknown>) => Promise<unknown> };
function lockManager(): LockManagerLike | null {
  const locks = (globalThis.navigator as Navigator & { locks?: LockManagerLike } | undefined)?.locks;
  return locks && typeof locks.request === 'function' ? locks : null;
}

/**
 * Holds the track's lock for the life of this tab (or until released), so a
 * second tab's recovery can never finalize a track that is still recording.
 */
function holdTrackLock(takeId: string, trackId: string): () => void {
  const locks = lockManager();
  let release = () => {};
  if (!locks) return release;
  const held = new Promise<void>(resolve => { release = resolve; });
  void locks.request(trackLockName(takeId, trackId), {}, () => held).catch(() => undefined);
  return () => release();
}

export async function recoverTakeFromOPFS(meta: OPFSTrackMeta, fallbackToken: string,
  onProgress: (p: UploadProgress) => void = () => {}, scheduler?: UploadScheduler): Promise<boolean> {
  // An interrupted capture without final media metadata cannot be called complete.
  if (!meta.sidecar || meta.lastSeq < 0) return false;
  const token = meta.uploadToken || fallbackToken;
  if (!token) return false;
  const run = async (): Promise<boolean> => {
    const queue = new ProgressiveUploadQueue(meta.takeId, meta.trackId, token, onProgress, { scheduler });
    try {
      const recovery=await queue.prepareRecovery();
      await queue.syncWithHostStatus();
      return await queue.complete(recovery.lastSeq,recovery.sidecar);
    } catch (error) {
      onProgress({ bufferedBytes: 0, uploadedBytes: 0, lastSeq: -1, isComplete: false, recovering: true,
        error: error instanceof Error ? error.message : String(error) });
      return false;
    }
  };
  const locks = lockManager();
  if (!locks) return run();
  // Another open tab still recording this track holds its lock: leave it alone.
  return await locks.request(trackLockName(meta.takeId, meta.trackId), { ifAvailable: true }, async lock => {
    if (!lock) {
      onProgress({ bufferedBytes: 0, uploadedBytes: 0, lastSeq: -1, isComplete: false, recovering: true, error: 'Still recording in another tab' });
      return false;
    }
    return run();
  }) as boolean;
}
