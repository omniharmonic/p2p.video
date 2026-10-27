import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import ts from 'typescript';

const source = await readFile(new URL('../src/lib/backoff.ts', import.meta.url), 'utf8');
const js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } }).outputText;
const { signalingRetryDelayMs, SIGNALING_RETRY_MAX_MS, SIGNALING_RETRY_JITTER } = await import(`data:text/javascript;base64,${Buffer.from(js).toString('base64')}`);

test('signaling retry starts fast and doubles', () => {
  const mid = () => 0.5;
  assert.deepEqual([0, 1, 2, 3].map(n => signalingRetryDelayMs(n, mid)), [250, 500, 1000, 2000]);
});

test('signaling retry is capped so a restarted daemon is reached within ~3.6 s [SL-P0-7]', () => {
  for (let n = 0; n < 200; n++) {
    for (const r of [0, 0.5, 1]) assert.ok(signalingRetryDelayMs(n, () => r) <= SIGNALING_RETRY_MAX_MS * (1 + SIGNALING_RETRY_JITTER));
  }
  assert.equal(signalingRetryDelayMs(50, () => 0.5), SIGNALING_RETRY_MAX_MS);
  assert.equal(signalingRetryDelayMs(Infinity, () => 0.5), 250);
  assert.equal(signalingRetryDelayMs(-3, () => 0.5), 250);
});

test('signaling retry jitter spreads simultaneous reconnects', () => {
  assert.equal(signalingRetryDelayMs(10, () => 0), 2400);
  assert.equal(signalingRetryDelayMs(10, () => 1), 3600);
  assert.equal(signalingRetryDelayMs(10, () => 7), 3600);
});
