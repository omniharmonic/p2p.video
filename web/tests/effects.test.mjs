// Background effects [SL-P1-7]: compositor parameters, mask → alpha, fps
// collapse detection and the remembered choice (pure, no browser).
import test from 'node:test';
import assert from 'node:assert/strict';
import { build } from 'esbuild';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const dir = await mkdtemp(join(tmpdir(), 'studiolink-effects-'));
const out = join(dir, 'segmentation.mjs');
await build({ entryPoints: [new URL('../src/lib/segmentation.ts', import.meta.url).pathname], bundle: true, format: 'esm', outfile: out, external: ['@mediapipe/tasks-vision'] });
const { EFFECTS, effectById, compositorParams, maskToAlpha, personMaskIndex, FpsMonitor, loadEffect, saveEffect, EFFECT_STORAGE_KEY, blockMediaPipeTelemetry } = await import(out);
test.after(() => rm(dir, { recursive: true, force: true }));

test('effects: Off, two blurs and generated backgrounds only (no images)', () => {
  assert.deepEqual(EFFECTS.map(e => e.id), ['off', 'blur-light', 'blur-strong', 'bg-graphite', 'bg-dusk', 'bg-ocean']);
  for (const e of EFFECTS.filter(e => e.kind === 'image')) assert.ok(e.gradient.stops.every(s => /^#[0-9a-f]{6}$/.test(s)), e.id);
  assert.ok(effectById('blur-strong').blurPx > effectById('blur-light').blurPx);
  assert.equal(effectById('nope').id, 'off');
});

test('compositor sizes: ≤720p output, never upscaled, even dimensions, 256-wide mask', () => {
  const strong = effectById('blur-strong');
  const p = compositorParams(strong, 1920, 1080);
  assert.deepEqual(p.out, { width: 1280, height: 720 });
  assert.deepEqual(p.mask, { width: 256, height: 144 });
  assert.equal(p.blurScale, 4);
  assert.equal(p.blurPx, 5.5); // 22 px at 720p, drawn on a quarter-size canvas
  // Portrait phone camera keeps its orientation.
  const portrait = compositorParams(strong, 1080, 1920);
  assert.deepEqual(portrait.out, { width: 720, height: 1280 });
  assert.deepEqual(portrait.mask, { width: 256, height: 456 });
  // Small cameras are not upscaled; blur scales with the output.
  const small = compositorParams(strong, 640, 360);
  assert.deepEqual(small.out, { width: 640, height: 360 });
  assert.equal(small.blurPx, 2.8);
  assert.equal(compositorParams(effectById('bg-dusk'), 1280, 720).blurPx, 0);
  // Odd or missing geometry still gives usable even sizes.
  const odd = compositorParams(strong, 1281, 721);
  assert.equal(odd.out.width % 2, 0); assert.equal(odd.out.height % 2, 0);
  assert.deepEqual(compositorParams(strong, 0, 0).out, { width: 1280, height: 720 });
});

test('mask → alpha: soft edges between the thresholds, white RGB, temporal smoothing', () => {
  const conf = new Float32Array([0, 0.3, 0.525, 0.75, 1]);
  const rgba = new Uint8ClampedArray(conf.length * 4);
  maskToAlpha(conf, rgba);
  const alpha = [...rgba].filter((_, i) => i % 4 === 3);
  assert.deepEqual([alpha[0], alpha[1], alpha[3], alpha[4]], [0, 0, 255, 255]);
  assert.ok(Math.abs(alpha[2] - 128) <= 1, `midpoint alpha ${alpha[2]}`);
  assert.ok([...rgba].filter((_, i) => i % 4 !== 3).every(v => v === 255));
  const prev = new Uint8ClampedArray(rgba.length); // previous frame: all background
  maskToAlpha(new Float32Array([1, 1, 1, 1, 1]), rgba, prev);
  assert.ok([...rgba].filter((_, i) => i % 4 === 3).every(a => a > 150 && a < 200), 'a sudden change is damped');
});

test('person mask by model label', () => {
  assert.equal(personMaskIndex(['selfie'], 1), 0); // MediaPipe selfie_segmenter's labels.txt
  assert.equal(personMaskIndex(['background', 'person'], 2), 1);
  assert.equal(personMaskIndex([], 1), 0);
  assert.equal(personMaskIndex([], 2), 1);
});

test('fps monitor: warm-up, sustained collapse, recovery and reset', () => {
  const m = new FpsMonitor(15, 5000, 4000);
  let t = 0;
  const run = (fps, ms) => { const end = t + ms; for (; t < end; t += 1000 / fps) m.frame(t); };
  run(30, 3000);
  assert.ok(Math.abs(m.fps(t) - 30) < 1.5);
  assert.equal(m.collapsed(t), false);
  run(8, 3000);           // low, but within warm-up + window
  assert.equal(m.collapsed(t), false);
  run(8, 5500);
  assert.equal(m.collapsed(t), true);
  run(30, 3000);           // recovered
  assert.equal(m.collapsed(t), false);
  m.reset();
  assert.equal(m.fps(t), 0);
  assert.equal(m.collapsed(t), false);
  // A short dip below the minimum is tolerated.
  const n = new FpsMonitor(15, 5000, 0);
  t = 0; for (; t < 2000; t += 33) n.frame(t);
  for (const end = t + 2000; t < end; t += 125) n.frame(t);
  assert.equal(n.collapsed(t), false);
});

test('choice is remembered; unknown or unreadable storage falls back to Off', () => {
  const m = new Map();
  const s = { getItem: k => m.get(k) ?? null, setItem: (k, v) => m.set(k, v) };
  assert.equal(loadEffect(s), 'off');
  saveEffect(s, 'bg-ocean');
  assert.equal(m.get(EFFECT_STORAGE_KEY), 'bg-ocean');
  assert.equal(loadEffect(s), 'bg-ocean');
  m.set(EFFECT_STORAGE_KEY, 'disco');
  assert.equal(loadEffect(s), 'off');
  assert.equal(loadEffect({ getItem: () => { throw new Error('denied'); } }), 'off');
});

test('MediaPipe usage logs never leave the page; other fetches pass through [SL-P1-12]', async () => {
  const original = globalThis.fetch;
  const seen = [];
  globalThis.fetch = async (input) => { seen.push(String(input?.url ?? input)); return new Response('ok'); };
  try {
    blockMediaPipeTelemetry();
    blockMediaPipeTelemetry(); // idempotent
    await assert.rejects(fetch('https://odml.pa.googleapis.com/v1/log', { method: 'POST' }), /MediaPipe usage logs/);
    await assert.rejects(fetch(new Request('https://odml.pa.googleapis.com/v1/log')), TypeError);
    assert.equal(await (await fetch('/api/v1/sessions')).text(), 'ok');
    assert.deepEqual(seen, ['/api/v1/sessions']);
  } finally { globalThis.fetch = original; }
});
