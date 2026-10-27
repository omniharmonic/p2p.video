import test from 'node:test';
import assert from 'node:assert/strict';
import { build } from 'esbuild';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
const dir = await mkdtemp(join(tmpdir(), 'studiolink-recording-'));
const modules = {};
for (const name of ['recorder', 'upload', 'opfs']) {
  const out = join(dir, `${name}.mjs`);
  await build({ entryPoints: [new URL(`../src/lib/${name}.ts`, import.meta.url).pathname], bundle: true, format: 'esm', outfile: out });
  modules[name] = await import(out);
}
test.after(() => rm(dir, { recursive: true, force: true }));
const { LocalTrackRecorder } = modules.recorder;
const fit = { a: 0, b: 1, rttP10Ms: 0 };
const tick = () => new Promise(resolve => setImmediate(resolve));
let current;
class FakeRecorder {
  static isTypeSupported(mime) { return mime.includes('webm'); }
  state = 'inactive'; mimeType = 'video/webm;codecs=vp9,opus';
  constructor(stream) { current = this; this.stream = stream; }
  start() { this.state = 'recording'; }
  stop() { this.state = 'inactive'; this.onstop?.(); }
  emit(data,timecode=0) { this.ondataavailable({ data,timecode }); }
}
globalThis.MediaRecorder = FakeRecorder;
test('stop waits for ordered blob conversion and durable chunk callbacks', async () => {
  let firstResolve, durableResolve;
  const chunks = [];
  const recorder = new LocalTrackRecorder('track', 'video', async c => { chunks.push(c); if (c.seq === 0) await new Promise(r => { durableResolve = r; }); }, fit);
  await recorder.start({});
  current.emit({ size: 1, arrayBuffer: () => new Promise(r => { firstResolve = r; }) });
  current.emit({ size: 1, arrayBuffer: async () => new Uint8Array([2]).buffer });
  let stopped = false;
  const stop = recorder.stop().then(value => { stopped = true; return value; });
  await tick();
  assert.equal(stopped, false);
  firstResolve(new Uint8Array([1]).buffer);
  await tick();
  assert.equal(stopped, false);
  durableResolve();
  assert.equal((await stop).lastSeq, 1);
  assert.deepEqual(chunks.map(c => [...c.data]), [[1], [2]]);
});
test('empty recording is explicit and timing events do not pretend to be capture timestamps', async () => {
  const recorder = new LocalTrackRecorder('track', 'video', () => {}, fit);
  await recorder.start({});
  assert.equal((await recorder.stop()).lastSeq, -1);
});
test('video remains on the container recorder even when raw WebCodecs exists', async () => {
  globalThis.window = { VideoEncoder: class { constructor() { throw Error('raw encoder selected'); } }, MediaStreamTrackProcessor: class {} };
  const recorder = new LocalTrackRecorder('track', 'video', () => {}, fit);
  await recorder.start({});
  assert.equal(recorder.getEngine(), 'mediarecorder');
  assert.match(recorder.getMimeType(), /webm/);
  await recorder.stop();
  delete globalThis.window;
});

const { ProgressiveUploadQueue, recoverTakeFromOPFS } = modules.upload;
const sidecar = { trackId: 'track', kind: 'audio', clockFit: fit, events: [] };
class MemoryStorage {
  meta = null; chunks = new Map(); log = [];
  async getMeta() { return this.meta && structuredClone(this.meta); }
  async saveMeta(_take, _track, meta) { this.log.push('meta'); this.meta = structuredClone(meta); }
  async saveChunk(_take, _track, seq, data) { this.log.push('chunk'); this.chunks.set(seq, data.slice()); }
  async getChunk(_take, _track, seq) { return this.chunks.get(seq) || null; }
  async listLocalSeqs() { return [...this.chunks.keys()].sort((a,b) => a-b); }
}
const chunk = { seq: 0, data: new Uint8Array([1,2]), perfTimeMs: 0 };
test('uploads only after closed chunk and journal metadata; includes whole-file byte total', async () => {
  const storage = new MemoryStorage(); let completion;
  const queue = new ProgressiveUploadQueue('take','track','token', () => {}, { storage, fetch: async (url, init) => {
    assert.deepEqual(storage.log.slice(0,3), ['meta','chunk','meta']);
    if (url.endsWith('complete')) completion = JSON.parse(init.body);
    return Response.json({status:'assembled',totalBytes:2,sha256:'a'.repeat(64)});
  }, sleep: async () => {} });
  await queue.enqueue(chunk);
  assert.equal(await queue.complete(0, sidecar), true);
  assert.equal(completion.totalBytes, 2);
  assert.equal(storage.meta.isComplete, true);
});
test('storage failure rejects enqueue before making any upload request', async () => {
  const storage = new MemoryStorage(); storage.saveChunk = async () => { throw Error('quota'); };
  let requests = 0;
  const queue = new ProgressiveUploadQueue('take','track','token', () => {}, { storage, fetch: async () => { requests++; return new Response('{}'); } });
  await assert.rejects(queue.enqueue(chunk), /quota/);
  assert.equal(requests, 0);
});
test('recovery preserves sidecar and counts chunks already received by the host', async () => {
  const storage = new MemoryStorage(); storage.chunks.set(0, chunk.data);
  storage.meta = { takeId:'take',trackId:'track',uploadToken:'token',kind:'audio',lastSeq:0,isComplete:false,createdAt:123,sidecar };
  const queue = new ProgressiveUploadQueue('take','track','token', () => {}, { storage, fetch: async (url, init) => {
    if (url.endsWith('status')) return Response.json({have:[[0,0]]});
    assert.equal(JSON.parse(init.body).totalBytes, 2);
    return Response.json({status:'assembled',totalBytes:2,sha256:'a'.repeat(64)});
  } });
  await queue.ready();
  assert.deepEqual(storage.meta.sidecar, sidecar);
  assert.equal(storage.meta.createdAt, 123);
  assert.deepEqual((await queue.syncWithHostStatus()).missing, []);
  assert.equal(await queue.complete(0, sidecar), true);
});
test('terminal upload failures stop and return incomplete without infinite retries', async () => {
  const storage = new MemoryStorage(); let requests = 0;
  const queue = new ProgressiveUploadQueue('take','track','token', () => {}, { storage, fetch: async () => { requests++; return new Response('',{status:403}); }, sleep:async()=>{} });
  await queue.enqueue(chunk);
  assert.equal(await queue.complete(0,sidecar), false);
  assert.equal(requests,1);
  assert.equal(storage.meta.isComplete,false);
});
test('a host upload limit (413) is terminal and its reason reaches the user [SL-SEC-1]', async () => {
  const storage = new MemoryStorage(); let requests = 0; const progress = [];
  const reason = 'track exceeds the host\'s storage limit for this take (900 MB); the recording stays on this device';
  const queue = new ProgressiveUploadQueue('take','track','token', p => progress.push(p), { storage, fetch: async () => { requests++; return new Response(reason+'\n',{status:413}); }, sleep:async()=>{} });
  await queue.enqueue(chunk);
  assert.equal(await queue.complete(0,sidecar), false);
  assert.equal(requests,1);
  assert.match(progress.at(-1).error, /HTTP 413/);
  assert.match(progress.at(-1).error, /storage limit/);
  assert.equal(storage.meta.isComplete,false);
  assert.ok(storage.chunks.has(0),'the local fragment is kept for recovery');
});
test('recovery without final sidecar cannot claim success', async () => {
  assert.equal(await recoverTakeFromOPFS({takeId:'take',trackId:'track',uploadToken:'',kind:'video',lastSeq:0,isComplete:false,createdAt:0}, ''), false);
});

test('OPFS corrupt metadata is surfaced instead of being treated as absent and overwritten', async () => {
  const leaf = { getFileHandle: async () => ({ getFile: async () => ({ text: async () => '{broken' }) }) };
  const branch = depth => ({ getDirectoryHandle: async () => depth === 1 ? leaf : branch(depth-1) });
  Object.defineProperty(globalThis, 'navigator', { configurable: true, value: { storage: { getDirectory: async () => branch(4) } } });
  await assert.rejects(modules.opfs.OPFSStorage.getInstance().getMeta('take','track'), /JSON|position|property/i);
});
test('recorder stops and reports failure when durable storage cannot keep up', async () => {
  let failure;
  const recorder = new LocalTrackRecorder('track','video', async () => new Promise(() => {}), fit, {onError: e => {failure=e;}});
  await recorder.start({});
  current.emit({size: 65 * 1024 * 1024, arrayBuffer: async () => new ArrayBuffer(0)});
  await tick();
  assert.match(failure?.message || '', /buffer/i);
  assert.equal(current.state,'inactive');
  await assert.rejects(recorder.stop(), /buffer/i);
});
test('transient failure is bounded to five attempts and final metadata remains recoverable', async () => {
  const storage = new MemoryStorage(); let requests = 0;
  const queue = new ProgressiveUploadQueue('take','track','token', () => {}, { storage, fetch: async () => { requests++; return new Response('',{status:503}); }, sleep:async()=>{} });
  await queue.enqueue(chunk);
  assert.equal(await queue.complete(0,sidecar), false);
  assert.equal(requests,5);
  assert.deepEqual(storage.meta.sidecar,sidecar);
  assert.equal(storage.meta.isComplete,false);
});
test('host status failure is not converted into a successful empty reconciliation', async () => {
  const storage = new MemoryStorage();
  const queue = new ProgressiveUploadQueue('take','track','token', () => {}, { storage, fetch: async () => new Response('',{status:401}), sleep:async()=>{} });
  await assert.rejects(queue.syncWithHostStatus(), /401/);
});
test('recovery refuses a missing local fragment even if host claims it received it', async () => {
  const storage = new MemoryStorage();
  storage.meta={takeId:'take',trackId:'track',kind:'audio',uploadToken:'token',lastSeq:0,createdAt:1,isComplete:false,sidecar};
  const queue = new ProgressiveUploadQueue('take','track','token',()=>{}, {storage,fetch:async()=>Response.json({have:[[0,0]]})});
  await assert.rejects(queue.syncWithHostStatus(), /fragment 0 is missing/);
});
test('HTTP 200 without assembly receipt cannot mark local recording complete', async () => {
  const storage=new MemoryStorage();
  const queue=new ProgressiveUploadQueue('take','track','token',()=>{}, {storage,fetch:async()=>Response.json({})});
  await queue.enqueue(chunk);
  assert.equal(await queue.complete(0,sidecar),false);
  assert.equal(storage.meta.isComplete,false);
});

test('sidecar media timestamps use integer microseconds accepted by host protocol', async()=>{
  const recorder=new LocalTrackRecorder('track','video',()=>{},fit);
  await recorder.start({});
  current.emit({size:1,arrayBuffer:async()=>new Uint8Array([1]).buffer},0.799999997);
  const {sidecar}=await recorder.stop();
  assert.equal(sidecar.events.find(event=>event.type==='dataavailable').mediaTimeUs,800);
});
test('deferred WAV header is journaled last but assembled first, with all bytes in receipt',async()=>{
 const storage=new MemoryStorage();const uploads=[];
 const queue=new ProgressiveUploadQueue('take','track','token',()=>{},{storage,kind:'audio',deferredWavHeader:true,fetch:async(url,init)=>{
  if(url.endsWith('/complete'))return Response.json({status:'assembled',totalBytes:66,sha256:'a'.repeat(64)});
  uploads.push(Number(url.split('/').at(-1)));return Response.json({});
 }});
 await queue.enqueue({seq:1,data:new Uint8Array(8),perfTimeMs:0,sidecar:{...sidecar,pcm:{format:'float32le',sampleRate:48000,channels:1,frames:2},captureComplete:false}});
 assert.equal(storage.meta.headerPending,true);
 assert.equal(storage.meta.sidecar.pcm.frames,2);
 await queue.enqueue({seq:0,data:new Uint8Array(58),perfTimeMs:1});
 assert.equal(storage.meta.headerPending,false);
 assert.equal(await queue.complete(1,{...sidecar,captureComplete:true}),true);
 assert.deepEqual(uploads,[1,0]);
});
test('PCM track cannot finalize while its exact WAV header is missing',async()=>{
 const storage=new MemoryStorage();let completes=0;
 const queue=new ProgressiveUploadQueue('take','track','token',()=>{},{storage,kind:'audio',deferredWavHeader:true,fetch:async(url)=>{if(url.endsWith('/complete'))completes++;return Response.json({});}});
 await queue.enqueue({seq:1,data:new Uint8Array(8),perfTimeMs:0});
 assert.equal(await queue.complete(1,sidecar),false);assert.equal(completes,0);
});

// Tab closed or crashed mid-take [SL-P0-7]: compressed tracks journal timing
// checkpoints so the reopened invite can finalize them, as partial captures.
test('recorder journals a timing checkpoint with the first fragment and periodically', async () => {
  const { SIDECAR_CHECKPOINT_EVERY } = modules.recorder;
  const chunks = [];
  const recorder = new LocalTrackRecorder('track', 'video', c => { chunks.push(c); }, fit);
  await recorder.start({});
  for (let i = 0; i <= SIDECAR_CHECKPOINT_EVERY; i++) current.emit({ size: 1, arrayBuffer: async () => new Uint8Array([i]).buffer }, i * 1000);
  await recorder.stop();
  const withSidecar = chunks.filter(c => c.sidecar).map(c => c.seq);
  assert.deepEqual(withSidecar, [0, SIDECAR_CHECKPOINT_EVERY]);
  assert.ok(chunks[0].sidecar.events.some(e => e.type === 'recorder-start'));
  assert.equal(chunks[0].sidecar.mimeType, 'video/webm;codecs=vp9,opus');
  chunks[0].sidecar.events.push({ type: 'mutated' });
  assert.ok(!chunks[SIDECAR_CHECKPOINT_EVERY].sidecar.events.some(e => e.type === 'mutated'), 'checkpoints are snapshots');
});
test('interrupted compressed capture recovers as partial; a stopped one stays complete', async () => {
  for (const [events, interrupted] of [[[{ type: 'recorder-start', perfTime: 1, mediaTimeUs: 0 }], true], [[{ type: 'recorder-stop', perfTime: 2, mediaTimeUs: 0 }], false]]) {
    const storage = new MemoryStorage(); storage.chunks.set(0, chunk.data);
    const checkpoint = { trackId: 'track', kind: 'video', engine: 'mediarecorder', clockFit: fit, events };
    storage.meta = { takeId: 'take', trackId: 'track', uploadToken: 'token', kind: 'video', lastSeq: 0, isComplete: false, createdAt: 1, sidecar: checkpoint };
    let completion;
    const queue = new ProgressiveUploadQueue('take', 'track', 'token', () => {}, { storage, fetch: async (url, init) => {
      if (url.endsWith('status')) return Response.json({ have: [] });
      if (url.endsWith('complete')) completion = JSON.parse(init.body);
      return Response.json({ status: 'assembled', totalBytes: 2, sha256: 'a'.repeat(64) });
    }, sleep: async () => {} });
    const recovery = await queue.prepareRecovery();
    await queue.syncWithHostStatus();
    assert.equal(await queue.complete(recovery.lastSeq, recovery.sidecar), true);
    assert.equal(completion.sidecar.captureComplete === false, interrupted);
    assert.equal(completion.sidecar.events.some(e => e.type === 'capture-interrupted'), interrupted);
  }
});
test('recovery leaves a track alone while another tab holds its recording lock', async () => {
  const original = Object.getOwnPropertyDescriptor(globalThis, 'navigator');
  const requested = [];
  Object.defineProperty(globalThis, 'navigator', { configurable: true, value: { locks: { request: async (name, options, cb) => { requested.push([name, !!options.ifAvailable]); return cb(options.ifAvailable ? null : {}); } } } });
  try {
    const progress = [];
    const meta = { takeId: 'take', trackId: 'busy', uploadToken: 'token', kind: 'video', lastSeq: 0, isComplete: false, createdAt: 0, sidecar: { trackId: 'busy', kind: 'video', clockFit: fit, events: [] } };
    assert.equal(await recoverTakeFromOPFS(meta, '', p => progress.push(p)), false);
    assert.match(progress.at(-1).error, /another tab/);
    // A new (live) track claims its lock in the recording tab.
    const storage = new MemoryStorage();
    const queue = new ProgressiveUploadQueue('take', 'live', 'token', () => {}, { storage, fetch: async () => Response.json({}) });
    await queue.ready();
    assert.deepEqual(requested, [['studiolink-track:take:busy', true], ['studiolink-track:take:live', false]]);
  } finally {
    if (original) Object.defineProperty(globalThis, 'navigator', original); else delete globalThis.navigator;
  }
});
