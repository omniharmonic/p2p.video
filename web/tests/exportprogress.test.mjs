// Export progress and quick-delivery originals [SL-P0-9].
import test from 'node:test';
import assert from 'node:assert/strict';
import { build } from 'esbuild';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const dir = await mkdtemp(join(tmpdir(), 'studiolink-exportprogress-'));
const out = join(dir, 'exportprogress.mjs');
await build({ entryPoints: [new URL('../src/lib/exportprogress.ts', import.meta.url).pathname], bundle: true, format: 'esm', outfile: out });
const { parseTakeExport, describeExport, formatElapsed, originalsUrl, pollTakeExport } = await import(out);
test.after(() => rm(dir, { recursive: true, force: true }));

test('parses the take status export fields defensively', () => {
  assert.deepEqual(parseTakeExport({}), { originalsReady: false, postproc: null });
  assert.deepEqual(parseTakeExport({ originalsReady: 'yes', postproc: { state: 'weird', percent: 3 } }), { originalsReady: false, postproc: null });
  const view = parseTakeExport({ originalsReady: true, postproc: { takeId: 't', state: 'running', stage: 'video', percent: 142, elapsedSec: 65.4, videoCodec: 'h264', videoWorkers: 2, stages: [] } });
  assert.equal(view.originalsReady, true);
  assert.deepEqual(view.postproc, { takeId: 't', state: 'running', stage: 'video', percent: 100, elapsedSec: 65.4, videoCodec: 'h264', videoWorkers: 2 });
});

test('describes running, failed and complete exports', () => {
  assert.equal(describeExport({ state: 'running', stage: 'video', percent: 42.9, elapsedSec: 65 }), 'Conforming video · 42% · 1:05 elapsed');
  assert.equal(describeExport({ state: 'running', stage: 'unknown', percent: 0, elapsedSec: 0 }), 'Preparing editor files · 0% · 0:00 elapsed');
  assert.equal(describeExport({ state: 'failed', stage: 'video', percent: 40, elapsedSec: 3, error: 'transcode video x' }), 'Export failed: transcode video x');
  assert.equal(describeExport({ state: 'complete', stage: 'complete', percent: 100, elapsedSec: 3725 }), 'Editor files ready · 1:02:05');
  assert.equal(formatElapsed(-4), '0:00');
});

test('originals URL escapes the take and token', () => {
  assert.equal(originalsUrl('t/1', 'a+b'), '/api/v1/takes/t%2F1/originals?token=a%2Bb');
});

// Deterministic: setTimeout is mocked, so no wall-clock time decides how many
// polls happen. A starved event loop (the full suite runs test files in
// parallel) used to let the 60 ms deadline fire after only one or two polls.
test('polls take status with the host token until stopped', async t => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const calls = [];
  const views = [];
  let n = 0;
  const fetchImpl = async (url, init) => {
    calls.push({ url, auth: init.headers.Authorization });
    if (++n === 2) throw new Error('offline');
    return { ok: true, json: async () => ({ originalsReady: true, postproc: { state: 'running', stage: 'audio', percent: n * 10, elapsedSec: n } }) };
  };
  // Let the in-flight tick (fetch, json, onUpdate, re-arm) settle. setImmediate
  // is not mocked and runs after all pending microtasks.
  const settle = () => new Promise(resolve => setImmediate(resolve));
  const stop = pollTakeExport('take', 'tok', view => views.push(view), { intervalMs: 5, fetchImpl });
  await settle();
  assert.equal(calls.length, 1, 'first poll is immediate');
  t.mock.timers.tick(4);
  await settle();
  assert.equal(calls.length, 1, 'no poll before the interval');
  t.mock.timers.tick(1);
  await settle();
  assert.equal(calls.length, 2, 'second poll after one interval (this one fails)');
  t.mock.timers.tick(5);
  await settle();
  assert.equal(calls.length, 3, 'a transient failure does not end polling');
  assert.equal(views.length, 2);
  stop();
  t.mock.timers.tick(50);
  await settle();
  assert.equal(calls.length, 3, 'no polling after stop');
  assert.equal(calls[0].url, '/api/v1/takes/take');
  assert.equal(calls[0].auth, 'Bearer tok');
  assert.ok(views.every(v => v.originalsReady && v.postproc.stage === 'audio'));
  assert.deepEqual(views.map(v => v.postproc.percent), [10, 30]);
});

test('stop during an in-flight poll suppresses the update and the next tick', async t => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  let release;
  const gate = new Promise(resolve => { release = resolve; });
  let calls = 0;
  const views = [];
  const fetchImpl = async () => { calls++; await gate; return { ok: true, json: async () => ({ originalsReady: true }) }; };
  const stop = pollTakeExport('take', 'tok', view => views.push(view), { intervalMs: 5, fetchImpl });
  stop();
  release();
  await new Promise(resolve => setImmediate(resolve));
  t.mock.timers.tick(100);
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(calls, 1);
  assert.equal(views.length, 0);
});
