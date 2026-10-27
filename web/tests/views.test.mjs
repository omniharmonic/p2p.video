import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import ts from 'typescript';

const source = await readFile(new URL('../src/lib/activespeaker.ts', import.meta.url), 'utf8');
const js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } }).outputText;
const { ActiveSpeakerDetector, DEFAULT_ACTIVE_SPEAKER, remoteAudioLevels } = await import(`data:text/javascript;base64,${Buffer.from(js).toString('base64')}`);

const levels = obj => new Map(Object.entries(obj));
// Feeds samples every 100 ms for the given duration; returns the last speaker.
function feed(d, startMs, durationMs, sample) {
  let speaker = d.speaker;
  for (let t = startMs; t < startMs + durationMs; t += 100) speaker = d.update(levels(sample), t);
  return speaker;
}

test('defaults: 1.5 s hold, short dominance window, quiet threshold', () => {
  assert.equal(DEFAULT_ACTIVE_SPEAKER.holdMs, 1500);
  assert.ok(DEFAULT_ACTIVE_SPEAKER.dominanceMs < DEFAULT_ACTIVE_SPEAKER.holdMs);
});

test('first voice takes the stage immediately; silence keeps the last speaker', () => {
  const d = new ActiveSpeakerDetector();
  assert.equal(d.update(levels({ a: 0, b: 0 }), 0), null, 'nobody until someone speaks');
  assert.equal(d.update(levels({ a: 0.3, b: 0.01 }), 100), 'a');
  assert.equal(feed(d, 200, 5000, { a: 0, b: 0 }), 'a', 'silence holds the last speaker');
});

test('a blip shorter than the dominance window never switches', () => {
  const d = new ActiveSpeakerDetector();
  feed(d, 0, 3000, { a: 0.3, b: 0 });
  // b coughs for 300 ms, louder than a.
  assert.equal(feed(d, 3000, 300, { a: 0.1, b: 0.6 }), 'a');
  assert.equal(feed(d, 3300, 2000, { a: 0.3, b: 0 }), 'a');
});

test('hysteresis: the current speaker keeps the stage for 1.5 s even if someone louder starts', () => {
  const d = new ActiveSpeakerDetector();
  d.update(levels({ a: 0.3, b: 0 }), 0); // a takes the stage at t=0
  // b dominates from t=100; dominance is met at 500 ms but a's hold lasts to 1500 ms.
  assert.equal(feed(d, 100, 1300, { a: 0.05, b: 0.5 }), 'a');
  assert.equal(d.update(levels({ a: 0.05, b: 0.5 }), 1500), 'b');
});

test('a sustained new speaker switches after the dominance window once the hold expired', () => {
  const d = new ActiveSpeakerDetector();
  feed(d, 0, 4000, { a: 0.3, b: 0 }); // a has held well past 1.5 s
  assert.equal(feed(d, 4000, 400, { a: 0, b: 0.4 }), 'a', 'not yet: 400 ms dominance not complete');
  assert.equal(d.update(levels({ a: 0, b: 0.4 }), 4400), 'b');
  // Alternating loudest every 200 ms never satisfies dominance: stays b.
  let speaker;
  for (let t = 4500; t < 8000; t += 100) speaker = d.update(levels(Math.floor(t / 200) % 2 ? { a: 0.5, b: 0.4 } : { a: 0.4, b: 0.5 }), t);
  assert.equal(speaker, 'b');
});

test('a departed speaker is dropped and levels below threshold do not count', () => {
  const d = new ActiveSpeakerDetector();
  d.update(levels({ a: 0.3 }), 0);
  assert.equal(d.update(levels({ b: 0.01 }), 100), null, 'a left; b is below threshold');
  assert.equal(d.update(levels({ b: 0.2 }), 200), 'b');
  assert.equal(d.update(levels({ b: NaN, c: Infinity }), 300), 'b', 'non-finite levels ignored');
});

test('remote audio levels map inbound-rtp audio to participants via track IDs', async () => {
  const stats = [
    { type: 'inbound-rtp', kind: 'audio', trackIdentifier: 'ta', audioLevel: 0.4 },
    { type: 'inbound-rtp', kind: 'video', trackIdentifier: 'tv', audioLevel: 0.9 },
    { type: 'inbound-rtp', kind: 'audio', trackIdentifier: 'unknown', audioLevel: 0.9 },
    { type: 'outbound-rtp', kind: 'audio', trackIdentifier: 'ta', audioLevel: 1 },
  ];
  const pc = { getStats: async () => ({ forEach: fn => stats.forEach(fn) }) };
  const got = await remoteAudioLevels(pc, new Map([['ta', 'alice'], ['tb', 'bob']]));
  assert.deepEqual([...got].sort(), [['alice', 0.4], ['bob', 0]]);
});
