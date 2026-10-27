// Origin Private File System (OPFS) Storage Engine for p2p.video
// Persists raw recorded media chunks locally to survive browser crashes, tab reloads, and network drops.
//
// Backends, best first [SL-P1-12]:
//   opfs         FileSystemFileHandle.createWritable (Chromium, Firefox, Safari 26+)
//   opfs-worker  a worker's FileSystemSyncAccessHandle (Safari 15.2-18: OPFS without createWritable)
//   memory       no OPFS at all (Safari private windows reject getDirectory): fragments are
//                kept in this tab's memory until the host acknowledges them, then dropped.
//                Recording and progressive upload work; a closed or crashed tab loses what
//                was not yet uploaded, and there is nothing to recover on reload.

export interface OPFSTrackMeta {
  takeId: string;
  trackId: string;
  uploadToken: string;
  kind: 'video' | 'audio';
  lastSeq: number;
  isComplete: boolean;
  createdAt: number;
  sidecar?: unknown;
  headerPending?: boolean;
  chunks?: Record<number, { size: number; sha256: string }>;
  receipt?: { totalBytes: number; sha256: string };
}

export type StorageBackend = 'opfs' | 'opfs-worker' | 'memory';

type WriteReply = { id: number; ok: boolean; name?: string; message?: string };

export class OPFSStorage {
  private static instance: OPFSStorage;

  public static getInstance(): OPFSStorage {
    if (!OPFSStorage.instance) {
      OPFSStorage.instance = new OPFSStorage();
    }
    return OPFSStorage.instance;
  }

  private backendPromise: Promise<StorageBackend> | null = null;
  private backendError = '';
  private worker: Worker | null = null;
  private workerSeq = 0;
  private workerWaiters = new Map<number, (reply: WriteReply) => void>();
  private memory = new Map<string, Uint8Array>();
  private memoryMeta = new Map<string, OPFSTrackMeta>();

  public isSupported(): boolean {
    return typeof navigator !== 'undefined' && !!navigator.storage?.getDirectory;
  }

  /** The storage this browser gives recordings, probed once per page. */
  public backend(): Promise<StorageBackend> {
    this.backendPromise ||= this.probeBackend();
    return this.backendPromise;
  }

  /** Why OPFS is unavailable (memory backend), for diagnostics. */
  public get unavailableReason(): string { return this.backendError; }

  private async probeBackend(): Promise<StorageBackend> {
    if (!this.isSupported()) { this.backendError = 'navigator.storage.getDirectory is missing'; return 'memory'; }
    try { await navigator.storage.getDirectory(); }
    catch (error) { this.backendError = error instanceof Error ? error.message : String(error); return 'memory'; }
    // No handle class to inspect (non-browser hosts): use the handles as they come.
    if (typeof FileSystemFileHandle === 'undefined') return 'opfs';
    if (typeof (FileSystemFileHandle.prototype as { createWritable?: unknown }).createWritable === 'function') return 'opfs';
    if (typeof Worker === 'undefined') { this.backendError = 'no createWritable and no Worker'; return 'memory'; }
    try {
      await this.workerWrite(['.studiolink-probe'], 'probe.bin', new Uint8Array([1]));
      const root = await navigator.storage.getDirectory();
      await root.removeEntry('.studiolink-probe', { recursive: true }).catch(() => undefined);
      return 'opfs-worker';
    } catch (error) {
      this.worker?.terminate(); this.worker = null;
      this.backendError = error instanceof Error ? error.message : String(error);
      return 'memory';
    }
  }

  private workerWrite(path: string[], name: string, data: Uint8Array): Promise<void> {
    if (!this.worker) {
      this.worker = new Worker(new URL('./opfs-sync-worker.ts', import.meta.url), { type: 'module' });
      this.worker.onmessage = (event: MessageEvent<WriteReply>) => {
        const done = this.workerWaiters.get(event.data.id);
        this.workerWaiters.delete(event.data.id);
        done?.(event.data);
      };
      this.worker.onerror = event => {
        const waiters = [...this.workerWaiters.values()];
        this.workerWaiters.clear();
        for (const done of waiters) done({ id: -1, ok: false, name: 'Error', message: event.message || 'OPFS worker failed' });
      };
    }
    const id = ++this.workerSeq;
    return new Promise<void>((resolve, reject) => {
      this.workerWaiters.set(id, reply => reply.ok ? resolve() : reject(new DOMException(reply.message || 'OPFS write failed', reply.name || 'Error')));
      this.worker!.postMessage({ id, path, name, data });
    });
  }

  /** Writes a whole file in the track directory with whichever OPFS writer this engine has. */
  private async writeFile(takeId: string, trackId: string, name: string, data: Uint8Array): Promise<void> {
    if (await this.backend() === 'opfs-worker') { await this.workerWrite(['takes', takeId, 'tracks', trackId], name, data); return; }
    const trackDir = await this.getTrackDir(takeId, trackId, true);
    if (!trackDir) throw new Error(`OPFS unavailable: cannot write ${takeId}/${trackId}/${name}`);
    const fileHandle = await trackDir.getFileHandle(name, { create: true });
    const writable = await fileHandle.createWritable();
    await writable.write(data as unknown as BufferSource);
    await writable.close();
  }

  private memKey(takeId: string, trackId: string, seq?: number): string { return seq === undefined ? `${takeId}/${trackId}` : `${takeId}/${trackId}/${seq}`; }

  /**
   * The host has acknowledged this fragment. Only the memory backend drops it
   * (memory is the scarce resource there); OPFS keeps it as the recovery
   * journal until the take is complete.
   */
  public async releaseUploaded(takeId: string, trackId: string, seq: number): Promise<void> {
    if (await this.backend() === 'memory') this.memory.delete(this.memKey(takeId, trackId, seq));
  }

  private async getTrackDir(takeId: string, trackId: string, create = true): Promise<FileSystemDirectoryHandle | null> {
    if (!this.isSupported()) return null;
    try {
      const root = await navigator.storage.getDirectory();
      const takesDir = await root.getDirectoryHandle('takes', { create });
      const takeDir = await takesDir.getDirectoryHandle(takeId, { create });
      const tracksDir = await takeDir.getDirectoryHandle('tracks', { create });
      return await tracksDir.getDirectoryHandle(trackId, { create });
    } catch (err) {
      if (err instanceof DOMException && err.name === 'NotFoundError' && !create) return null;
      throw err;
    }
  }

  public async saveChunk(takeId: string, trackId: string, seq: number, data: Uint8Array): Promise<void> {
    const key = `${takeId}/${trackId}/${seq}`;
    if (await this.backend() === 'memory') { this.memory.set(this.memKey(takeId, trackId, seq), data.slice()); return; }
    try {
      await this.writeFile(takeId, trackId, `chunk_${seq}.bin`, data);
    } catch (err) {
      console.error(`OPFS saveChunk error (${key}):`, err);
      throw err;
    }
  }

  public async getChunk(takeId: string, trackId: string, seq: number): Promise<Uint8Array | null> {
    if (await this.backend() === 'memory') return this.memory.get(this.memKey(takeId, trackId, seq)) ?? null;
    const trackDir = await this.getTrackDir(takeId, trackId, false);
    if (!trackDir) return null;

    try {
      const fileName = `chunk_${seq}.bin`;
      const fileHandle = await trackDir.getFileHandle(fileName, { create: false });
      const file = await fileHandle.getFile();
      const buffer = await file.arrayBuffer();
      return new Uint8Array(buffer);
    } catch {
      return null;
    }
  }

  public async deleteChunk(takeId: string, trackId: string, seq: number): Promise<void> {
    if (await this.backend() === 'memory') { this.memory.delete(this.memKey(takeId, trackId, seq)); return; }
    const trackDir = await this.getTrackDir(takeId, trackId, false);
    if (!trackDir) return;
    try {
      await trackDir.removeEntry(`chunk_${seq}.bin`);
    } catch {
      // Ignored if already removed
    }
  }

  public async saveMeta(takeId: string, trackId: string, meta: OPFSTrackMeta): Promise<void> {
    const key = `${takeId}/${trackId}`;
    if (await this.backend() === 'memory') { this.memoryMeta.set(this.memKey(takeId, trackId), structuredClone(meta)); return; }
    try {
      await this.writeFile(takeId, trackId, 'meta.json', new TextEncoder().encode(JSON.stringify(meta)));
    } catch (err) {
      console.error(`OPFS saveMeta error (${key}):`, err);
      throw err;
    }
  }

  public async getMeta(takeId: string, trackId: string): Promise<OPFSTrackMeta | null> {
    if (await this.backend() === 'memory') { const meta = this.memoryMeta.get(this.memKey(takeId, trackId)); return meta ? structuredClone(meta) : null; }
    const trackDir = await this.getTrackDir(takeId, trackId, false);
    if (!trackDir) return null;

    try {
      const fileHandle = await trackDir.getFileHandle('meta.json', { create: false });
      const file = await fileHandle.getFile();
      const text = await file.text();
      return JSON.parse(text) as OPFSTrackMeta;
    } catch (error) {
      if (error instanceof DOMException && error.name === 'NotFoundError') return null;
      throw error;
    }
  }

  public async listLocalSeqs(takeId: string, trackId: string): Promise<number[]> {
    const seqSet = new Set<number>();
    if (await this.backend() === 'memory') {
      const prefix = `${this.memKey(takeId, trackId)}/`;
      for (const key of this.memory.keys()) if (key.startsWith(prefix)) seqSet.add(Number(key.slice(prefix.length)));
      return Array.from(seqSet).sort((a, b) => a - b);
    }
    const trackDir = await this.getTrackDir(takeId, trackId, false);
    if (trackDir) {
      try {
        const dirEntries = (trackDir as unknown as { entries(): AsyncIterable<[string, FileSystemHandle]> }).entries?.();
        if (dirEntries) {
          for await (const [name, handle] of dirEntries) {
            if (handle.kind === 'file' && /^chunk_[0-9]+\.bin$/.test(name)) {
              const numStr = name.replace('chunk_', '').replace('.bin', '');
              const seq = parseInt(numStr, 10);
              if (!isNaN(seq)) seqSet.add(seq);
            }
          }
        }
      } catch (err) {
        throw err;
      }
    }

    return Array.from(seqSet).sort((a, b) => a - b);
  }

  public async listUnfinishedTracks(): Promise<OPFSTrackMeta[]> {
    const results: OPFSTrackMeta[] = [];
    const seen = new Set<string>();

    if (await this.backend() === 'memory') return [...this.memoryMeta.values()].filter(meta => !meta.isComplete).map(meta => structuredClone(meta));

    try {
      const root = await navigator.storage.getDirectory();
      let takesDir: FileSystemDirectoryHandle;
      try {
        takesDir = await root.getDirectoryHandle('takes', { create: false });
      } catch {
        return results;
      }

      const takesEntries = (takesDir as unknown as { entries(): AsyncIterable<[string, FileSystemHandle]> }).entries?.();
      if (!takesEntries) return results;

      for await (const [takeName, takeHandle] of takesEntries) {
        if (takeHandle.kind !== 'directory') continue;
        const takeDir = takeHandle as FileSystemDirectoryHandle;
        let tracksDir: FileSystemDirectoryHandle;
        try {
          tracksDir = await takeDir.getDirectoryHandle('tracks', { create: false });
        } catch {
          continue;
        }

        const tracksEntries = (tracksDir as unknown as { entries(): AsyncIterable<[string, FileSystemHandle]> }).entries?.();
        if (!tracksEntries) continue;

        for await (const [trackName, trackHandle] of tracksEntries) {
          if (trackHandle.kind !== 'directory') continue;
          const trackDir = trackHandle as FileSystemDirectoryHandle;
          const key = `${takeName}/${trackName}`;
          if (seen.has(key)) continue;

          try {
            const metaFile = await trackDir.getFileHandle('meta.json', { create: false });
            const file = await metaFile.getFile();
            const text = await file.text();
            const meta = JSON.parse(text) as OPFSTrackMeta;
            if (!meta.isComplete) {
              results.push(meta);
              seen.add(key);
            }
          } catch {
            // Track without meta.json; reconstruct partial metadata
            const seqs = await this.listLocalSeqs(takeName, trackName);
            if (seqs.length > 0) {
              results.push({
                takeId: takeName,
                trackId: trackName,
                uploadToken: '',
                kind: 'video',
                lastSeq: seqs[seqs.length - 1],
                isComplete: false,
                createdAt: Date.now(),
              });
              seen.add(key);
            }
          }
        }
      }
    } catch (err) {
      throw err;
    }

    return results;
  }

  public async deleteTrack(takeId: string, trackId: string): Promise<void> {
    if (await this.backend() === 'memory') {
      const prefix = `${this.memKey(takeId, trackId)}/`;
      for (const key of [...this.memory.keys()]) if (key.startsWith(prefix)) this.memory.delete(key);
      this.memoryMeta.delete(this.memKey(takeId, trackId));
      return;
    }
    try {
      const root = await navigator.storage.getDirectory();
      const takesDir = await root.getDirectoryHandle('takes', { create: false });
      const takeDir = await takesDir.getDirectoryHandle(takeId, { create: false });
      const tracksDir = await takeDir.getDirectoryHandle('tracks', { create: false });
      await tracksDir.removeEntry(trackId, { recursive: true });
    } catch {
      // Ignored if already removed
    }
  }
}
