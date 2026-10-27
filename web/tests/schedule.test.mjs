// Scheduling and calendar invites [SL-P6-1]: API client and pure helpers.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import ts from 'typescript';

const toUrl = async (rel, replace = s => s) => {
  const source = await readFile(new URL(rel, import.meta.url), 'utf8');
  const js = replace(ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } }).outputText);
  return `data:text/javascript;base64,${Buffer.from(js).toString('base64')}`;
};
const roomsUrl = await toUrl('../src/lib/rooms.ts');
const S = await import(await toUrl('../src/lib/schedule.ts', js => js.replace(/from ['"]\.\/rooms['"]/g, `from '${roomsUrl}'`)));
const { ApiError } = await import(roomsUrl);
const { ScheduleApi, toStartsAt, localParts, defaultStart, parseEmails, meetingPhase, meetingWhen, relativeStart, durationLabel, localTimeZone, DURATIONS } = S;

test('local date + time ↔ RFC 3339 UTC instant', () => {
  const iso = toStartsAt('2026-10-30', '10:05');
  assert.match(iso, /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:00Z$/, 'whole seconds, Z, no milliseconds');
  assert.equal(new Date(iso).getTime(), new Date(2026, 9, 30, 10, 5).getTime());
  assert.deepEqual(localParts(iso), { date: '2026-10-30', time: '10:05' });
  assert.equal(toStartsAt('2026-02-30', '10:00'), null, 'no Feb 30');
  assert.equal(toStartsAt('2026-10-30', '24:00'), null);
  assert.equal(toStartsAt('30/10/2026', '10:00'), null);
  assert.equal(toStartsAt('2026-10-30', ''), null);
  assert.ok(localTimeZone().length > 0);
});

test('default start is the next half hour at least 10 minutes away', () => {
  const at = (h, m) => new Date(2026, 9, 26, h, m, 17);
  const hm = d => `${d.getHours()}:${String(d.getMinutes()).padStart(2, '0')}`;
  assert.equal(hm(defaultStart(at(10, 7))), '10:30');
  assert.equal(hm(defaultStart(at(10, 25))), '11:00');
  assert.equal(hm(defaultStart(at(10, 50))), '11:00');
  assert.equal(hm(defaultStart(at(23, 55))), '0:30');
  assert.equal(defaultStart(at(10, 7)).getSeconds(), 0);
});

test('guest emails: split, dedupe, and the same strict check as the daemon', () => {
  const { emails, invalid } = parseEmails('ada@example.com, Grace@Example.org;\n<lin@example.net> ada@EXAMPLE.com  nope a@b x@example.com?bcc=y@z.com');
  assert.deepEqual(emails, ['ada@example.com', 'Grace@Example.org', 'lin@example.net']);
  assert.deepEqual(invalid, ['nope', 'a@b', 'x@example.com?bcc=y@z.com']);
  assert.deepEqual(parseEmails('   '), { emails: [], invalid: [] });
});

test('phase, relative start and the when line', () => {
  const now = new Date(2026, 9, 26, 10, 0);
  const m = (startMin, dur = 60, status = 'scheduled') => {
    const s = new Date(now.getTime() + startMin * 60_000);
    return { status, startsAt: s.toISOString(), endsAt: new Date(s.getTime() + dur * 60_000).toISOString() };
  };
  assert.equal(meetingPhase(m(120), now), 'upcoming');
  assert.equal(meetingPhase(m(15), now), 'soon');
  assert.equal(meetingPhase(m(-5), now), 'now');
  assert.equal(meetingPhase(m(-90), now), 'over');
  assert.equal(meetingPhase(m(-5, 60, 'cancelled'), now), 'cancelled');
  assert.equal(relativeStart(m(5), now), 'in 5 min');
  assert.equal(relativeStart(m(150), now), 'in 3 h');
  assert.equal(relativeStart(m(3 * 24 * 60), now), '');
  assert.equal(relativeStart(m(-10), now), 'started 10 min ago');
  assert.equal(relativeStart(m(-90), now), '');
  assert.match(meetingWhen(m(60), now), /^Today · 11:00\s?AM – 12:00\s?PM$|^Today · 11:00 – 12:00$/);
  assert.match(meetingWhen(m(24 * 60), now), /^Tomorrow · /);
  assert.match(meetingWhen(m(4 * 24 * 60), now), /^\w{3}, \w{3} \d{1,2} · /);
  assert.equal(meetingWhen({ startsAt: 'x', endsAt: 'y' }, now), '');
  assert.equal(durationLabel(45), '45 min');
  assert.equal(durationLabel(60), '1 h');
  assert.equal(durationLabel(90), '1 h 30 min');
  assert.ok(DURATIONS.includes(60));
});

test('schedule and feed requests use the documented endpoints and methods', async () => {
  const calls = [];
  const api = new ScheduleApi(async (url, init = {}) => {
    calls.push([init.method || 'GET', url, init.body ?? null]);
    return url.includes('mtg_1') && (init.method === 'DELETE') ? new Response(null, { status: 204 }) : new Response('{}', { status: 200 });
  });
  const draft = { roomId: 'room_1', startsAt: '2026-10-30T17:00:00Z', durationMin: 60, timeZone: 'UTC', guestEmails: ['ada@example.com'] };
  await api.list();
  await api.create(draft);
  await api.update('mtg_1', { title: 'Episode 12' });
  await api.cancel('mtg_1');
  assert.equal(await api.remove('mtg_1'), undefined);
  await api.feed();
  await api.rotateFeed();
  await api.revokeFeed();
  assert.deepEqual(calls.map(c => c.slice(0, 2)), [
    ['GET', '/api/v1/schedule'],
    ['POST', '/api/v1/schedule'],
    ['PATCH', '/api/v1/schedule/mtg_1'],
    ['POST', '/api/v1/schedule/mtg_1/cancel'],
    ['DELETE', '/api/v1/schedule/mtg_1'],
    ['GET', '/api/v1/calendar/feed'],
    ['POST', '/api/v1/calendar/feed/rotate'],
    ['POST', '/api/v1/calendar/feed/revoke'],
  ]);
  assert.deepEqual(JSON.parse(calls[1][2]), draft);
  assert.deepEqual(JSON.parse(calls[2][2]), { title: 'Episode 12' });
});

test('API errors carry the server message', async () => {
  const api = new ScheduleApi(async () => new Response('the room\'s guest link is off', { status: 409 }));
  await assert.rejects(api.cancel('x'), err => err instanceof ApiError && err.status === 409 && /guest link is off/.test(err.message));
});
