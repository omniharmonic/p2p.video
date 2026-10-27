// ClockSync under a coarse (1 ms) timer, as in WKWebView/Safari [SL-P2-3].
import test from 'node:test';
import assert from 'node:assert/strict';
import { build } from 'esbuild';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const dir = await mkdtemp(join(tmpdir(), 'studiolink-clock-'));
const out = join(dir, 'clock.mjs');
await build({ entryPoints: [new URL('../src/lib/clock.ts', import.meta.url).pathname], bundle: true, format: 'esm', outfile: out });
const { ClockSync } = await import(out);
test.after(() => rm(dir, { recursive: true, force: true }));

function withClock(fn) {
  const real = performance.now;
  const clock = { now: 0 };
  performance.now = () => clock.now;
  try { fn(clock); } finally { performance.now = real; }
}

test('a sub-resolution loopback round trip still counts as a measured sample', () => withClock(clock => {
  clock.now = 1000; // whole milliseconds only
  const sync = new ClockSync();
  for (let i = 0; i < 6; i++) {
    const ping = sync.createPing();
    const host = 5_000_000 + i * 1_000_000;
    sync.handlePong({ t0: ping.t0, t1: host, t2: host }); // answered within the same tick: rtt 0
    clock.now += 1000;
  }
  const fit = sync.getFit();
  assert.ok(fit.rttP10Ms > 0, `rttP10Ms must be > 0, got ${fit.rttP10Ms}`);
  assert.ok(fit.rttP10Ms <= 0.001);
  assert.ok(Math.abs(fit.b - 1) < 1e-9);
}));

test('a single zero-rtt sample is also reported as measured', () => withClock(clock => {
  const sync = new ClockSync();
  const ping = sync.createPing();
  sync.handlePong({ t0: ping.t0, t1: 2000, t2: 2000 });
  assert.ok(sync.getFit().rttP10Ms > 0);
  clock.now = 0;
}));

test('measured round trips are reported unchanged', () => withClock(clock => {
  const sync = new ClockSync();
  const ping = sync.createPing();
  clock.now = 4;
  sync.handlePong({ t0: ping.t0, t1: 2000, t2: 2000 });
  assert.equal(sync.getFit().rttP10Ms, 4);
}));
