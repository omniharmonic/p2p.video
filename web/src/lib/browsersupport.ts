/**
 * Safari and Firefox guests [SL-P1-12]: which p2p.video features this
 * browser can't offer, from feature detection only (never the user agent
 * alone). Pure: the probe lives in app/greenroom.ts so this stays testable.
 *
 * The green room shows a gentle recommendation only when this list is not
 * empty, and names exactly what is missing. Calls and local recording work
 * in all three engines; these are the optional extras that degrade.
 */
export type Engine = 'chromium' | 'firefox' | 'webkit' | 'other';

export interface BrowserCapabilities {
  engine: Engine;
  /** Mobile (iPhone/iPad/Android) user agent. */
  mobile: boolean;
  /** HTMLMediaElement.setSinkId exists. */
  setSinkId: boolean;
  /** enumerateDevices lists at least one audiooutput (Firefox lists none until selectAudioOutput). */
  listsOutputs: boolean;
  /** navigator.mediaDevices.getDisplayMedia exists. */
  displayMedia: boolean;
  /** AudioWorkletNode exists (uncompressed microphone, live captions). */
  audioWorklet: boolean;
  /** MediaRecorder exists with a usable video container. */
  mediaRecorder: boolean;
  /** Recording storage backend (lib/opfs.ts). */
  storage: 'opfs' | 'opfs-worker' | 'memory';
}

export interface BrowserLimitation { id: string; text: string }

/** Engine from the user agent; only used to name the browser in the note. */
export function detectEngine(ua: string): Engine {
  if (/Firefox\/|FxiOS\//.test(ua)) return 'firefox';
  // Every iOS browser is WebKit, whatever its name.
  if (/iPhone|iPad|iPod/.test(ua) || (/AppleWebKit\//.test(ua) && !/Chrome\/|Chromium\/|Edg\//.test(ua))) return 'webkit';
  if (/Chrome\/|Chromium\/|Edg\//.test(ua)) return 'chromium';
  return 'other';
}

export function browserName(engine: Engine, mobile: boolean): string {
  if (engine === 'webkit') return mobile ? 'Safari on iPhone or iPad' : 'Safari';
  if (engine === 'firefox') return 'Firefox';
  if (engine === 'chromium') return 'Chrome';
  return 'this browser';
}

/** What this browser can't do, most important first. */
export function browserLimitations(c: BrowserCapabilities): BrowserLimitation[] {
  const out: BrowserLimitation[] = [];
  if (!c.mediaRecorder) out.push({ id: 'recording', text: 'local recording, so the host only gets your call audio and video' });
  if (!c.displayMedia) out.push({ id: 'screen', text: 'presenting your screen' });
  if (!c.setSinkId || !c.listsOutputs) out.push({ id: 'speaker', text: c.engine === 'firefox' && c.setSinkId ? 'choosing a speaker output in the call (Firefox uses your system default output)' : 'choosing a speaker output in the call (your system default output is used)' });
  if (!c.audioWorklet) out.push({ id: 'pcm', text: 'the uncompressed microphone track' });
  return out;
}

/**
 * The green-room note, or '' when nothing is missing or the browser is
 * Chromium-based (the reference engine: nothing to recommend). A private
 * window (no local storage) is its own sentence: the fix there is a normal
 * window, not another browser.
 */
export function browserNote(c: BrowserCapabilities): string {
  if (c.engine === 'chromium') return '';
  const missing = browserLimitations(c);
  const parts: string[] = [];
  if (missing.length) {
    const list = missing.map(m => m.text);
    const joined = list.length === 1 ? list[0] : `${list.slice(0, -1).join('; ')}; and ${list[list.length - 1]}`;
    const alternative = c.mobile && c.engine === 'webkit' ? 'Chrome or Edge on a computer' : 'Chrome or Edge';
    parts.push(`${browserName(c.engine, c.mobile)} doesn’t support ${joined}. If you need ${list.length === 1 ? 'that' : 'those'}, ${alternative} supports ${list.length === 1 ? 'it' : 'them'}.`);
  }
  if (c.storage === 'memory') parts.push('This looks like a private window, so no local copy of your recording is kept: closing this tab loses anything not yet uploaded. Open the link in a normal window to keep one.');
  if (!parts.length) return '';
  return `You can ${c.mediaRecorder ? 'join and record' : 'join'} from ${browserName(c.engine, c.mobile)}. Good to know: ${parts.join(' ')}`;
}

/** Limitation ids for the note (storage included), for diagnostics and tests. */
export function noteLimitationIds(c: BrowserCapabilities): string[] {
  return [...browserLimitations(c).map(l => l.id), ...(c.storage === 'memory' ? ['storage'] : [])];
}
