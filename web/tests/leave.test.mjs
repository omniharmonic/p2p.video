// Leave call for guests [SL-P1-6]: state machine, confirmation and upload copy.
import test from 'node:test';
import assert from 'node:assert/strict';
import { build } from 'esbuild';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const dir = await mkdtemp(join(tmpdir(), 'studiolink-leave-'));
const out = join(dir, 'leave.mjs');
await build({ entryPoints: [new URL('../src/lib/leave.ts', import.meta.url).pathname], bundle: true, format: 'esm', outfile: out });
const { nextLeaveState, uploadsPending, leaveConfirmation, leftUploadText, leftUploadFraction } = await import(out);
test.after(() => rm(dir, { recursive: true, force: true }));

const idle = { recording: false, pendingBytes: 0, unfinishedTracks: 0 };

test('leaving with nothing pending skips the confirmation', () => {
  assert.equal(uploadsPending(idle), false);
  assert.equal(leaveConfirmation(idle), null);
  assert.equal(nextLeaveState('in-call', { type: 'leave', pending: false }), 'left');
});

test('pending uploads or a running take ask first; cancel stays in the call', () => {
  for (const b of [{ ...idle, pendingBytes: 1 }, { ...idle, unfinishedTracks: 1 }, { ...idle, recording: true }]) {
    assert.equal(uploadsPending(b), true);
    assert.ok(leaveConfirmation(b));
  }
  assert.equal(nextLeaveState('in-call', { type: 'leave', pending: true }), 'confirming');
  assert.equal(nextLeaveState('confirming', { type: 'cancel' }), 'in-call');
  assert.equal(nextLeaveState('confirming', { type: 'confirm' }), 'left');
});

test('the upload confirmation says leaving pauses it and how to finish', () => {
  const copy = leaveConfirmation({ ...idle, pendingBytes: 5e6, unfinishedTracks: 1 });
  assert.match(copy.body, /still uploading/);
  assert.match(copy.body, /reopen this link from this browser to finish/);
  assert.equal(copy.confirmLabel, 'Leave call');
  assert.match(leaveConfirmation({ ...idle, recording: true }).body, /stops your recording/);
});

test('rejoin goes back through admission and can fail or find the meeting over', () => {
  assert.equal(nextLeaveState('left', { type: 'rejoin' }), 'rejoining');
  assert.equal(nextLeaveState('rejoining', { type: 'admitted' }), 'in-call');
  assert.equal(nextLeaveState('rejoining', { type: 'rejoin-failed' }), 'left');
  assert.equal(nextLeaveState('rejoining', { type: 'meeting-ended' }), 'ended');
  assert.equal(nextLeaveState('left', { type: 'meeting-ended' }), 'ended');
  // In the call, session_end is handled by the meeting-ended summary instead.
  assert.equal(nextLeaveState('in-call', { type: 'meeting-ended' }), 'in-call');
});

test('events that do not apply leave the state unchanged', () => {
  assert.equal(nextLeaveState('left', { type: 'leave', pending: true }), 'left');
  assert.equal(nextLeaveState('in-call', { type: 'admitted' }), 'in-call');
  assert.equal(nextLeaveState('ended', { type: 'rejoin' }), 'ended');
  assert.equal(nextLeaveState('in-call', { type: 'confirm' }), 'in-call');
});

test('post-leave upload text and progress', () => {
  assert.equal(leftUploadText(idle), 'No recording on this device.');
  assert.equal(leftUploadFraction(idle), null);
  assert.equal(leftUploadText({ ...idle, recording: true }), 'Finishing your recording…');
  const mid = { ...idle, pendingBytes: 18 * 1024 * 1024, unfinishedTracks: 2, rateBps: 8 * 1024 * 1024, uploadedBytes: 6 * 1024 * 1024 };
  assert.equal(leftUploadText(mid), 'Uploading 18.0 MB · less than a minute left. Keep this tab open.');
  assert.equal(leftUploadFraction(mid), 0.25);
  assert.match(leftUploadText({ ...mid, rateBps: 400_000 }), /~7 min left/);
  assert.match(leftUploadText({ ...idle, unfinishedTracks: 1, uploadedBytes: 10 }), /Waiting for the host/);
  assert.equal(leftUploadText({ ...idle, uploadedBytes: 10 }), 'Your recording reached the host. You can close this tab.');
});
