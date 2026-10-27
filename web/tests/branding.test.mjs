// Branding and lower thirds [SL-P5-2]: text defaults, colour helpers, layout
// and animation math, the speaker-driven director, and the fixtures.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import ts from 'typescript';

async function load(rel) {
  const source = await readFile(new URL(rel, import.meta.url), 'utf8');
  const js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } }).outputText;
  return import(`data:text/javascript;base64,${Buffer.from(js).toString('base64')}`);
}
const B = await load('../src/lib/branding.ts');
const proto = await load('../src/lib/protocol.ts');

const inside = (r, outer) => r.x >= outer.x && r.y >= outer.y && r.x + r.w <= outer.x + outer.w && r.y + r.h <= outer.y + outer.h;

test('lower-third text: override, else display name; title optional', () => {
  const brand = { ...B.emptyBrand(), lowerThirds: [{ participantId: 'a', name: 'Ana Ruiz', title: 'Founder, X' }, { participantId: 'b', title: 'Host' }] };
  assert.deepEqual(B.lowerThirdText(brand, 'a', 'ana'), { name: 'Ana Ruiz', title: 'Founder, X' });
  assert.deepEqual(B.lowerThirdText(brand, 'b', 'Ben'), { name: 'Ben', title: 'Host' });
  assert.deepEqual(B.lowerThirdText(brand, 'c', 'Cy'), { name: 'Cy', title: '' });
  // The store resolves the local tile to the participant's own id and sanitizes the accent.
  B.brandStore.selfId = 'b';
  B.brandStore.set({ ...brand, accent: 'javascript:alert(1)' });
  assert.equal(B.brandStore.get().accent, B.DEFAULT_ACCENT);
  assert.deepEqual(B.brandStore.text('local-tile', 'Me'), { name: 'Me', title: 'Host' });
  let seen = null;
  const off = B.brandStore.subscribe(s => { seen = s.showName; });
  B.brandStore.set({ ...brand, showName: 'Show', accent: '#E07A5F' });
  assert.equal(seen, 'Show');
  assert.equal(B.brandStore.get().accent, '#e07a5f');
  off();
});

test('colour helpers: accent validation, readable text, mixing', () => {
  assert.ok(B.validAccent('#a0b1c2') && !B.validAccent('#abc') && !B.validAccent('red') && !B.validAccent(null));
  assert.equal(B.textOn('#ffffff'), '#0b0f13');
  assert.equal(B.textOn('#bfdbe8'), '#0b0f13');
  assert.equal(B.textOn('#1a237e'), '#ffffff');
  assert.equal(B.textOn('#000000'), '#ffffff');
  assert.equal(B.mixColor('#000000', '#ffffff', 0.5), '#808080');
  assert.equal(B.mixColor('#102030', '#ffffff', 0), '#102030');
  assert.equal(B.mixColor('#102030', '#ffffff', 2), '#ffffff');
});

test('branded frame: a top band and an inset stage; unbranded uses the canvas', () => {
  assert.deepEqual(B.brandedFrame(1280, 720, false), { band: null, stage: { x: 0, y: 0, w: 1280, h: 720 } });
  const { band, stage } = B.brandedFrame(1280, 720, true);
  assert.deepEqual(band, { x: 0, y: 0, w: 1280, h: 72 });
  assert.ok(stage.y >= band.h && inside(stage, { x: 0, y: 0, w: 1280, h: 720 }));
  assert.ok(stage.w < 1280 && stage.h < 720 - band.h);
});

test('logo fits the top-right corner with its aspect ratio, inside the band when branded', () => {
  const W = 1280, H = 720;
  const wide = B.logoRect(W, H, 1000, 200, null);
  assert.ok(wide.w <= W * 0.14 + 1 && wide.h <= H * 0.09 + 1);
  assert.ok(Math.abs(wide.w / wide.h - 5) < 0.1, 'aspect kept');
  assert.equal(wide.x + wide.w, W - Math.round(H * 0.025), 'right margin');
  const { band } = B.brandedFrame(W, H, true);
  const square = B.logoRect(W, H, 512, 512, band);
  assert.ok(inside(square, band), JSON.stringify(square));
  assert.equal(square.w, square.h);
  assert.deepEqual(B.logoRect(W, H, 0, 10, null), { x: 0, y: 0, w: 0, h: 0 });
});

test('lower-third layout: bottom-left of its tile, sized by text, capped, slides in', () => {
  const tile = { x: 640, y: 0, w: 640, h: 360 };
  const m = B.lowerThirdMetrics(tile);
  assert.ok(m.nameFont >= 14 && m.nameFont <= 40 && m.titleFont < m.nameFont);
  const full = B.lowerThirdLayout(tile, m, 200, 150, true, 1);
  assert.ok(inside(full.box, tile), JSON.stringify(full.box));
  assert.equal(full.box.x, tile.x + m.margin);
  assert.equal(full.box.y + full.box.h, tile.y + tile.h - m.margin);
  assert.equal(full.alpha, 1);
  assert.ok(full.titleY > full.nameY && full.nameX > full.strip.x + full.strip.w - 1);
  assert.equal(full.strip.h, full.box.h);
  // Very long text is capped at 80% of the tile.
  const long = B.lowerThirdLayout(tile, m, 5000, 10, true, 1);
  assert.ok(long.box.w <= tile.w * 0.8 + 1);
  // No title: a shorter box.
  assert.ok(B.lowerThirdLayout(tile, m, 200, 0, false, 1).box.h < full.box.h);
  // Animating in: further left and transparent at 0.
  const start = B.lowerThirdLayout(tile, m, 200, 150, true, 0);
  assert.ok(start.box.x < full.box.x && start.alpha === 0);
  // Fonts scale with the tile, within bounds.
  assert.ok(B.lowerThirdMetrics({ x: 0, y: 0, w: 1920, h: 1080 }).nameFont <= 40);
  assert.ok(B.lowerThirdMetrics({ x: 0, y: 0, w: 160, h: 90 }).nameFont >= 14);
});

test('lower-third animation: ease in, hold, ease out, done', () => {
  assert.equal(B.lowerThirdProgress(1000, 1000), 0);
  assert.equal(B.lowerThirdProgress(1000, 500), 0);
  const mid = B.lowerThirdProgress(1000, 1000 + B.LT_IN_MS / 2);
  assert.ok(mid > 0.5 && mid < 1, 'ease-out is past half at half time');
  assert.equal(B.lowerThirdProgress(1000, 1000 + B.LT_IN_MS + 10), 1);
  const out = B.lowerThirdProgress(1000, 1000 + B.LT_IN_MS + B.LT_HOLD_MS + B.LT_OUT_MS / 2);
  assert.ok(out > 0 && out < 0.5);
  assert.equal(B.lowerThirdProgress(1000, 1000 + B.LT_IN_MS + B.LT_HOLD_MS + B.LT_OUT_MS), null);
});

test('director: new speaker shows theirs, cooldown, cue on demand, manual and off modes', () => {
  const d = new B.LowerThirdDirector();
  assert.equal(d.update(null, 'speaker', 0), null);
  assert.equal(d.update('a', 'speaker', 100).participantId, 'a');
  // b takes over: b's lower third replaces a's.
  assert.equal(d.update('b', 'speaker', 2000).participantId, 'b');
  // a again within the cooldown: b keeps going, a is not re-shown.
  assert.equal(d.update('a', 'speaker', 3000).participantId, 'b');
  const done = 2000 + B.LT_IN_MS + B.LT_HOLD_MS + B.LT_OUT_MS + 1;
  assert.equal(d.update('a', 'speaker', done), null);
  // After the cooldown a new turn shows a again.
  d.update('b', 'speaker', 100 + B.LT_COOLDOWN_MS);
  assert.equal(d.update('a', 'speaker', 200 + B.LT_COOLDOWN_MS).participantId, 'a');
  // Manual: speakers don't trigger, a cue does.
  const m = new B.LowerThirdDirector();
  assert.equal(m.update('a', 'manual', 0), null);
  m.cue('c', 10);
  assert.equal(m.update('a', 'manual', 20).participantId, 'c');
  // Off clears whatever is on air.
  assert.equal(m.update('a', 'off', 30), null);
  assert.equal(m.current, null);
});

test('branding fixtures match the TypeScript mirror', async () => {
  const fx = name => readFile(new URL(`../../testdata/protocol/${name}`, import.meta.url), 'utf8').then(JSON.parse);
  const b = await fx('branding.json');
  assert.equal(b.t, proto.TypeBranding);
  assert.ok(B.validAccent(b.payload.accent));
  assert.ok(['speaker', 'manual', 'off'].includes(b.payload.lowerThirdsMode));
  assert.deepEqual(Object.keys(b.payload.lowerThirds[0]).sort(), ['name', 'participantId', 'title']);
  assert.equal(typeof b.payload.rememberTitles, 'boolean'); // room meetings remember titles [SL-P5-4]
  const s = new (B.brandStore.constructor)();
  s.set(b.payload);
  assert.equal(s.get().rememberTitles, true);
  assert.equal((await fx('lower-third.json')).t, proto.TypeLowerThird);
  assert.equal(proto.DefaultBrandAccent, B.DEFAULT_ACCENT);
  assert.equal(proto.BrandLogoMaxBytes, 1048576);
});
