// Recording microphone processing mode [SL-P1-10].
//
// The recording microphone is either raw (no browser processing: the
// editor-grade ISO track, right with headphones) or clean (echo cancellation
// and noise suppression on, automatic gain off: without headphones the other
// voices from the speakers would otherwise bleed into the recording).
//
//   participant's choice   auto (default) | raw | clean
//   host's session default auto (default) | raw | clean   replaces "auto"
//   auto                   raw with headphones, clean without
//
// The browser's actual processing is read back from the recording track's
// getSettings() and reported (pre-flight → host, sidecar → manifest), so a
// browser that shares processing between captures of one device can't make
// the label lie. Chromium measurements and the arrangement: DECISIONS
// 2026-10-26.

import type { CallAudioProcessing } from './callmic';
import type { RecordingMicMode, RecordingMicReason, RecordingMicState, ResolvedMicMode } from './protocol';

export const RECORDING_MIC_STORAGE_KEY = 'studiolink.recordingMicMode';

/** Raw: no browser processing. Clean: EC + NS, never automatic gain. */
export const RECORDING_PROCESSING: Readonly<Record<ResolvedMicMode, Readonly<CallAudioProcessing>>> = {
  raw: { echoCancellation: false, noiseSuppression: false, autoGainControl: false },
  clean: { echoCancellation: true, noiseSuppression: true, autoGainControl: false },
};

export function isRecordingMicMode(v: unknown): v is RecordingMicMode {
  return v === 'auto' || v === 'raw' || v === 'clean';
}

/** Resolves the participant's mode against the session default and headphones. */
export function resolveRecordingMic(own: RecordingMicMode, sessionDefault: RecordingMicMode | undefined, headphones: boolean): { resolved: ResolvedMicMode; reason: RecordingMicReason } {
  if (own !== 'auto') return { resolved: own, reason: 'participant' };
  if (sessionDefault === 'raw' || sessionDefault === 'clean') return { resolved: sessionDefault, reason: 'session' };
  return headphones ? { resolved: 'raw', reason: 'headphones' } : { resolved: 'clean', reason: 'no_headphones' };
}

type ProcessingSettings = Partial<Record<keyof CallAudioProcessing, boolean>>;

/**
 * The state to report for a recording track: the resolved mode plus what the
 * browser actually applies. An unreported setting counts as the requested one.
 */
export function recordingMicState(own: RecordingMicMode, sessionDefault: RecordingMicMode | undefined, headphones: boolean, settings: ProcessingSettings): RecordingMicState {
  const { resolved, reason } = resolveRecordingMic(own, sessionDefault, headphones);
  const want = RECORDING_PROCESSING[resolved];
  const actual = {
    echoCancellation: settings.echoCancellation ?? want.echoCancellation,
    noiseSuppression: settings.noiseSuppression ?? want.noiseSuppression,
    autoGainControl: settings.autoGainControl ?? want.autoGainControl,
  };
  const followsCall = (Object.keys(want) as Array<keyof CallAudioProcessing>).some(k => actual[k] !== want[k]);
  return { mode: own, resolved, reason, headphones, ...actual, ...(followsCall ? { followsCall } : {}) };
}

/** Short label: "Raw mic" / "Clean mic (no headphones)". */
export function recordingMicLabel(state: Pick<RecordingMicState, 'resolved' | 'reason' | 'followsCall'> | null | undefined): string {
  if (!state) return 'Recording mic unknown';
  const base = state.resolved === 'raw' ? 'Raw mic' : 'Clean mic';
  const why = state.reason === 'no_headphones' ? ' (no headphones)'
    : state.reason === 'headphones' ? ' (headphones)'
    : state.reason === 'session' ? ' (host setting)'
    : state.reason === 'participant' ? ' (chosen)' : '';
  return `${base}${why}${state.followsCall ? ' · browser applies call processing' : ''}`;
}

/** One sentence for the person recording. */
export function recordingMicExplanation(resolved: ResolvedMicMode): string {
  return resolved === 'raw'
    ? 'Raw: your recording gets no browser processing — best for editing. Keep headphones on so others’ voices stay out.'
    : 'Clean: echo cancellation and noise suppression keep the voices from your speakers out of your recording (no automatic volume).';
}

type KV = Pick<Storage, 'getItem' | 'setItem'>;

export function loadRecordingMicMode(storage: KV | null | undefined): RecordingMicMode {
  try { const v = storage?.getItem(RECORDING_MIC_STORAGE_KEY); return isRecordingMicMode(v) ? v : 'auto'; } catch { return 'auto'; }
}

export function saveRecordingMicMode(storage: KV | null | undefined, mode: RecordingMicMode): void {
  try { storage?.setItem(RECORDING_MIC_STORAGE_KEY, mode); } catch { /* Private mode: this page only. */ }
}
