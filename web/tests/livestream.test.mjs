import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import ts from 'typescript';

async function load(rel) {
  const source = await readFile(new URL(rel, import.meta.url), 'utf8');
  const js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } }).outputText;
  return import(`data:text/javascript;base64,${Buffer.from(js).toString('base64')}`);
}
const { computeLayout, coverCrop, pickSpeaker, orderForLayout } = await load('../src/lib/compositor.ts');
const { pickMimeType, congestionLevel, validateServerUrl, validateDestination, destinationLine, bitrateLabel, PLATFORM_PRESETS, BUFFER_HIGH_BYTES, BUFFER_MAX_BYTES } = await load('../src/lib/livestream.ts');

const W = 1280, H = 720;
const inside = (r) => r.x >= 0 && r.y >= 0 && r.x + r.w <= W && r.y + r.h <= H && r.w > 0 && r.h > 0;
const overlap = (a, b) => a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h;
function assertNoOverlapInside(rects) {
  for (const r of rects) assert.ok(inside(r), JSON.stringify(r));
  for (let i = 0; i < rects.length; i++) for (let j = i + 1; j < rects.length; j++) assert.ok(!overlap(rects[i], rects[j]), `${i} overlaps ${j}`);
}

test('no sources or a degenerate canvas yields no tiles; one source fills the canvas in every layout', () => {
  assert.deepEqual(computeLayout(0, W, H, 'grid'), []);
  assert.deepEqual(computeLayout(3, 0, H, 'grid'), []);
  for (const mode of ['grid', 'speaker', 'side-by-side']) assert.deepEqual(computeLayout(1, W, H, mode), [{ x: 0, y: 0, w: W, h: H }]);
});

test('grid: near-square cells, equal sizes, centered short last row, no overlap', () => {
  for (let n = 2; n <= 9; n++) {
    const rects = computeLayout(n, W, H, 'grid', { gap: 8 });
    assert.equal(rects.length, n);
    assertNoOverlapInside(rects);
    const cols = Math.ceil(Math.sqrt(n));
    assert.equal(new Set(rects.map(r => r.y)).size, Math.ceil(n / cols), `rows for ${n}`);
    for (const r of rects) { assert.ok(Math.abs(r.w - rects[0].w) <= 1); assert.ok(Math.abs(r.h - rects[0].h) <= 1); }
  }
  // Two sources: two halves side by side, full height minus gaps.
  assert.deepEqual(computeLayout(2, W, H, 'grid', { gap: 8 }), [{ x: 8, y: 8, w: 628, h: 704 }, { x: 644, y: 8, w: 628, h: 704 }]);
  // Three sources: 2x2 grid; the lone tile in the last row is horizontally centered.
  const three = computeLayout(3, W, H, 'grid', { gap: 8 });
  assert.equal(three[2].y, three[0].y + three[0].h + 8);
  assert.ok(Math.abs(three[2].x + three[2].w / 2 - W / 2) <= 1);
});

test('speaker: stage tile dominates, filmstrip thumbnails sit below it, centered', () => {
  for (let n = 2; n <= 6; n++) {
    const [stage, ...thumbs] = computeLayout(n, W, H, 'speaker', { gap: 8 });
    assert.equal(thumbs.length, n - 1);
    assertNoOverlapInside([stage, ...thumbs]);
    for (const t of thumbs) { assert.ok(t.y >= stage.y + stage.h); assert.ok(t.w * t.h < stage.w * stage.h / 4); assert.ok(t.w / t.h <= 16 / 9 + 0.02); }
    const left = thumbs[0].x, right = thumbs.at(-1).x + thumbs.at(-1).w;
    assert.ok(Math.abs((left + right) / 2 - W / 2) <= 1);
  }
});

test('side-by-side: one row of 16:9 tiles, vertically centered', () => {
  for (let n = 2; n <= 5; n++) {
    const rects = computeLayout(n, W, H, 'side-by-side', { gap: 8 });
    assertNoOverlapInside(rects);
    assert.equal(new Set(rects.map(r => r.y)).size, 1);
    for (const r of rects) assert.ok(Math.abs(r.w / r.h - 16 / 9) < 0.02);
    assert.ok(Math.abs(rects[0].y + rects[0].h / 2 - H / 2) <= 1);
  }
});

test('cover crop preserves the destination aspect and centers the crop', () => {
  const crop = coverCrop(640, 480, { x: 0, y: 0, w: 1280, h: 720 });
  assert.ok(Math.abs(crop.w / crop.h - 16 / 9) < 1e-9);
  assert.equal(crop.w, 640);
  assert.ok(Math.abs(crop.y - (480 - crop.h) / 2) < 1e-9);
  const portrait = coverCrop(1080, 1920, { x: 0, y: 0, w: 628, h: 704 });
  assert.ok(Math.abs(portrait.w / portrait.h - 628 / 704) < 1e-9);
  assert.deepEqual(coverCrop(0, 0, { x: 0, y: 0, w: 1, h: 1 }), { x: 0, y: 0, w: 0, h: 0 });
});

test('active speaker switches to a louder source only after the hold time', () => {
  const levels = new Map([['a', 0.01], ['b', 0.2]]);
  assert.equal(pickSpeaker(levels, null, 0, 0), 'b');
  assert.equal(pickSpeaker(levels, 'a', 0, 1000), 'a', 'held for less than 1.5s');
  assert.equal(pickSpeaker(levels, 'a', 0, 2000), 'b');
  assert.equal(pickSpeaker(new Map([['a', 0], ['b', 0]]), 'b', 0, 9999), 'b', 'silence keeps the current speaker');
  assert.equal(pickSpeaker(new Map([['a', 0]]), 'gone', 0, 0), 'a', 'departed speaker is replaced');
  assert.equal(pickSpeaker(new Map(), null, 0, 0), null);
  const srcs = [{ participantId: 'a' }, { participantId: 'b' }, { participantId: 'c' }];
  assert.deepEqual(orderForLayout(srcs, 'speaker', 'c').map(s => s.participantId), ['c', 'a', 'b']);
  assert.deepEqual(orderForLayout(srcs, 'grid', 'c').map(s => s.participantId), ['a', 'b', 'c']);
});

test('MediaRecorder type preference: low-latency WebM first, MP4 only as a fallback', () => {
  assert.equal(pickMimeType(() => true), 'video/webm;codecs=h264,opus');
  assert.equal(pickMimeType(t => t.startsWith('video/webm;codecs=vp8') || t.startsWith('video/mp4')), 'video/webm;codecs=vp8,opus');
  assert.match(pickMimeType(t => t.startsWith('video/mp4')), /^video\/mp4;codecs=avc1/);
  assert.equal(pickMimeType(() => { throw new Error('unsupported'); }), null);
  assert.equal(pickMimeType(() => false), null);
});

test('backpressure: congested above the high mark, overflow (stop, never buffer unboundedly) at the max', () => {
  assert.equal(congestionLevel(0), 'ok');
  assert.equal(congestionLevel(BUFFER_HIGH_BYTES - 1), 'ok');
  assert.equal(congestionLevel(BUFFER_HIGH_BYTES), 'congested');
  assert.equal(congestionLevel(BUFFER_MAX_BYTES), 'overflow');
  assert.ok(BUFFER_MAX_BYTES > BUFFER_HIGH_BYTES);
});

test('platform presets default to RTMPS; Facebook and LinkedIn require it [SL-P3-3]', () => {
  assert.equal(PLATFORM_PRESETS.youtube.url, 'rtmps://a.rtmps.youtube.com:443/live2');
  assert.equal(PLATFORM_PRESETS.twitch.url, 'rtmps://ingest.global-contribute.live-video.net/app');
  assert.equal(PLATFORM_PRESETS.facebook.url, 'rtmps://live-api-s.facebook.com:443/rtmp/');
  assert.equal(PLATFORM_PRESETS.linkedin.url, '', 'LinkedIn issues a URL per event');
  for (const [p, preset] of Object.entries(PLATFORM_PRESETS)) {
    if (preset.url) { assert.ok(preset.url.startsWith('rtmps://'), p); assert.equal(validateServerUrl(preset.url, p), null, p); }
    assert.ok(preset.hint.length > 10, p);
  }
  assert.ok(PLATFORM_PRESETS.facebook.requireTls && PLATFORM_PRESETS.linkedin.requireTls);
  assert.match(validateServerUrl('rtmp://live-api-s.facebook.com/rtmp/', 'facebook'), /only accepts rtmps/);
  assert.match(validateServerUrl('', 'linkedin'), /LinkedIn/);
  assert.equal(validateServerUrl('rtmps://1a2b.channel.media.azure.net:2935/live/abc', 'linkedin'), null);
  assert.equal(validateServerUrl('rtmp://a.rtmp.youtube.com/live2', 'youtube'), null, 'plain RTMP stays allowed where the platform offers it');
  assert.ok(validateServerUrl('https://example.com/live'));
  assert.ok(validateServerUrl('not a url'));
  assert.ok(validateServerUrl('rtmp://h/app with space'));
  assert.ok(validateServerUrl('rtmp://h'), 'needs an application path');
});

test('destination validation mirrors the daemon (keys, inline keys) [SL-P3-3]', () => {
  assert.equal(validateDestination('youtube', PLATFORM_PRESETS.youtube.url, 'abcd-efgh-ijkl'), null);
  assert.match(validateDestination('youtube', PLATFORM_PRESETS.youtube.url, ''), /stream key from YouTube/);
  assert.match(validateDestination('twitch', PLATFORM_PRESETS.twitch.url, 'live key'), /not allowed/);
  // Facebook keys carry a query string.
  assert.equal(validateDestination('facebook', PLATFORM_PRESETS.facebook.url, 'FB-123-0-Ab?s_bl=1&s_sw=0&a=Xy'), null);
  assert.equal(validateDestination('custom', 'rtmp://owncast.local/live/inline-key', ''), null);
  assert.match(validateDestination('custom', 'rtmp://owncast.local/live', ''), /stream key/);
  assert.equal(validateDestination('custom', 'rtmp://owncast.local/live', 'k'), null);
});

test('destination line: state, rate, retry countdown and redacted reason [SL-P3-3]', () => {
  assert.equal(destinationLine({ name: 'YouTube', server: 'rtmps://a.rtmps.youtube.com:443', state: 'live', kbps: 3120 }), 'Live · 3.1 Mbps');
  assert.equal(destinationLine({ name: 'T', server: 's', state: 'reconnecting', attempt: 2, maxAttempts: 8, retryInSec: 3.2, error: 'connect: connection refused' }),
    'Reconnecting · attempt 2 of 8 · retry in 4 s · connect: connection refused');
  assert.equal(destinationLine({ name: 'T', server: 's', state: 'failed', error: 'server rejected the stream: NetStream.Publish.BadName' }), 'Failed · server rejected the stream: NetStream.Publish.BadName');
  assert.equal(destinationLine({ name: 'T', server: 's', state: 'live', kbps: 900, behind: true }), 'Live · 900 kbps · skipping frames');
  assert.equal(destinationLine({ name: 'T', server: 's', state: 'live', kbps: 900, backlogMs: 2100 }), 'Live · 900 kbps · 2.1 s behind');
  assert.equal(destinationLine({ name: 'T', server: 's', state: 'pending' }), 'Waiting for video');
});

test('bitrate label falls back to the ingest rate and names its source [SL-P3-2]', () => {
  assert.deepEqual(bitrateLabel(null), { value: '—', source: '' });
  assert.deepEqual(bitrateLabel({ ingestKbps: 0, bitrateKbps: 0, bitrateSource: '' }), { value: '—', source: '' });
  assert.deepEqual(bitrateLabel({ ingestKbps: 1830.4, bitrateKbps: 1830.4, bitrateSource: 'ingest' }), { value: '1.8 Mbps', source: 'ingest' });
  assert.deepEqual(bitrateLabel({ ingestKbps: 900, bitrateKbps: 640, bitrateSource: 'output' }), { value: '640 kbps', source: 'output' });
  // An older daemon without bitrateKbps: the ingest rate still shows.
  assert.deepEqual(bitrateLabel({ ingestKbps: 750 }), { value: '750 kbps', source: 'ingest' });
});
