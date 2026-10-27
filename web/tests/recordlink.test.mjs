// Solo recorder state machine and record-link helpers [SL-P5-3].
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import ts from 'typescript';

async function load(rel) {
  const source = await readFile(new URL(rel, import.meta.url), 'utf8');
  const js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } }).outputText;
  return import(`data:text/javascript;base64,${Buffer.from(js).toString('base64')}`);
}
const R = await load('../src/lib/recordlink.ts');
const { soloReduce, INITIAL_SOLO_STATE, canRecord, canSubmit, canDiscard, uploadSummary } = R;

const run = (events, from = INITIAL_SOLO_STATE) => events.reduce((s, e) => soloReduce(s, e), from);
const take = (id, n) => ({ type: 'started', take: { takeId: id, number: n } });

test('a guest records, re-records and submits', () => {
  let s = run([{ type: 'link-ok', accepting: true }]);
  assert.equal(s.phase, 'setup');
  assert.equal(canSubmit(s), false, 'nothing to submit yet');
  assert.equal(canRecord(s), true);
  s = run([{ type: 'record' }, take('t1', 1)], s);
  assert.equal(s.phase, 'recording');
  assert.equal(s.activeTakeId, 't1');
  assert.equal(canRecord(s), false, 'one take at a time');
  assert.equal(canSubmit(s), false, 'no submit while recording');
  assert.equal(canDiscard(s, 't1'), false, 'the running take cannot be deleted');
  s = run([{ type: 'stop' }], s);
  assert.equal(s.phase, 'stopping');
  s = run([{ type: 'stopped', takeId: 't1', durationSec: 20.2 }], s);
  assert.equal(s.phase, 'review');
  assert.equal(s.takes[0].durationSec, 20.2);
  assert.equal(s.activeTakeId, undefined);
  assert.equal(canSubmit(s), true);
  // Re-record: delete take 1, record take 2.
  assert.equal(canDiscard(s, 't1'), true);
  s = run([{ type: 'discarded', takeId: 't1' }], s);
  assert.equal(s.phase, 'setup', 'no take left');
  s = run([{ type: 'record' }, take('t2', 2), { type: 'stop' }, { type: 'stopped', takeId: 't2', durationSec: 20 }], s);
  assert.deepEqual(s.takes.map(t => t.number), [2]);
  // Uploads continue after submit.
  s = run([{ type: 'progress', takeId: 't2', uploadedBytes: 100, pendingBytes: 300, uploaded: false }], s);
  assert.deepEqual(uploadSummary(s.takes), { pendingBytes: 300, uploadedBytes: 100, allUploaded: false });
  s = run([{ type: 'submit' }], s);
  assert.equal(s.phase, 'submitting');
  s = run([{ type: 'submitted' }], s);
  assert.equal(s.phase, 'submitted');
  // Frozen once sent.
  assert.equal(canRecord(s), false);
  assert.equal(canSubmit(s), false);
  assert.equal(canDiscard(s, 't2'), false);
  assert.equal(run([{ type: 'record' }], s).phase, 'submitted');
  assert.equal(run([{ type: 'discarded', takeId: 't2' }], s).takes.length, 1);
  s = run([{ type: 'progress', takeId: 't2', uploadedBytes: 400, pendingBytes: 0, uploaded: true }], s);
  assert.equal(uploadSummary(s.takes).allUploaded, true);
});

test('countdown can be cancelled and start failures return to where the guest was', () => {
  let s = run([{ type: 'link-ok', accepting: true }, { type: 'record' }]);
  assert.equal(s.phase, 'countdown');
  s = run([{ type: 'stop' }], s);
  assert.equal(s.phase, 'setup');
  // An event for a phase that has passed is ignored.
  assert.equal(run([take('late', 1)], s).phase, 'setup');
  s = run([{ type: 'record' }, take('t1', 1), { type: 'stop' }, { type: 'stopped', takeId: 't1', durationSec: 3 }, { type: 'record' }, { type: 'start-failed', message: 'Camera busy' }], s);
  assert.equal(s.phase, 'review');
  assert.equal(s.message, 'Camera busy');
  s = run([{ type: 'submit' }, { type: 'submit-failed', message: 'Link expired' }], s);
  assert.equal(s.phase, 'review');
  assert.equal(s.message, 'Link expired');
});

test('link errors map to the right screen; a sent recording keeps its screen', () => {
  assert.equal(run([{ type: 'link-error', status: 401 }]).phase, 'invalid');
  assert.equal(run([{ type: 'link-error', status: 404 }]).phase, 'invalid');
  assert.equal(run([{ type: 'link-error', status: 410 }]).phase, 'expired');
  assert.equal(run([{ type: 'link-ok', accepting: false }]).phase, 'full');
  assert.equal(run([{ type: 'link-error', status: 500, message: 'boom' }]).phase, 'loading');
  const sent = run([{ type: 'resumed', submission: { status: 'submitted', takes: [] } }]);
  assert.equal(run([{ type: 'link-error', status: 401 }], sent).phase, 'submitted');
  // While recording, a rotated link stops the page.
  const rec = run([{ type: 'link-ok', accepting: true }, { type: 'record' }, take('t', 1)]);
  const after = run([{ type: 'link-error', status: 401 }], rec);
  assert.equal(after.phase, 'invalid');
  assert.equal(after.activeTakeId, undefined);
});

test('resume restores takes and upload state from the host', () => {
  const sub = { status: 'draft', takes: [
    { id: 'a', number: 1, status: 'stopped', durationSec: 20, tracks: [{ id: 'x', kind: 'video', label: 'Camera', status: 'assembled', bytes: 1000 }] },
    { id: 'b', number: 3, status: 'stopped', durationSec: 19, tracks: [{ id: 'y', kind: 'video', label: 'Camera', status: 'recording', bytes: 400 }, { id: 'z', kind: 'audio', label: 'Uncompressed', status: 'partial', bytes: 9 }] },
  ] };
  const s = run([{ type: 'resumed', submission: sub }]);
  assert.equal(s.phase, 'review');
  assert.deepEqual(s.takes.map(t => [t.number, t.uploaded, t.uploadedBytes]), [[1, true, 1000], [3, false, 409]]);
  assert.equal(canSubmit(s), true);
  assert.equal(run([{ type: 'resumed', submission: { status: 'draft', takes: [] } }]).phase, 'setup');
  assert.equal(run([{ type: 'resumed', submission: { ...sub, status: 'submitted' } }]).phase, 'submitted');
});

test('take count is capped like the host cap', () => {
  let s = run([{ type: 'link-ok', accepting: true }]);
  for (let i = 1; i <= 10; i++) s = run([{ type: 'record' }, take(`t${i}`, i), { type: 'stop' }, { type: 'stopped', takeId: `t${i}`, durationSec: 1 }], s);
  assert.equal(s.takes.length, 10);
  assert.equal(canRecord(s), false);
  assert.equal(run([{ type: 'record' }], s).phase, 'review');
});

test('link URL, saved draft and formatting helpers', () => {
  const secret = 'a'.repeat(64);
  assert.deepEqual(R.linkFromUrl(`?link=rl_0123456789ab&token=${secret}`), { linkId: 'rl_0123456789ab', secret });
  for (const bad of ['', '?link=rl_x&token=' + secret, `?link=../etc&token=${secret}`, '?link=rl_0123456789ab&token=short', `?link=rl_0123456789ab&token=${'G'.repeat(64)}`]) assert.equal(R.linkFromUrl(bad), null, bad);
  assert.equal(R.draftKey('rl_1'), 'studiolink.record.rl_1');
  assert.deepEqual(R.parseDraft(JSON.stringify({ submissionId: 'ab12cd34', key: 'f'.repeat(64), name: 'Ana' })), { submissionId: 'ab12cd34', key: 'f'.repeat(64), name: 'Ana' });
  for (const bad of [null, '', '{', '[]', JSON.stringify({ submissionId: 'a b', key: 'f'.repeat(64) }), JSON.stringify({ submissionId: 'x', key: 'nothex' })]) assert.equal(R.parseDraft(bad), null, String(bad));
  assert.equal(R.formatClock(7.9), '0:07');
  assert.equal(R.formatClock(754), '12:34');
  assert.equal(R.formatClock(3723), '1:02:03');
  assert.equal(R.formatLimit(45), '45 s');
  assert.equal(R.formatLimit(1800), '30 min');
  assert.equal(R.formatLimit(5400), '1 h 30 min');
  assert.equal(R.formatLimit(7200), '2 h');
  assert.equal(R.remainingSec(30, 12.2), 18);
  assert.equal(R.remainingSec(30, 31), 0);
  const now = Date.parse('2026-10-26T12:00:00Z');
  assert.equal(R.expiryLabel('2026-10-26T11:00:00Z', now), 'Expired');
  assert.equal(R.expiryLabel('2026-10-26T12:20:00Z', now), 'Expires in 20 min');
  assert.equal(R.expiryLabel('2026-10-27T03:00:00Z', now), 'Expires in 15 h');
  assert.equal(R.expiryLabel('2026-11-02T12:00:00Z', now), 'Expires in 7 days');
  assert.equal(R.formatBytes(1536 * 1024), '1.5 MB');
  assert.deepEqual(R.linkStatus({ linkEnabled: true, expired: false, submitted: 1, maxSubmissions: 10 }), { label: 'Accepting', tone: 'ok' });
  assert.deepEqual(R.linkStatus({ linkEnabled: true, expired: false, submitted: 10, maxSubmissions: 10 }), { label: 'Full', tone: 'warn' });
  assert.deepEqual(R.linkStatus({ linkEnabled: true, expired: true, submitted: 0, maxSubmissions: 10 }), { label: 'Expired', tone: 'off' });
  assert.deepEqual(R.linkStatus({ linkEnabled: false, expired: false, submitted: 0, maxSubmissions: 10 }), { label: 'Turned off', tone: 'off' });
});

test('guest API sends the secret and the submission key, and never in the URL', async () => {
  const calls = [];
  const fake = async (url, init = {}) => { calls.push({ url, init }); return new Response(JSON.stringify({ ok: true }), { status: 200 }); };
  const api = new R.RecordGuestApi('rl_0123456789ab', 's'.repeat(64), fake);
  await api.info();
  await api.get('sub1', 'k'.repeat(64));
  await api.startTake('sub1', 'k'.repeat(64));
  await api.discardTake('sub1', 'k'.repeat(64), 'take_sub1_01');
  for (const c of calls) {
    assert.equal(c.init.headers.Authorization, `Bearer ${'s'.repeat(64)}`);
    assert.ok(!c.url.includes('s'.repeat(64)) && !c.url.includes('k'.repeat(64)), c.url);
  }
  assert.equal(calls[0].init.headers['X-Submission-Key'], undefined);
  assert.equal(calls[1].init.headers['X-Submission-Key'], 'k'.repeat(64));
  assert.equal(calls[3].init.method, 'DELETE');
  const failing = new R.RecordGuestApi('rl_0123456789ab', 's'.repeat(64), async () => new Response('This recording link has expired.', { status: 410 }));
  await assert.rejects(failing.info(), err => err instanceof R.RecordApiError && err.status === 410 && /expired/.test(err.message));
});

test('abandoned-draft host API and labels [SL-P5-4]', async () => {
  const calls = [];
  const fake = async (url, init = {}) => { calls.push({ url, init }); return url.endsWith('/drafts/a b') || init.method === 'DELETE' ? new Response(null, { status: 204 }) : new Response(JSON.stringify({ deleted: [], skipped: [], bytes: 0, drafts: [], policy: {} }), { status: 200 }); };
  const api = new R.RecordLinksApi(fake);
  await api.drafts();
  await api.deleteDraft('a b');
  await api.cleanupDrafts();
  await api.cleanupDrafts(['x']);
  assert.deepEqual(calls.map(c => [c.init.method ?? 'GET', c.url]), [
    ['GET', '/api/v1/recordlinks/drafts'], ['DELETE', '/api/v1/recordlinks/drafts/a%20b'],
    ['POST', '/api/v1/recordlinks/drafts/cleanup'], ['POST', '/api/v1/recordlinks/drafts/cleanup']]);
  assert.equal(calls[2].init.body, '{}');
  assert.equal(calls[3].init.body, '{"ids":["x"]}');
  const now = Date.parse('2026-10-26T12:00:00Z');
  assert.equal(R.agoLabel('2026-10-26T11:59:30Z', now), 'just now');
  assert.equal(R.agoLabel('2026-10-26T11:00:00Z', now), '1 hour ago');
  assert.equal(R.agoLabel('2026-10-24T11:00:00Z', now), '2 days ago');
  assert.equal(R.agoLabel('2026-10-26T11:55:00Z', now), '5 minutes ago');
  assert.match(R.draftPolicyText({ autoCleanup: true, expiryDays: 14, closedLinkHours: 24, uploadGuardMinutes: 60, abandonedAfterHours: 24 }), /14 days without activity, or 24 hours after their link expires/);
  assert.match(R.draftPolicyText({ autoCleanup: false, expiryDays: 0, closedLinkHours: 24, uploadGuardMinutes: 60, abandonedAfterHours: 24 }), /off/);
});
