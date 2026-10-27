// Call audio processing (noise suppression etc.) for the call mic only [SL-P1-8].
import test from 'node:test';
import assert from 'node:assert/strict';
import { build } from 'esbuild';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const dir = await mkdtemp(join(tmpdir(), 'studiolink-callaudio-'));
const out = join(dir, 'callmic.mjs');
await build({ entryPoints: [new URL('../src/lib/callmic.ts', import.meta.url).pathname], bundle: true, format: 'esm', outfile: out });
const { CallMicrophone, SharedProcessingError, captureAudioConstraints, RAW_CAPTURE_PROCESSING, DEFAULT_CALL_AUDIO_PROCESSING, loadCallAudioProcessing, saveCallAudioProcessing, processingMatches, callAudioConstraints, CALL_AUDIO_STORAGE_KEY, openAudio, openCaptureMic } = await import(out);
test.after(() => rm(dir, { recursive: true, force: true }));

class FakeTrack {
  constructor(settings, label = 'mic') { this.settings = settings; this.label = label; this.enabled = true; this.readyState = 'live'; this.kind = 'audio'; }
  getSettings() { return { ...this.settings }; }
  clone() { return new FakeTrack(this.settings, this.label + '-clone'); }
  stop() { this.readyState = 'ended'; }
}
const stream = track => ({ getAudioTracks: () => [track], getVideoTracks: () => [] });
// What a browser reports for granted constraints: exact/ideal wrappers unwrapped.
const granted = audio => Object.fromEntries(Object.entries(audio).map(([k, v]) => [k, v && typeof v === 'object' && ('exact' in v || 'ideal' in v) ? (v.exact ?? v.ideal) : v]));
const store = () => { const m = new Map(); return { getItem: k => m.get(k) ?? null, setItem: (k, v) => m.set(k, v), m }; };

test('defaults are all on; saved choices round-trip and garbage falls back', () => {
  assert.deepEqual(loadCallAudioProcessing(null), DEFAULT_CALL_AUDIO_PROCESSING);
  const s = store();
  saveCallAudioProcessing(s, { noiseSuppression: false, echoCancellation: true, autoGainControl: false });
  assert.deepEqual(loadCallAudioProcessing(s), { noiseSuppression: false, echoCancellation: true, autoGainControl: false });
  s.setItem(CALL_AUDIO_STORAGE_KEY, '{"noiseSuppression":"no"');
  assert.deepEqual(loadCallAudioProcessing(s), DEFAULT_CALL_AUDIO_PROCESSING);
  s.setItem(CALL_AUDIO_STORAGE_KEY, '{"noiseSuppression":"no","autoGainControl":false}');
  assert.deepEqual(loadCallAudioProcessing(s), { ...DEFAULT_CALL_AUDIO_PROCESSING, autoGainControl: false });
});

test('processing match treats unreported settings as matching', () => {
  const on = DEFAULT_CALL_AUDIO_PROCESSING;
  assert.equal(processingMatches({ noiseSuppression: true, echoCancellation: true, autoGainControl: true }, on), true);
  assert.equal(processingMatches({}, on), true);
  assert.equal(processingMatches({ noiseSuppression: true }, { ...on, noiseSuppression: false }), false);
  // Processing is requested exactly (Chromium otherwise reuses an open capture as "best fit").
  assert.deepEqual(callAudioConstraints('dev1', { noiseSuppression: false, echoCancellation: true, autoGainControl: false }),
    { deviceId: { exact: 'dev1' }, echoCancellation: { exact: true }, noiseSuppression: { exact: false }, autoGainControl: { exact: false } });
  assert.deepEqual(callAudioConstraints('dev1', { noiseSuppression: false, echoCancellation: true, autoGainControl: false }, false),
    { deviceId: { exact: 'dev1' }, echoCancellation: true, noiseSuppression: false, autoGainControl: false });
});

test('matching processing keeps a clone; a different choice opens a dedicated call capture; recording track untouched', async () => {
  const recording = new FakeTrack({ deviceId: 'dev1', noiseSuppression: true, echoCancellation: true, autoGainControl: true });
  const mic = new CallMicrophone();
  const calls = [];
  const gum = async c => { calls.push(c); return { getAudioTracks: () => [new FakeTrack({ ...granted(c.audio), deviceId: 'dev1' }, 'dedicated')] }; };
  assert.equal(await mic.prepare(stream(recording), undefined, gum), null);
  assert.equal(mic.callTrack.label, 'mic-clone');
  assert.equal(mic.usesDedicatedCapture, false);
  assert.equal(calls.length, 0);
  // Same choice again: no change.
  assert.equal(await mic.prepare(stream(recording), undefined, gum), null);
  // Noise suppression off for the call: a dedicated capture; the clone is released.
  const clone = mic.callTrack;
  const previous = await mic.prepare(stream(recording), { ...DEFAULT_CALL_AUDIO_PROCESSING, noiseSuppression: false }, gum);
  assert.equal(previous, clone);
  assert.equal(clone.readyState, 'ended');
  assert.equal(mic.callTrack.label, 'dedicated');
  assert.equal(mic.usesDedicatedCapture, true);
  assert.deepEqual(calls[0].audio, { deviceId: { exact: 'dev1' }, echoCancellation: { exact: true }, noiseSuppression: { exact: false }, autoGainControl: { exact: true } });
  assert.equal(calls[0].video, false);
  assert.equal(recording.readyState, 'live');
  assert.equal(recording.enabled, true);
  // Back to defaults: a clone again.
  await mic.prepare(stream(recording), DEFAULT_CALL_AUDIO_PROCESSING, gum);
  assert.equal(mic.callTrack.label, 'mic-clone');
  assert.equal(mic.usesDedicatedCapture, false);
});

test('host mute and self mute still apply to a dedicated call capture', async () => {
  const recording = new FakeTrack({ deviceId: 'dev1', noiseSuppression: true });
  const mic = new CallMicrophone();
  const gum = async c => ({ getAudioTracks: () => [new FakeTrack(granted(c.audio), 'dedicated')] });
  await mic.prepare(stream(recording), { ...DEFAULT_CALL_AUDIO_PROCESSING, noiseSuppression: false }, gum);
  mic.setHostMuted(true);
  assert.equal(mic.callTrack.enabled, false);
  assert.equal(recording.enabled, true, 'a host mute never silences the recording');
  mic.setSelfMuted(false);
  assert.equal(mic.callTrack.enabled, true);
  mic.setSelfMuted(true);
  assert.equal(mic.callTrack.enabled, false);
  assert.equal(recording.enabled, false);
});

test('a browser that shares processing per device is detected, nothing leaks', async () => {
  const recording = new FakeTrack({ deviceId: 'dev1', noiseSuppression: false, echoCancellation: false, autoGainControl: false });
  const mic = new CallMicrophone();
  let opened;
  // The browser hands back the already-open raw source's processing.
  const gum = async () => { opened = new FakeTrack({ ...recording.settings }, 'shared'); return { getAudioTracks: () => [opened] }; };
  await assert.rejects(mic.prepare(stream(recording), DEFAULT_CALL_AUDIO_PROCESSING, gum), SharedProcessingError);
  assert.equal(opened.readyState, 'ended', 'the useless capture is released');
  assert.equal(mic.sharesRecordingProcessing, true);
  // After the caller reopens the capture with the call's processing, a clone matches.
  const processed = new FakeTrack({ deviceId: 'dev1', ...DEFAULT_CALL_AUDIO_PROCESSING });
  await mic.prepare(stream(processed), undefined, gum);
  assert.equal(mic.callTrack.label, 'mic-clone');
});

test('the recording capture opens raw by default, stereo, processing exact', () => {
  assert.deepEqual(captureAudioConstraints('dev1'), { deviceId: { exact: 'dev1' }, echoCancellation: { exact: false }, noiseSuppression: { exact: false }, autoGainControl: { exact: false }, channelCount: 2 });
  assert.deepEqual(captureAudioConstraints(undefined, RAW_CAPTURE_PROCESSING, false), { echoCancellation: false, noiseSuppression: false, autoGainControl: false, channelCount: 2 });
});

test('exact processing falls back to ideal values only on OverconstrainedError', async () => {
  const calls = [];
  const refusing = async c => {
    calls.push(c.audio);
    if (typeof c.audio.autoGainControl === 'object') { const e = new Error('no exact AGC'); e.name = 'OverconstrainedError'; throw e; }
    return { getAudioTracks: () => [new FakeTrack(granted(c.audio))] };
  };
  const s = await openCaptureMic('dev1', { echoCancellation: true, noiseSuppression: true, autoGainControl: false }, refusing);
  assert.equal(calls.length, 2);
  assert.deepEqual(calls[1], { deviceId: { exact: 'dev1' }, echoCancellation: true, noiseSuppression: true, autoGainControl: false, channelCount: 2 });
  assert.equal(s.getAudioTracks()[0].getSettings().echoCancellation, true);
  const denied = async () => { const e = new Error('denied'); e.name = 'NotAllowedError'; throw e; };
  await assert.rejects(openAudio(() => ({}), denied), /denied/);
});

test('a clean (processed) recording mic: a refused call capture shares it instead of touching the recording', async () => {
  const recording = new FakeTrack({ deviceId: 'dev1', echoCancellation: true, noiseSuppression: true, autoGainControl: false });
  const mic = new CallMicrophone();
  mic.shareWhenRefused = true;
  let calls = 0;
  // Chromium: a second processed config of the device is refused (exact), and ideal hands back the open one.
  const gum = async c => {
    calls++;
    if (typeof c.audio.autoGainControl === 'object') { const e = new Error('device busy'); e.name = 'OverconstrainedError'; throw e; }
    return { getAudioTracks: () => [new FakeTrack({ ...recording.settings }, 'shared')] };
  };
  assert.equal(await mic.prepare(stream(recording), DEFAULT_CALL_AUDIO_PROCESSING, gum), null);
  assert.equal(calls, 2);
  assert.equal(mic.followsRecording, true);
  assert.equal(mic.usesDedicatedCapture, false);
  assert.equal(mic.sharesRecordingProcessing, false, 'the recording is never reopened for the call');
  assert.equal(mic.callTrack.label, 'mic-clone');
  assert.equal(recording.readyState, 'live');
  // Asking again (every renegotiation) doesn't hit the device again.
  assert.equal(await mic.prepare(stream(recording), DEFAULT_CALL_AUDIO_PROCESSING, gum), null);
  assert.equal(calls, 2);
  // A raw recording mic again: the call gets its own processed capture back.
  const raw = new FakeTrack({ deviceId: 'dev1', echoCancellation: false, noiseSuppression: false, autoGainControl: false });
  mic.shareWhenRefused = false;
  const own = async c => ({ getAudioTracks: () => [new FakeTrack({ ...granted(c.audio), deviceId: 'dev1' }, 'dedicated')] });
  await mic.prepare(stream(raw), DEFAULT_CALL_AUDIO_PROCESSING, own);
  assert.equal(mic.followsRecording, false);
  assert.equal(mic.usesDedicatedCapture, true);
  // releaseCallCapture stops only a dedicated call capture.
  mic.releaseCallCapture();
  assert.equal(mic.callTrack.readyState, 'ended');
  assert.equal(raw.readyState, 'live');
});
