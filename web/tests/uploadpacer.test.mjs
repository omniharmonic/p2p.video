// Upload pacing during a live take [SL-P0-8]: policy, token bucket, shared
// scheduler, and the pooled ProgressiveUploadQueue never dropping data.
import test from 'node:test';
import assert from 'node:assert/strict';
import { build } from 'esbuild';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const dir = await mkdtemp(join(tmpdir(), 'studiolink-uploadpacer-'));
const load = async name => {
  const out = join(dir, `${name}.mjs`);
  await build({ entryPoints: [new URL(`../src/lib/${name}.ts`, import.meta.url).pathname], bundle: true, format: 'esm', outfile: out });
  return import(out);
};
const { uploadPolicy, TokenBucket, UploadScheduler, UPLOAD_PACING, backlogSeconds, formatBacklog, etaRateBps } = await load('uploadpacer');
const { ProgressiveUploadQueue } = await load('upload');
test.after(() => rm(dir, { recursive: true, force: true }));

const tick = () => new Promise(resolve => setImmediate(resolve));
async function settle(n = 20) { for (let i = 0; i < n; i++) await tick(); }

// Fake clock: sleep() resolves when the test advances time.
function fakeClock() {
  let now = 0; const timers = [];
  return {
    now: () => now,
    sleep: ms => new Promise(resolve => timers.push({ at: now + ms, resolve })),
    async advance(ms) {
      const end = now + ms;
      for (;;) {
        timers.sort((a, b) => a.at - b.at);
        const next = timers[0];
        if (!next || next.at > end) break;
        timers.shift(); now = next.at; next.resolve(); await settle();
      }
      now = end; await settle();
    },
  };
}

test('policy: live 2, relayed 1, throttled 1 with a paced share, drain 4 after Stop', () => {
  const base = { recording: true, quality: 'good', bandwidthLimited: false, path: 'direct', availableOutgoingBps: 2_000_000 };
  assert.deepEqual(uploadPolicy(base), { mode: 'live', concurrency: 2, rateLimitBps: null });
  assert.deepEqual(uploadPolicy({ ...base, path: 'relay' }), { mode: 'live', concurrency: 1, rateLimitBps: null });
  assert.deepEqual(uploadPolicy({ ...base, bandwidthLimited: true }), { mode: 'throttled', concurrency: 1, rateLimitBps: 500_000 });
  assert.deepEqual(uploadPolicy({ ...base, capsReduced: true }), { mode: 'throttled', concurrency: 1, rateLimitBps: 1_000_000 }, 'call adapted down: paced with a larger share');
  assert.equal(uploadPolicy({ ...base, capsReduced: true, bandwidthLimited: true }).rateLimitBps, 500_000);
  assert.deepEqual(uploadPolicy({ ...base, quality: 'poor', availableOutgoingBps: 10_000_000 }), { mode: 'throttled', concurrency: 1, rateLimitBps: 1_500_000 });
  assert.equal(uploadPolicy({ ...base, quality: 'poor', availableOutgoingBps: 300_000 }).rateLimitBps, UPLOAD_PACING.minRateBps, 'floor keeps the backlog moving');
  assert.equal(uploadPolicy({ ...base, quality: 'poor', availableOutgoingBps: undefined }).rateLimitBps, UPLOAD_PACING.minRateBps);
  assert.deepEqual(uploadPolicy({ ...base, recording: false, quality: 'poor' }), { mode: 'drain', concurrency: 4, rateLimitBps: null });
});

test('token bucket: burst, then the long-run rate holds even for big chunks', () => {
  const b = new TokenBucket(800_000, 100_000, 0); // 100 kB/s
  assert.equal(b.waitMs(0), 0);
  b.charge(300_000, 0); // 200 kB of debt
  assert.equal(b.waitMs(0), 2000);
  assert.equal(b.waitMs(1000), 1000);
  assert.equal(b.waitMs(2000), 0);
  b.setRate(1_600_000, 2000);
  b.charge(200_000, 2000);
  assert.equal(b.waitMs(2000), 1000, 'new rate applies to the remaining debt');
});

test('scheduler: global FIFO concurrency across queues; widening wakes waiters', async () => {
  const s = new UploadScheduler(fakeClock());
  s.configure({ mode: 'throttled', concurrency: 1, rateLimitBps: null });
  const order = []; const releases = [];
  for (const id of ['a', 'b', 'c']) void s.acquire(10).then(r => { order.push(id); releases.push(r); });
  await settle();
  assert.deepEqual(order, ['a']);
  s.configure({ mode: 'drain', concurrency: 4, rateLimitBps: null });
  await settle();
  assert.deepEqual(order, ['a', 'b', 'c'], 'Stop speeds the backlog up at once');
  assert.equal(s.inFlight, 3);
  releases.forEach(r => { r(); r(); });
  assert.equal(s.inFlight, 0, 'release is idempotent');
});

test('scheduler: token bucket paces chunk starts and a switch to drain cancels the wait', async () => {
  const clock = fakeClock();
  const s = new UploadScheduler(clock);
  s.configure({ mode: 'throttled', concurrency: 1, rateLimitBps: 800_000 }); // 100 kB/s, 256 kB burst
  const started = [];
  const go = async id => { const r = await s.acquire(356 * 1024); started.push([id, clock.now()]); r(); };
  void go('a'); void go('b'); void go('c');
  await settle();
  assert.deepEqual(started.map(x => x[0]), ['a'], 'first chunk spends the burst');
  await clock.advance(500);
  assert.equal(started.length, 1);
  await clock.advance(600);
  assert.deepEqual(started.map(x => x[0]), ['a', 'b'], '100 kB debt cleared after ~1 s');
  s.configure({ mode: 'drain', concurrency: 4, rateLimitBps: null });
  await clock.advance(250);
  assert.equal(started.length, 3, 'drain stops pacing within one 250 ms check');
});

test('scheduler: backlog, recent rate and snapshot', async () => {
  const clock = fakeClock();
  const s = new UploadScheduler(clock);
  assert.equal(s.mode, 'idle');
  s.setPending('t/a', 3_000_000); s.setPending('t/b', 1_000_000);
  assert.equal(s.pendingBytes, 4_000_000);
  assert.equal(s.mode, 'drain');
  s.recordAcked(500_000); await clock.advance(1000); s.recordAcked(500_000); await clock.advance(1000);
  assert.equal(s.rateBps, 4_000_000, '1 MB over 2 s');
  await clock.advance(29_000);
  assert.equal(s.rateBps, 133_333, 'only the second ack is still in the 30 s window');
  await clock.advance(2_000);
  assert.equal(s.rateBps, 0);
  s.recordAcked(500_000); await clock.advance(1000); s.recordAcked(500_000); await clock.advance(1000);
  s.configure({ mode: 'throttled', concurrency: 1, rateLimitBps: 256_000 });
  assert.deepEqual(s.snapshot(), { pendingBytes: 4_000_000, rateBps: 4_000_000, mode: 'throttled', concurrency: 1, rateLimitBps: 256_000 });
  s.setPending('t/a', 0);
  assert.equal(s.pendingBytes, 1_000_000);
  assert.equal(backlogSeconds(1_000_000, 800_000), 10);
  assert.equal(backlogSeconds(1_000_000, 0), null);
  assert.equal(backlogSeconds(0, 0), 0);
  assert.equal(formatBacklog(18 * 1048576, 1_000_000), 'Uploading 18 MB · ~3 min left');
  assert.equal(formatBacklog(1048576, 4_000_000), 'Uploading 1.0 MB · ~3 s left');
  assert.equal(formatBacklog(1048576, 0), 'Uploading 1.0 MB · stalled');
  assert.equal(etaRateBps({ rateBps: 0, mode: 'throttled', rateLimitBps: 256_000 }), 256_000, 'paced, not stalled');
  assert.equal(etaRateBps({ rateBps: 0, mode: 'live' }), 0);
  assert.equal(etaRateBps({ rateBps: 900_000, mode: 'throttled', rateLimitBps: 256_000 }), 900_000);
});

class MemoryStorage {
  meta = null; chunks = new Map();
  async getMeta() { return this.meta && structuredClone(this.meta); }
  async saveMeta(_t, _k, meta) { this.meta = structuredClone(meta); }
  async saveChunk(_t, _k, seq, data) { this.chunks.set(seq, data.slice()); }
  async getChunk(_t, _k, seq) { return this.chunks.get(seq) || null; }
  async listLocalSeqs() { return [...this.chunks.keys()].sort((a, b) => a - b); }
}
const sidecar = { trackId: 'track', kind: 'video', clockFit: { a: 0, b: 1, rttP10Ms: 0 }, events: [] };
const receipt = (total) => new Response(JSON.stringify({ status: 'assembled', totalBytes: total, sha256: 'a'.repeat(64) }));

test('pooled queue: throttled to one upload, widens to four on drain, never drops a chunk', async () => {
  const s = new UploadScheduler(); // real clock, no pacing
  s.configure({ mode: 'throttled', concurrency: 1, rateLimitBps: null });
  const storage = new MemoryStorage();
  let inflight = 0, peak = 0; const gates = []; const put = new Map();
  const fetch = async (url, init) => {
    if (url.endsWith('/complete')) return receipt(JSON.parse(init.body).totalBytes);
    inflight++; peak = Math.max(peak, inflight);
    await new Promise(r => gates.push(r));
    inflight--;
    const seq = Number(url.split('/').pop());
    put.set(seq, (put.get(seq) || 0) + 1);
    return new Response(null, { status: 201 });
  };
  const progress = [];
  const queue = new ProgressiveUploadQueue('take', 'track', 'token', p => progress.push(p), { storage, fetch, scheduler: s, kind: 'video' });
  for (let seq = 0; seq < 12; seq++) await queue.enqueue({ seq, data: new Uint8Array([seq, 1, 2, 3]), perfTimeMs: seq });
  await settle();
  assert.equal(inflight, 1, 'one upload while the call is congested');
  assert.equal(s.pendingBytes, 48);
  gates.shift()(); await settle();
  assert.equal(peak, 1);
  s.configure({ mode: 'drain', concurrency: 4, rateLimitBps: null });
  await settle();
  assert.equal(inflight, 4, 'after Stop: four at once');
  const done = queue.complete(11, sidecar);
  while (put.size < 12 || gates.length) { gates.splice(0).forEach(g => g()); await settle(); }
  assert.equal(await done, true);
  assert.deepEqual([...put.keys()].sort((a, b) => a - b), [...Array(12).keys()], 'every chunk uploaded');
  assert.ok([...put.values()].every(n => n === 1), 'no chunk uploaded twice');
  assert.equal(s.pendingBytes, 0);
  assert.equal(progress.at(-1).isComplete, true);
  assert.equal(progress.at(-1).uploadedBytes, 48);
});

test('pooled queue: a failed upload keeps the chunk queued and on disk for recovery', async () => {
  const s = new UploadScheduler();
  s.configure({ mode: 'drain', concurrency: 4, rateLimitBps: null });
  const storage = new MemoryStorage();
  const fetch = async url => (url.endsWith('/chunks/2') ? new Response(null, { status: 403 }) : new Response(null, { status: 201 }));
  const queue = new ProgressiveUploadQueue('take', 'track', 'token', () => {}, { storage, fetch, scheduler: s, kind: 'video' });
  for (let seq = 0; seq < 5; seq++) await queue.enqueue({ seq, data: new Uint8Array([seq]), perfTimeMs: seq });
  await assert.rejects(queue.waitForUploads(), /HTTP 403/);
  assert.equal(storage.chunks.size, 5, 'nothing deleted locally');
  assert.ok(s.pendingBytes >= 1, 'the failed chunk still counts as backlog');
  assert.equal(s.inFlight, 0, 'slots released');
});
