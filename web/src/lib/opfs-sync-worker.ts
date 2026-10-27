// OPFS writes for engines without FileSystemFileHandle.createWritable
// (Safari ≤ 18) [SL-P1-12]: a dedicated worker writes each file whole through
// a FileSystemSyncAccessHandle (Safari 15.2+, worker-only). Reads, listing and
// removal stay on the main thread; only writing needs the worker.
interface WriteRequest { id: number; path: string[]; name: string; data: Uint8Array }
interface SyncAccessHandle { truncate(size: number): void; write(buffer: BufferSource, options?: { at?: number }): number; flush(): void; close(): void }
type SyncFileHandle = FileSystemFileHandle & { createSyncAccessHandle(): Promise<SyncAccessHandle> };

const scope = self as unknown as { onmessage: ((event: MessageEvent<WriteRequest>) => void) | null; postMessage(message: unknown): void };

// Writes are serialized: a sync access handle is exclusive per file.
let chain: Promise<void> = Promise.resolve();
scope.onmessage = (event) => {
  const { id, path, name, data } = event.data;
  chain = chain.then(async () => {
    let handle: SyncAccessHandle | null = null;
    try {
      let dir = await navigator.storage.getDirectory();
      for (const part of path) dir = await dir.getDirectoryHandle(part, { create: true });
      const file = await dir.getFileHandle(name, { create: true }) as SyncFileHandle;
      handle = await file.createSyncAccessHandle();
      handle.truncate(0);
      const written = handle.write(data as unknown as BufferSource, { at: 0 });
      if (written !== data.byteLength) throw new Error(`short write (${written} of ${data.byteLength} bytes)`);
      handle.flush();
      scope.postMessage({ id, ok: true });
    } catch (error) {
      const e = error as { name?: string; message?: string };
      scope.postMessage({ id, ok: false, name: e?.name || 'Error', message: e?.message || String(error) });
    } finally {
      try { handle?.close(); } catch { /* Already closed. */ }
    }
  });
};
