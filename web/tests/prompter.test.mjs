// Teleprompter model [SL-P5-1]: script parsing, scroll math, visibility rules.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import ts from 'typescript';

async function load(rel) {
  const source = await readFile(new URL(rel, import.meta.url), 'utf8');
  const js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } }).outputText;
  return import(`data:text/javascript;base64,${Buffer.from(js).toString('base64')}`);
}
const P = await load('../src/lib/prompter.ts');
const proto = await load('../src/lib/protocol.ts');

test('script parsing: headings, one gap per blank run, plain text kept verbatim', () => {
  const blocks = P.parseScript('# Cold open\r\nWelcome back.\n\n\n## Guest intro\n  Ana <b>Ruiz</b> & co  \n#nospace\n\n');
  assert.deepEqual(blocks, [
    { kind: 'h1', text: 'Cold open' },
    { kind: 'p', text: 'Welcome back.' },
    { kind: 'gap', text: '' },
    { kind: 'h2', text: 'Guest intro' },
    { kind: 'p', text: 'Ana <b>Ruiz</b> & co' },
    { kind: 'p', text: '#nospace' },
  ]);
  assert.deepEqual(P.parseScript(''), []);
  assert.deepEqual(P.parseScript('\n\n'), []);
  assert.equal(P.parseScript('### deep')[0].kind, 'h2');
});

test('font and speed clamp to their ranges', () => {
  assert.equal(P.clampFont(10), P.FONT_MIN);
  assert.equal(P.clampFont(500), P.FONT_MAX);
  assert.equal(P.clampFont(NaN), P.FONT_DEFAULT);
  assert.equal(P.clampSpeed(0), P.SPEED_MIN);
  assert.equal(P.clampSpeed(42), P.SPEED_MAX);
  assert.equal(P.clampSpeed(3.4), 3);
});

test('auto-scroll speed is lines per 10 s at any font size', () => {
  // Speed 10 is one line a second.
  assert.equal(P.pixelsPerSecond(10, 40), 40 * P.LINE_HEIGHT);
  assert.equal(P.pixelsPerSecond(5, 40), 20 * P.LINE_HEIGHT);
  // Bigger type scrolls proportionally more pixels: same lines per second.
  assert.equal(P.pixelsPerSecond(3, 80) / (80 * P.LINE_HEIGHT), P.pixelsPerSecond(3, 40) / (40 * P.LINE_HEIGHT));
});

test('advance integrates time, clamps to the end and caps a stalled frame', () => {
  const max = 1000;
  const oneSec = P.advance(0, 1000, 10, 40, max);
  // A whole second arrives as one capped step (250 ms), so a hidden tab never jumps.
  assert.equal(oneSec, 40 * P.LINE_HEIGHT * 0.25);
  let pos = 0;
  for (let i = 0; i < 60; i++) pos = P.advance(pos, 1000 / 60, 10, 40, max);
  assert.ok(Math.abs(pos - 40 * P.LINE_HEIGHT) < 1e-9, `60 frames ≈ one second: ${pos}`);
  assert.equal(P.advance(995, 250, 10, 96, max), max);
  assert.equal(P.advance(10, -50, 10, 40, max), 10);
  assert.ok(P.atEnd(max, max) && !P.atEnd(max - 2, max) && P.atEnd(0, 0));
});

test('nudge moves one reading line, inside the bounds', () => {
  assert.equal(P.nudge(100, 1, 40, 1000), 100 + 40 * P.LINE_HEIGHT);
  assert.equal(P.nudge(100, -1, 40, 1000), 100 - 40 * P.LINE_HEIGHT);
  assert.equal(P.nudge(10, -1, 40, 1000), 0);
  assert.equal(P.nudge(990, 1, 40, 1000), 1000);
});

test('position is shared as a fraction and mapped onto any reader size', () => {
  assert.equal(P.maxScroll(1500, 400), 1100);
  assert.equal(P.maxScroll(300, 400), 0);
  assert.equal(P.toFraction(550, 1100), 0.5);
  assert.equal(P.toFraction(5, 0), 0);
  assert.equal(P.toFraction(2000, 1000), 1);
  // The host at half way puts a follower with a different font size at its half way.
  assert.equal(P.fromFraction(P.toFraction(550, 1100), 3000), 1500);
  assert.equal(P.fromFraction(NaN, 3000), 0);
  assert.equal(P.fromFraction(-1, 3000), 0);
});

test('a sharing host sends on state changes at once, otherwise throttled and only when moved', () => {
  assert.equal(P.shouldShare(null, 0, 0, false, 3), true);
  const last = { at: 1000, position: 0.2, playing: true, speed: 3 };
  assert.equal(P.shouldShare(last, 1100, 0.25, true, 3), false, 'throttled');
  assert.equal(P.shouldShare(last, 1300, 0.25, true, 3), true);
  assert.equal(P.shouldShare(last, 1300, 0.2, true, 3), false, 'did not move');
  assert.equal(P.shouldShare(last, 1001, 0.2, false, 3), true, 'pause is immediate');
  assert.equal(P.shouldShare(last, 1001, 0.2, true, 4), true, 'speed is immediate');
});

test('visibility rules mirror the hub', () => {
  const matrix = { host: ['host', 'crew', 'everyone'], producer: ['crew', 'everyone'], guest: ['everyone'], '': [] };
  for (const [role, sees] of Object.entries(matrix)) {
    for (const vis of ['host', 'crew', 'everyone']) assert.equal(P.canSee(role, vis), sees.includes(vis), `${role} sees ${vis}`);
  }
  assert.ok(P.canEdit('host', 'host') && P.canEdit('producer', 'crew') && P.canEdit('producer', 'everyone'));
  assert.ok(!P.canEdit('producer', 'host') && !P.canEdit('guest', 'everyone') && !P.canEdit('', 'everyone'));
  assert.deepEqual([...proto.PrompterVisibilities], ['host', 'crew', 'everyone']);
  assert.equal(P.utf8Bytes('é'), 2);
});

test('protocol fixtures match the TypeScript mirror', async () => {
  const fx = name => readFile(new URL(`../../testdata/protocol/${name}`, import.meta.url), 'utf8').then(JSON.parse);
  const state = await fx('prompter.json');
  assert.equal(state.t, proto.TypePrompter);
  assert.ok(proto.PrompterVisibilities.includes(state.payload.visibility));
  assert.deepEqual(Object.keys(state.payload.scroll).sort(), ['playing', 'position', 'shared', 'speed']);
  const hidden = await fx('prompter-hidden.json');
  assert.equal(hidden.payload.hidden, true);
  assert.equal(hidden.payload.text, '');
  assert.equal((await fx('prompter-edit.json')).t, proto.TypePrompterEdit);
  assert.equal((await fx('prompter-scroll.json')).t, proto.TypePrompterScroll);
  assert.equal(proto.PrompterMaxBytes, 65536);
});
