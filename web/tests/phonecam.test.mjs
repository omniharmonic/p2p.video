// Phone camera (PRD F3) [SL-P1-9]: link building, expiry and status copy,
// the vendored QR encoder, and the protocol fixtures.
import test from 'node:test';
import assert from 'node:assert/strict';
import { build } from 'esbuild';
import { mkdtemp, rm, readFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const dir = await mkdtemp(join(tmpdir(), 'studiolink-phonecam-'));
const load = async name => {
  const out = join(dir, `${name}.mjs`);
  await build({ entryPoints: [new URL(`../src/lib/${name}.ts`, import.meta.url).pathname], bundle: true, format: 'esm', outfile: out });
  return import(out);
};
const { phoneLink, expiryText, phoneStatusText, phoneVideoConstraints, PHONE_AUDIO_CONSTRAINTS } = await load('phonecam');
const { qrMatrix, qrSvg } = await load('qr');
const { RoleSubsource } = await load('protocol');
test.after(() => rm(dir, { recursive: true, force: true }));

test('phone link: server base for the local dashboard, page origin otherwise', () => {
  const resp = { path: '/phone.html?session=s1&token=abc.def', expiresAt: 1000 };
  assert.deepEqual(phoneLink({ ...resp, baseUrl: 'https://studio.tail1.ts.net' }, 'http://127.0.0.1:7443'),
    { url: 'https://studio.tail1.ts.net/phone.html?session=s1&token=abc.def', reachable: true, expiresAt: 1000 });
  assert.equal(phoneLink(resp, 'https://studio.tail1.ts.net').url, 'https://studio.tail1.ts.net/phone.html?session=s1&token=abc.def');
  assert.equal(phoneLink(resp, 'http://localhost:7443').reachable, false, 'a phone cannot open localhost');
  assert.equal(phoneLink(resp, 'http://127.0.0.1:7443').reachable, false);
});

test('expiry countdown and status copy', () => {
  assert.equal(expiryText(10 * 60_000, 0), 'Works once · expires in 10:00');
  assert.equal(expiryText(61_500, 0), 'Works once · expires in 1:02');
  assert.match(expiryText(0, 5), /expired/);
  assert.match(phoneStatusText({ state: 'live', parentName: 'Ada' }), /Ada’s phone/);
  assert.equal(phoneStatusText({ state: 'uploading', uploadedBytes: 3, pendingBytes: 1 }), 'Uploading 75% — keep this page open.');
  assert.match(phoneStatusText({ state: 'recording' }), /Keep this page open/);
  assert.equal(phoneStatusText({ state: 'error', message: 'x' }), 'x');
});

test('phone capture: full-quality camera, raw room mic', () => {
  assert.deepEqual(phoneVideoConstraints('environment').facingMode, { ideal: 'environment' });
  assert.equal(phoneVideoConstraints('user').width.ideal, 1920);
  assert.deepEqual(PHONE_AUDIO_CONSTRAINTS, { echoCancellation: false, noiseSuppression: false, autoGainControl: false });
});

test('QR encoder: finder patterns, quiet zone, sized for a real phone link', () => {
  const url = 'https://studio.tail1234.ts.net/phone.html?session=e05db950&token=' + 'x'.repeat(175);
  const { size, modules } = qrMatrix(url);
  assert.ok(size >= 21 && (size - 17) % 4 === 0, `size ${size}`);
  // Top-left finder: 7×7 dark ring with a 3×3 core.
  const ring = [0, 6].every(i => [0, 1, 2, 3, 4, 5, 6].every(j => modules[i][j] && modules[j][i]));
  assert.ok(ring && modules[3][3] && !modules[1][1] && modules[2][2]);
  for (const [r, c] of [[0, size - 7], [size - 7, 0]]) assert.ok(modules[r][c] && modules[r + 6][c + 6]);
  const svg = qrSvg(url, 'Phone <link>');
  assert.match(svg, new RegExp(`viewBox="0 0 ${size + 8} ${size + 8}"`));
  assert.ok(svg.includes('aria-label="Phone &lt;link&gt;"'));
  assert.ok(!svg.includes('<script'));
  // Deterministic for the same text.
  assert.equal(qrSvg(url), qrSvg(url));
});

test('protocol: phone admission and roster fixtures match protocol.ts', async () => {
  const adm = JSON.parse(await readFile(new URL('../../testdata/protocol/admission-subsource.json', import.meta.url), 'utf8'));
  assert.equal(adm.payload.role, RoleSubsource);
  assert.ok(adm.payload.parentParticipantId && adm.payload.name.endsWith(' · Phone'));
  const roster = JSON.parse(await readFile(new URL('../../testdata/protocol/roster-subsource.json', import.meta.url), 'utf8'));
  const phone = roster.payload.participants.find(p => p.role === RoleSubsource);
  assert.equal(phone.parentParticipantId, roster.payload.participants[0].participantId);
});
