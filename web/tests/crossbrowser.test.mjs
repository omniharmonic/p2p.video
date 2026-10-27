// Safari and Firefox guests [SL-P1-12]: the pure parts — browser note,
// VP8-first codec preference, coarse-timer clock samples, and the memory
// recording backend used when a private window has no OPFS.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import ts from 'typescript';

const load = async file => {
  const source = await readFile(new URL(`../src/lib/${file}`, import.meta.url), 'utf8');
  const js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } }).outputText;
  return import(`data:text/javascript;base64,${Buffer.from(js).toString('base64')}`);
};
const support = await load('browsersupport.ts');
const rtc = await load('rtc.ts');
const clock = await load('clock.ts');

const UA = {
  chrome: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36',
  edge: 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36 Edg/141.0.0.0',
  safari: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.5 Safari/605.1.15',
  iphone: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1',
  chromeIOS: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/141.0 Mobile/15E148 Safari/604.1',
  firefox: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:151.0) Gecko/20100101 Firefox/151.0',
};
const full = { engine: 'webkit', mobile: false, setSinkId: true, listsOutputs: true, displayMedia: true, audioWorklet: true, mediaRecorder: true, storage: 'opfs' };

test('engine detection: every iOS browser is WebKit', () => {
  assert.equal(support.detectEngine(UA.chrome), 'chromium');
  assert.equal(support.detectEngine(UA.edge), 'chromium');
  assert.equal(support.detectEngine(UA.safari), 'webkit');
  assert.equal(support.detectEngine(UA.iphone), 'webkit');
  assert.equal(support.detectEngine(UA.chromeIOS), 'webkit');
  assert.equal(support.detectEngine(UA.firefox), 'firefox');
});

test('no note when nothing is missing, and never for Chromium', () => {
  assert.equal(support.browserNote(full), '');
  assert.equal(support.browserNote({ ...full, engine: 'chromium', displayMedia: false, storage: 'memory' }), '');
  assert.deepEqual(support.browserLimitations(full), []);
});

test('Firefox without listed outputs: the note names speaker choice only', () => {
  const caps = { ...full, engine: 'firefox', listsOutputs: false };
  assert.deepEqual(support.noteLimitationIds(caps), ['speaker']);
  const note = support.browserNote(caps);
  assert.match(note, /^You can join and record from Firefox\./);
  assert.match(note, /choosing a speaker output in the call \(Firefox uses your system default output\)/);
  assert.match(note, /Chrome or Edge supports it\.$/);
});

test('iPhone Safari: screen, speaker; private window is its own advice', () => {
  const caps = { ...full, mobile: true, setSinkId: false, displayMedia: false, storage: 'memory' };
  assert.deepEqual(support.noteLimitationIds(caps), ['screen', 'speaker', 'storage']);
  const note = support.browserNote(caps);
  assert.match(note, /Safari on iPhone or iPad doesn’t support presenting your screen; and choosing a speaker output/);
  assert.match(note, /Chrome or Edge on a computer supports them\./);
  assert.match(note, /private window.*Open the link in a normal window/);
  // Only a private window: no other-browser recommendation.
  const priv = support.browserNote({ ...full, storage: 'memory' });
  assert.doesNotMatch(priv, /Chrome/);
  assert.match(priv, /private window/);
});

test('no MediaRecorder: the note does not promise recording', () => {
  const note = support.browserNote({ ...full, mediaRecorder: false });
  assert.match(note, /^You can join from Safari\./);
  assert.match(note, /local recording/);
});

test('VP8 first, everything else in the browser order', () => {
  const safari = ['video/H264', 'video/rtx', 'video/H265', 'video/VP8', 'video/VP9', 'video/red'].map(mimeType => ({ mimeType }));
  assert.deepEqual(rtc.universalVideoFirst(safari).map(c => c.mimeType), ['video/VP8', 'video/H264', 'video/rtx', 'video/H265', 'video/VP9', 'video/red']);
  const noVp8 = [{ mimeType: 'video/H264' }];
  assert.deepEqual(rtc.universalVideoFirst(noVp8), noVp8);
});

test('preferUniversalVideo applies to video transceivers only, and tolerates refusal', () => {
  const caps = [{ mimeType: 'video/H264' }, { mimeType: 'video/VP8' }];
  globalThis.RTCRtpReceiver = { getCapabilities: kind => (kind === 'video' ? { codecs: caps } : null) };
  try {
    const set = [];
    const t = (kind, refuse = false) => ({ sender: { track: kind ? { kind } : null }, receiver: { track: { kind: kind || 'video' } }, currentDirection: 'sendrecv', setCodecPreferences(list) { if (refuse) throw new Error('InvalidModificationError'); set.push([kind, list.map(c => c.mimeType)]); } });
    rtc.preferUniversalVideo({ getTransceivers: () => [t('audio'), t('video'), t(null), t('video', true)] });
    assert.deepEqual(set, [['video', ['video/VP8', 'video/H264']], [null, ['video/VP8', 'video/H264']]]);
  } finally { delete globalThis.RTCRtpReceiver; }
});

test('a sub-resolution round trip still counts as a measured clock sample', () => {
  const original = performance.now;
  let now = 1000;
  performance.now = () => now;
  try {
    const sync = new clock.ClockSync();
    const ping = sync.createPing();
    // Firefox/Safari coarsen performance.now: the pong arrives in the same ms.
    sync.handlePong({ t0: ping.t0, t1: 5_000_000, t2: 5_000_010 });
    assert.ok(sync.getFit().rttP10Ms > 0, JSON.stringify(sync.getFit()));
    now = 2000;
    const p2 = sync.createPing();
    sync.handlePong({ t0: p2.t0, t1: 6_000_000, t2: 6_000_010 });
    assert.ok(sync.getFit().rttP10Ms > 0);
    assert.ok(Math.abs(sync.getFit().b - 1) < 1e-6);
  } finally { performance.now = original; }
});

test('pre-flight: a private window is a warning, OPFS via a worker is fine', async () => {
  const pf = await load('preflight.ts');
  assert.equal(pf.evaluateOPFS(false, 'UnknownError').status, 'fail');
  const memory = pf.evaluateOPFS(false, 'UnknownError', 'memory');
  assert.equal(memory.status, 'warn');
  assert.match(memory.detail, /Private browsing.*uploads as it records/);
  assert.deepEqual([pf.evaluateOPFS(true, undefined, 'opfs-worker').status, pf.evaluateOPFS(true).status], ['ok', 'ok']);
});

test('memory backend: no OPFS keeps fragments in memory and frees acknowledged ones', async () => {
  const hadNavigator = Object.getOwnPropertyDescriptor(globalThis, 'navigator');
  Object.defineProperty(globalThis, 'navigator', { value: { storage: {} }, configurable: true });
  try {
    const { OPFSStorage } = await load('opfs.ts');
    const store = new OPFSStorage();
    assert.equal(await store.backend(), 'memory');
    assert.match(store.unavailableReason, /getDirectory/);
    const meta = { takeId: 't', trackId: 'k', uploadToken: 'u', kind: 'video', lastSeq: -1, isComplete: false, createdAt: 1, chunks: {} };
    await store.saveMeta('t', 'k', meta);
    await store.saveChunk('t', 'k', 0, new Uint8Array([1, 2]));
    await store.saveChunk('t', 'k', 1, new Uint8Array([3]));
    assert.deepEqual(await store.listLocalSeqs('t', 'k'), [0, 1]);
    assert.deepEqual([...await store.getChunk('t', 'k', 0)], [1, 2]);
    assert.equal((await store.listUnfinishedTracks()).length, 1);
    await store.releaseUploaded('t', 'k', 0);
    assert.equal(await store.getChunk('t', 'k', 0), null);
    assert.deepEqual(await store.listLocalSeqs('t', 'k'), [1]);
    // Stored copies are independent of the caller's buffers.
    meta.lastSeq = 99;
    assert.equal((await store.getMeta('t', 'k')).lastSeq, -1);
    await store.deleteTrack('t', 'k');
    assert.equal(await store.getMeta('t', 'k'), null);
    assert.deepEqual(await store.listLocalSeqs('t', 'k'), []);
  } finally {
    if (hadNavigator) Object.defineProperty(globalThis, 'navigator', hadNavigator); else delete globalThis.navigator;
  }
});
