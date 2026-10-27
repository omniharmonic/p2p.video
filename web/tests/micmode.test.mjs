// Recording mic processing mode [SL-P1-10]: resolution, reporting, labels.
import test from 'node:test';
import assert from 'node:assert/strict';
import { build } from 'esbuild';
import { mkdtemp, rm, readFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const dir = await mkdtemp(join(tmpdir(), 'studiolink-micmode-'));
const out = join(dir, 'micmode.mjs');
await build({ entryPoints: [new URL('../src/lib/micmode.ts', import.meta.url).pathname], bundle: true, format: 'esm', outfile: out });
const { resolveRecordingMic, recordingMicState, recordingMicLabel, RECORDING_PROCESSING, loadRecordingMicMode, saveRecordingMicMode, isRecordingMicMode, RECORDING_MIC_STORAGE_KEY } = await import(out);
test.after(() => rm(dir, { recursive: true, force: true }));

test('auto follows headphones; the session default replaces auto; an explicit choice wins', () => {
  assert.deepEqual(resolveRecordingMic('auto', 'auto', true), { resolved: 'raw', reason: 'headphones' });
  assert.deepEqual(resolveRecordingMic('auto', undefined, false), { resolved: 'clean', reason: 'no_headphones' });
  assert.deepEqual(resolveRecordingMic('auto', 'raw', false), { resolved: 'raw', reason: 'session' });
  assert.deepEqual(resolveRecordingMic('auto', 'clean', true), { resolved: 'clean', reason: 'session' });
  assert.deepEqual(resolveRecordingMic('clean', 'raw', true), { resolved: 'clean', reason: 'participant' });
  assert.deepEqual(resolveRecordingMic('raw', 'clean', false), { resolved: 'raw', reason: 'participant' });
});

test('clean is echo cancellation + noise suppression without automatic gain; raw is nothing', () => {
  assert.deepEqual(RECORDING_PROCESSING.clean, { echoCancellation: true, noiseSuppression: true, autoGainControl: false });
  assert.deepEqual(RECORDING_PROCESSING.raw, { echoCancellation: false, noiseSuppression: false, autoGainControl: false });
});

test('the reported state carries what the browser actually applies', () => {
  const ok = recordingMicState('auto', 'auto', false, { echoCancellation: true, noiseSuppression: true, autoGainControl: false });
  assert.deepEqual(ok, { mode: 'auto', resolved: 'clean', reason: 'no_headphones', headphones: false, echoCancellation: true, noiseSuppression: true, autoGainControl: false });
  // A browser that applied the call's processing to the recording: said so.
  const shared = recordingMicState('raw', 'auto', true, { echoCancellation: true, noiseSuppression: true, autoGainControl: true });
  assert.equal(shared.followsCall, true);
  assert.equal(shared.resolved, 'raw');
  assert.match(recordingMicLabel(shared), /browser applies call processing/);
  // Unreported settings count as requested.
  assert.equal(recordingMicState('auto', 'raw', false, {}).followsCall, undefined);
});

test('labels the host reads', () => {
  assert.equal(recordingMicLabel({ resolved: 'raw', reason: 'headphones' }), 'Raw mic (headphones)');
  assert.equal(recordingMicLabel({ resolved: 'clean', reason: 'no_headphones' }), 'Clean mic (no headphones)');
  assert.equal(recordingMicLabel({ resolved: 'raw', reason: 'session' }), 'Raw mic (host setting)');
  assert.equal(recordingMicLabel(null), 'Recording mic unknown');
});

test('the own choice is remembered; garbage falls back to auto', () => {
  const m = new Map();
  const kv = { getItem: k => m.get(k) ?? null, setItem: (k, v) => m.set(k, v) };
  assert.equal(loadRecordingMicMode(kv), 'auto');
  saveRecordingMicMode(kv, 'clean');
  assert.equal(loadRecordingMicMode(kv), 'clean');
  m.set(RECORDING_MIC_STORAGE_KEY, 'loud');
  assert.equal(loadRecordingMicMode(kv), 'auto');
  assert.equal(loadRecordingMicMode({ getItem() { throw new Error('blocked'); } }), 'auto');
  assert.equal(isRecordingMicMode('raw'), true);
  assert.equal(isRecordingMicMode('RAW'), false);
});

test('protocol fixtures match the state shape', async () => {
  const pf = JSON.parse(await readFile(new URL('../../testdata/protocol/preflight-recording-mic.json', import.meta.url), 'utf8'));
  const state = pf.payload.recordingMic;
  assert.deepEqual(state, recordingMicState(state.mode, 'auto', state.headphones, state));
  assert.equal(recordingMicLabel(state), 'Clean mic (no headphones)');
  const rs = JSON.parse(await readFile(new URL('../../testdata/protocol/room-state-mic.json', import.meta.url), 'utf8'));
  assert.equal(isRecordingMicMode(rs.payload.recordingMicMode), true);
});
