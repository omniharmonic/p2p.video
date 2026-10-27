import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import ts from 'typescript';

const source = await readFile(new URL('../src/lib/ecosystem.ts', import.meta.url), 'utf8');
const js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } }).outputText;
const { actionModel, anyRunning, canCancelTranscript, engineOptions, engineStatus, integrationUrl, isValidView, nextPollMs, trackRows, transcriptCapability, transcriptUrl } =
  await import(`data:text/javascript;base64,${Buffer.from(js).toString('base64')}`);

const idle = { state: 'idle' };
const builtinReady = { id: 'builtin', label: 'Built-in (this Mac)', available: true, detail: 'p2p.video on-device transcription (apple-speechtranscriber)' };
const minutesReady = { id: 'minutes', label: 'Minutes', available: true };
function view(overrides = {}) {
  return {
    takeId: 't 1', packageReady: true, hasTranscript: false, hasHandoff: false, autoTranscribe: false, canResume: false,
    capabilities: { transcript: { available: true }, minutes: { available: true }, parachute: { available: true, target: 'https://hub.example.com/vault/default' }, programMix: { available: true } },
    transcription: { engine: 'builtin', engines: [builtinReady, minutesReady], language: 'en-US' },
    transcript: idle, parachute: idle, clipEditor: idle,
    ...overrides,
  };
}

test('ready actions are enabled with their run labels; the vault target is shown', () => {
  const v = view();
  assert.deepEqual([actionModel(v, 'transcript').label, actionModel(v, 'transcript').enabled], ['Transcribe', true]);
  assert.equal(actionModel(v, 'parachute').note, 'Vault: https://hub.example.com/vault/default');
  assert.equal(actionModel(v, 'clipEditor').label, 'Prepare clip-editor handoff');
  assert.deepEqual(['transcript', 'parachute', 'clipEditor'].map(kind => actionModel(v, kind).title), ['Transcript', 'Parachute session note', 'Clip-editor handoff']);
});

test('the transcript follows the selected engine, not Minutes alone', () => {
  const v = view({ capabilities: { ...view().capabilities, transcript: { available: false, code: 'no_provider', reason: 'Built-in transcription runs in the p2p.video Mac app. Open p2p.video…' } } });
  const t = actionModel(v, 'transcript');
  assert.deepEqual([t.enabled, t.badge, t.tone], [false, 'Unavailable', 'unavailable']);
  assert.match(t.note, /Open p2p.video/);
  // Older hosts without capabilities.transcript fall back to Minutes.
  const legacy = view({ capabilities: { minutes: { available: false, reason: 'minutes-cli not found' }, parachute: { available: true }, programMix: { available: true } } });
  assert.equal(transcriptCapability(legacy).reason, 'minutes-cli not found');
  assert.equal(actionModel(legacy, 'transcript').enabled, false);
});

test('unavailable integrations are disabled and say why', () => {
  const v = view({ capabilities: { transcript: { available: false, reason: 'minutes-cli not found' }, minutes: { available: false }, parachute: { available: false, reason: 'no Parachute vault configured' }, programMix: { available: false } } });
  const t = actionModel(v, 'transcript');
  assert.equal(t.enabled, false);
  assert.equal(t.note, 'minutes-cli not found');
  assert.equal(actionModel(v, 'parachute').enabled, false);
  assert.equal(actionModel(v, 'clipEditor').enabled, true, 'the clip-editor handoff needs nothing external');
});

test('running, done, error and cancelled states map to badges; reruns get "again" or "retry" labels', () => {
  const running = actionModel(view({ transcript: { state: 'running', message: 'Transcribing Ana (1 of 2)…' } }), 'transcript');
  assert.deepEqual([running.enabled, running.badge, running.tone, running.note], [false, 'Running', 'running', 'Transcribing Ana (1 of 2)…']);
  const done = actionModel(view({ hasTranscript: true, transcript: { state: 'done', message: 'Transcript ready: 3 segments from 2 speakers.' } }), 'transcript');
  assert.deepEqual([done.enabled, done.badge, done.label], [true, 'Done', 'Transcribe again']);
  const failed = actionModel(view({ parachute: { state: 'error', message: 'the vault refused the credential' } }), 'parachute');
  assert.deepEqual([failed.enabled, failed.tone, failed.note], [true, 'danger', 'the vault refused the credential']);
  const cancelled = actionModel(view({ canResume: true, transcript: { state: 'cancelled', message: 'Cancelled. Nothing was written to the package.' } }), 'transcript');
  assert.deepEqual([cancelled.enabled, cancelled.badge, cancelled.label], [true, 'Cancelled', 'Retry remaining tracks']);
  const partial = actionModel(view({ canResume: true, hasTranscript: true, transcript: { state: 'done', message: 'Some tracks failed' } }), 'transcript');
  assert.equal(partial.label, 'Retry remaining tracks');
  assert.equal(actionModel(view({ parachute: { state: 'done' } }), 'parachute').label, 'Update Parachute note');
  assert.equal(actionModel(view({ hasHandoff: true }), 'clipEditor').label, 'Refresh clip-editor handoff');
});

test('a permission error stays visible even while the engine is unavailable', () => {
  const v = view({
    capabilities: { ...view().capabilities, transcript: { available: false, code: 'not_authorized', reason: 'Speech Recognition is not allowed for p2p.video.' } },
    transcript: { state: 'error', message: 'Ana: Speech Recognition is not allowed for p2p.video.' },
  });
  const m = actionModel(v, 'transcript');
  assert.deepEqual([m.badge, m.tone, m.enabled], ['Error', 'danger', false]);
  assert.match(m.note, /Speech Recognition/);
});

test('nothing runs before the package exists', () => {
  const m = actionModel(view({ packageReady: false }), 'clipEditor');
  assert.deepEqual([m.enabled, m.note], [false, 'Prepare the editor files first.']);
});

test('per-track rows show progress, results and errors; bad data is dropped', () => {
  const rows = trackRows({ state: 'running', detail: { engine: 'builtin', tracks: [
    { sourceId: 'a', speaker: 'Ana', state: 'done', progress: 1, segments: 12, reused: true },
    { sourceId: 'b', speaker: 'Ben', state: 'running', progress: 0.426 },
    { sourceId: 'c', speaker: 'Cy', state: 'queued', progress: 0 },
    { sourceId: 'd', speaker: 'Di', state: 'empty', progress: 1, segments: 0 },
    { sourceId: 'e', speaker: 'Ed', state: 'error', progress: 0, error: 'Speech Recognition is not allowed' },
    { sourceId: 'f', speaker: 'Fa', state: 'exploded' },
    null,
  ] } });
  assert.deepEqual(rows.map(r => [r.speaker, r.percent, r.status, r.tone]), [
    ['Ana', 100, '12 lines (kept)', 'success'], ['Ben', 42, '42%', 'running'], ['Cy', 0, 'Waiting', 'idle'], ['Di', 100, 'No speech', 'success'], ['Ed', 0, 'Failed', 'danger'],
  ]);
  assert.equal(rows[4].error, 'Speech Recognition is not allowed');
  assert.deepEqual(trackRows({ state: 'idle' }), []);
  assert.deepEqual(trackRows({ state: 'running', detail: { tracks: 'nope' } }), []);
});

test('engine picker: built-in always selectable, Minutes only when detected', () => {
  const missing = { id: 'minutes', label: 'Minutes', available: false, code: 'not_found', reason: 'Minutes is optional…' };
  const opts = engineOptions(view({ transcription: { engine: 'builtin', engines: [{ ...builtinReady, available: false, code: 'no_provider' }, missing] } }));
  assert.deepEqual(opts.map(o => [o.id, o.selected, o.disabled, o.label]), [['builtin', true, false, 'Built-in (this Mac)'], ['minutes', false, true, 'Minutes (not detected)']]);
  // A previously chosen Minutes that disappeared stays selectable so the picker can show it.
  const stale = engineOptions(view({ transcription: { engine: 'minutes', engines: [builtinReady, missing] } }));
  assert.equal(stale[1].disabled, false);
  assert.deepEqual(engineOptions(view({ transcription: undefined })), []);
});

test('engine status explains readiness and what to do', () => {
  assert.equal(engineStatus(view()).tone, 'success');
  assert.match(engineStatus(view()).text, /audio never leaves this Mac/);
  const denied = engineStatus(view({ transcription: { engine: 'builtin', engines: [{ ...builtinReady, available: false, code: 'not_authorized', reason: 'Speech Recognition is not allowed for p2p.video.' }, minutesReady] } }));
  assert.deepEqual([denied.tone, denied.text], ['danger', 'Speech Recognition is not allowed for p2p.video.']);
  const noApp = engineStatus(view({ transcription: { engine: 'builtin', engines: [{ ...builtinReady, available: false, code: 'no_provider', reason: 'Open p2p.video' }, minutesReady] } }));
  assert.equal(noApp.tone, 'unavailable');
  assert.match(engineStatus(view({ transcription: { engine: 'minutes', engines: [builtinReady, minutesReady] } })).text, /minutes-cli/);
});

test('polling: fast while running, slow while the engine is missing, otherwise off', () => {
  assert.equal(nextPollMs(null), null);
  assert.equal(nextPollMs(view()), null);
  assert.equal(anyRunning(view({ clipEditor: { state: 'running' } })), true);
  assert.equal(nextPollMs(view({ parachute: { state: 'running' } })), 1500);
  assert.equal(nextPollMs(view({ capabilities: { ...view().capabilities, transcript: { available: false } } })), 5000, 'notices when the app opens');
  assert.equal(nextPollMs(view({ packageReady: false, capabilities: { ...view().capabilities, transcript: { available: false } } })), null);
  assert.equal(canCancelTranscript(view({ transcript: { state: 'running' } })), true);
  assert.equal(canCancelTranscript(view()), false);
  assert.equal(canCancelTranscript(null), false);
});

test('URLs encode the take id; status payloads are validated', () => {
  assert.equal(integrationUrl('t 1'), '/api/v1/takes/t%201/integrations');
  assert.equal(integrationUrl('t/../x', 'clipEditor'), '/api/v1/takes/t%2F..%2Fx/handoff/clip-editor');
  assert.equal(integrationUrl('a', 'transcript'), '/api/v1/takes/a/transcript');
  assert.equal(transcriptUrl('t 1', 'cancel'), '/api/v1/takes/t%201/transcript/cancel');
  assert.equal(transcriptUrl('a', 'engine'), '/api/v1/takes/a/transcript/engine');
  assert.equal(isValidView(view()), true);
  assert.equal(isValidView({ takeId: 'x' }), false);
  assert.equal(isValidView(null), false);
});
