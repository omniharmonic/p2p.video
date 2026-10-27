import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import ts from 'typescript';

const source = await readFile(new URL('../src/lib/rooms.ts', import.meta.url), 'utf8');
const js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } }).outputText;
const rooms = await import(`data:text/javascript;base64,${Buffer.from(js).toString('base64')}`);
const { entryMode, formatDuration, formatWhen, hostLabel, takesSummary, takeStatusLabel, redactedLink, RoomsApi, ApiError } = rooms;

test('entry mode: host home, room quick-open, room guest, explicit session', () => {
  assert.deepEqual(entryMode('/', ''), { kind: 'host-home', roomId: undefined });
  assert.deepEqual(entryMode('/', '?room=room_abc'), { kind: 'host-home', roomId: 'room_abc' });
  assert.deepEqual(entryMode('/join', '?room=the-podcast&token=s3cret'), { kind: 'guest-room', slug: 'the-podcast', token: 's3cret' });
  assert.deepEqual(entryMode('/join', '?session=a1&token=t'), { kind: 'session' });
  assert.deepEqual(entryMode('/', '?session=a1&token=t'), { kind: 'session' }, 'a host rejoin link never shows the home');
  assert.deepEqual(entryMode('/join', '?room=x'), { kind: 'session' }, 'a room link without its secret is not a room entry');
});

test('copy helpers', () => {
  assert.equal(hostLabel(' Ben '), 'Ben');
  assert.equal(hostLabel(''), 'the host');
  assert.equal(formatDuration('2026-10-25T10:00:00Z'), '', 'open meeting has no duration');
  assert.equal(formatDuration('2026-10-25T10:00:00Z', '2026-10-25T10:42:10Z'), '42 min');
  assert.equal(formatDuration('2026-10-25T10:00:00Z', '2026-10-25T11:05:00Z'), '1 h 05 min');
  assert.equal(formatDuration('2026-10-25T10:00:00Z', '2026-10-25T09:00:00Z'), '');
  const now = new Date(2026, 9, 25, 18, 0);
  assert.match(formatWhen(new Date(2026, 9, 25, 14, 5).toISOString(), now), /^Today 14:05$/);
  assert.match(formatWhen(new Date(2026, 9, 24, 9, 30).toISOString(), now), /^Yesterday 09:30$/);
  assert.equal(formatWhen('nope', now), '');
  assert.equal(takesSummary([]), 'No recording');
  assert.equal(takesSummary([{ id: 'a', number: 1, status: 'complete' }, { id: 'b', number: 2, status: 'stopped' }]), '2 takes · 1 exported');
  assert.equal(takesSummary([{ id: 'a', number: 1, status: 'stopped' }]), '1 take · uploading');
  assert.equal(takeStatusLabel('failed'), 'Needs attention');
  assert.equal(redactedLink({ slug: 'the-podcast' }), '/join?room=the-podcast', 'display link never carries the secret');
});

test('lobby sends the room secret as a bearer header, never in the URL', async () => {
  const calls = [];
  const api = new RoomsApi(async (url, init) => { calls.push({ url, init }); return new Response(JSON.stringify({ name: 'The Podcast', live: false }), { status: 200 }); });
  const lobby = await api.lobby('the-podcast', 'secret-123');
  assert.equal(lobby.name, 'The Podcast');
  assert.equal(calls[0].url, '/api/v1/rooms/lobby?room=the-podcast');
  assert.equal(calls[0].init.headers.Authorization, 'Bearer secret-123');
  assert.ok(!calls[0].url.includes('secret-123'));
});

test('API errors carry status and server message; 204 resolves empty', async () => {
  const api = new RoomsApi(async (url, init) => url.endsWith('/end')
    ? new Response('stop recording before ending the meeting', { status: 409 })
    : new Response(null, { status: 204 }));
  await assert.rejects(api.end('s1', 'h'), err => err instanceof ApiError && err.status === 409 && /stop recording/.test(err.message));
  assert.equal(await api.remove('room_1'), undefined);
});

test('room requests use the documented endpoints and methods', async () => {
  const calls = [];
  const api = new RoomsApi(async (url, init = {}) => { calls.push([init.method || 'GET', url, init.body]); return new Response('{}', { status: 200 }); });
  await api.create('The Podcast');
  await api.update('room_1', { name: 'Show' });
  await api.rotate('room_1');
  await api.revoke('room_1');
  await api.producerRotate('room_1');
  await api.start('room_1', 'Ben');
  await api.home();
  assert.deepEqual(calls, [
    ['POST', '/api/v1/rooms', '{"name":"The Podcast"}'],
    ['PATCH', '/api/v1/rooms/room_1', '{"name":"Show"}'],
    ['POST', '/api/v1/rooms/room_1/rotate', undefined],
    ['POST', '/api/v1/rooms/room_1/revoke', undefined],
    ['POST', '/api/v1/rooms/room_1/producer/rotate', undefined],
    ['POST', '/api/v1/rooms/room_1/start', '{"hostName":"Ben"}'],
    ['GET', '/api/v1/home', undefined],
  ]);
});

test('session_end fixture matches the TypeScript protocol mirror', async () => {
  const fixture = JSON.parse(await readFile(new URL('../../testdata/protocol/session-end.json', import.meta.url), 'utf8'));
  const protocolTs = await readFile(new URL('../src/lib/protocol.ts', import.meta.url), 'utf8');
  assert.equal(fixture.t, 'session_end');
  assert.match(protocolTs, /TypeSessionEnd = "session_end"/);
  assert.match(protocolTs, /interface SessionEndPayload \{\s*reason: "ended_by_host";/);
  assert.equal(fixture.payload.reason, 'ended_by_host');
});
