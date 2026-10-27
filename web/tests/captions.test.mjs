// Live captions [SL-P4-2]: caption board state, frame encoding shared with the
// Go daemon and Swift provider, the `c` shortcut and the worklet downsampler.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import vm from 'node:vm';
import ts from 'typescript';

async function load(rel) {
  const source = await readFile(new URL(rel, import.meta.url), 'utf8');
  const js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } }).outputText;
  return import(`data:text/javascript;base64,${Buffer.from(js).toString('base64')}`);
}
const { CaptionBoard, tailText, encodeCaptionFrame, isDigitalSilence, readCaptionsVisible, writeCaptionsVisible, readCaptionLanguage, CAPTIONS_VISIBLE_KEY } = await load('../src/lib/captions.ts');
const { matchShortcut, shortcutLabels } = await load('../src/lib/shortcuts.ts');
const protocol = await load('../src/lib/protocol.ts');

let seq = 0;
const cap = (id, pid, text, isFinal = false, name = pid.toUpperCase()) => ({ id, seq: ++seq, participantId: pid, name, text, isFinal, startMs: 0, endMs: 0 });
const texts = b => b.lines().map(l => `${l.name}:${l.text}${l.isFinal ? '' : '…'}`);

test('a partial is replaced in place by later partials and by its final', () => {
  const b = new CaptionBoard();
  assert.equal(b.apply(cap('s1.1', 'a', 'Welcome'), 0), true);
  assert.equal(b.apply(cap('s2.1', 'b', 'Hi'), 0), true);
  b.apply(cap('s1.1', 'a', 'Welcome to the'), 10);
  b.apply(cap('s1.1', 'a', 'Welcome to the show.', true), 20);
  assert.deepEqual(texts(b), ['A:Welcome to the show.', 'B:Hi…'], 'final keeps its position; other speakers unaffected');
  b.apply(cap('s1.2', 'a', 'Today'), 30);
  assert.deepEqual(texts(b), ['A:Welcome to the show.', 'B:Hi…', 'A:Today…']);
});

test('stale and reopening updates are ignored; an empty final withdraws a partial', () => {
  const b = new CaptionBoard();
  const late = cap('x', 'a', 'old partial');
  const fresh = cap('x', 'a', 'newer partial');
  b.apply(fresh, 0);
  assert.equal(b.apply(late, 1), false, 'lower seq for the same line is stale');
  assert.deepEqual(texts(b), ['A:newer partial…']);
  b.apply(cap('x', 'a', 'done', true), 2);
  assert.equal(b.apply(cap('x', 'a', 'reopened'), 3), false, 'a final is never reopened');
  b.apply(cap('y', 'b', 'uh'), 4);
  assert.equal(b.apply(cap('y', 'b', '', true), 5), true);
  assert.deepEqual(texts(b), ['A:done']);
  assert.equal(b.apply(cap('z', 'b', '   ', false), 6), false, 'blank captions never create lines');
});

test('only the newest lines are kept, ordered by seq', () => {
  const b = new CaptionBoard({ maxLines: 3 });
  for (let i = 1; i <= 5; i++) b.apply(cap(`l${i}`, 'a', `line ${i}`, true), i);
  assert.deepEqual(b.lines().map(l => l.text), ['line 3', 'line 4', 'line 5']);
});

test('finals expire after their TTL, lost partials later', () => {
  const b = new CaptionBoard({ finalTtlMs: 8000, partialTtlMs: 15000 });
  b.apply(cap('f', 'a', 'final', true), 0);
  b.apply(cap('p', 'b', 'partial'), 0);
  assert.equal(b.expire(7999), false);
  assert.equal(b.expire(8000), true);
  assert.deepEqual(texts(b), ['B:partial…']);
  b.expire(15000);
  assert.deepEqual(b.lines(), []);
});

test('long lines show their most recent words', () => {
  const long = Array.from({ length: 60 }, (_, i) => `word${i}`).join(' ');
  const t = tailText(long, 40);
  assert.ok(t.startsWith('…'));
  assert.ok(t.length <= 41);
  assert.ok(t.endsWith('word59'));
  assert.ok(!/^…\S*\d\S* /.test(t) || t.slice(1).split(' ')[0].startsWith('word'), 'cut at a word boundary');
  assert.equal(tailText('  short   text ', 40), 'short text');
  const b = new CaptionBoard({ maxChars: 40 });
  b.apply(cap('l', 'a', long), 0);
  assert.equal(b.lines()[0].text, t);
});

test('caption audio frame matches the Go/Swift golden frame', () => {
  const frame = new Uint8Array(encodeCaptionFrame('s1', Int16Array.from([0, 1, -1, 32767, -32768])));
  assert.equal(Buffer.from(frame).toString('hex'), '0102733100000100ffffff7f0080');
  const pid = 'p_8a1f0c2e-3b4d-4e5f-9a6b-7c8d9e0f1a2b';
  const f2 = new Uint8Array(encodeCaptionFrame(pid, new Int16Array(1600)));
  assert.equal(f2[1], pid.length);
  assert.equal(f2.length, 2 + pid.length + 3200);
  assert.throws(() => encodeCaptionFrame('bad/key', new Int16Array(1)));
  assert.throws(() => encodeCaptionFrame('', new Int16Array(1)));
  assert.equal(isDigitalSilence(new Int16Array(1600)), true);
  assert.equal(isDigitalSilence(Int16Array.from([0, 0, 1])), false);
});

test('viewer preference and host language persist, with safe defaults', () => {
  const store = new Map();
  const storage = { getItem: k => store.get(k) ?? null, setItem: (k, v) => store.set(k, v) };
  assert.equal(readCaptionsVisible(storage), true, 'captions show by default when the host turns them on');
  writeCaptionsVisible(storage, false);
  assert.equal(store.get(CAPTIONS_VISIBLE_KEY), '0');
  assert.equal(readCaptionsVisible(storage), false);
  const throwing = { getItem() { throw new Error('denied'); }, setItem() { throw new Error('denied'); } };
  assert.equal(readCaptionsVisible(throwing), true);
  assert.doesNotThrow(() => writeCaptionsVisible(throwing, true));
  assert.equal(readCaptionLanguage(storage), 'en-US');
  store.set('studiolink.captions.language', 'fr-FR');
  assert.equal(readCaptionLanguage(storage), 'fr-FR');
  store.set('studiolink.captions.language', '<script>');
  assert.equal(readCaptionLanguage(storage), 'en-US');
});

test('`c` toggles captions, but not while typing or with modifiers', () => {
  const key = (k, mods = {}) => ({ key: k, code: `Key${k.toUpperCase()}`, metaKey: false, ctrlKey: false, altKey: false, shiftKey: false, ...mods });
  assert.equal(matchShortcut(key('c'), true), 'toggle-captions');
  assert.equal(matchShortcut(key('c'), false), 'toggle-captions');
  assert.equal(matchShortcut(key('c', { metaKey: true }), true), null, '⌘C stays copy');
  assert.equal(matchShortcut(key('c', { ctrlKey: true }), false), null, 'Ctrl+C stays copy');
  assert.equal(matchShortcut(key('c', { shiftKey: true }), true), null);
  assert.equal(matchShortcut(key('c'), true, { tagName: 'TEXTAREA' }), null);
  assert.equal(matchShortcut(key('c'), true, { tagName: 'INPUT', type: 'text' }), null);
  assert.deepEqual(shortcutLabels(true).find(s => s.action === 'toggle-captions').keys, ['C']);
});

test('protocol mirrors the Go caption block', async () => {
  assert.equal(protocol.TypeCaption, 'caption');
  assert.equal(protocol.TypeCaptionState, 'caption_state');
  assert.equal(protocol.TypeCaptionControl, 'caption_control');
  assert.equal(protocol.CaptionMaxRunes, 500);
  const fixture = JSON.parse(await readFile(new URL('../../testdata/protocol/caption.json', import.meta.url), 'utf8'));
  assert.deepEqual(Object.keys(fixture.payload).sort(), ['endMs', 'id', 'isFinal', 'name', 'participantId', 'seq', 'startMs', 'text']);
  const b = new CaptionBoard();
  assert.equal(b.apply(fixture.payload, 0), true);
});

test('worklet downsamples 48 kHz stereo to 100 ms frames of 16 kHz mono Int16', async () => {
  const source = await readFile(new URL('../src/lib/caption-worklet.js', import.meta.url), 'utf8');
  const posted = [];
  let Processor;
  const context = {
    sampleRate: 48000,
    AudioWorkletProcessor: class { constructor() { this.port = { postMessage: m => posted.push(m), onmessage: null }; } },
    registerProcessor: (name, cls) => { assert.equal(name, 'studiolink-caption-tap'); Processor = cls; },
    Int16Array, Math,
  };
  vm.runInNewContext(source, context);
  const p = new Processor();
  // 1 kHz tone at half scale, left and right identical; 128-sample render quanta.
  let n = 0;
  for (let q = 0; q < 48000 * 0.25 / 128; q++) {
    const l = new Float32Array(128);
    for (let i = 0; i < 128; i++, n++) l[i] = 0.5 * Math.sin(2 * Math.PI * 1000 * n / 48000);
    assert.equal(p.process([[l, l.slice()]]), true);
  }
  assert.equal(posted.length, 2, '250 ms → two full 100 ms frames');
  for (const f of posted) {
    assert.equal(f.length, 1600);
    const peak = Math.max(...f.map(Math.abs));
    assert.ok(peak > 12000 && peak < 16500, `level preserved: ${peak}`);
  }
  // Silence stays exactly zero (so the tap can skip it).
  const q = new Processor();
  for (let i = 0; i < 400; i++) q.process([[new Float32Array(128)]]);
  assert.ok(posted.slice(2).every(f => f.every(v => v === 0)));
  q.port.onmessage({ data: 'stop' });
  assert.equal(q.process([[new Float32Array(128)]]), false, 'stop ends the processor');
});
