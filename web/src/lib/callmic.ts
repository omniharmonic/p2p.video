// The call sends a clone of the microphone track, so the call microphone can
// be muted (e.g. at the host's request) while the local ISO recording keeps
// the original track. Self-mute still disables both: see setMuted().
//
//   original track  → local recorders (MediaRecorder / PCM) and the level meter
//   clone           → WebRTC call sender
//
// Call audio processing [SL-P1-8]: noise suppression, echo cancellation and
// automatic gain for the call only. The capture (recording) microphone opens
// raw or clean (recording mic mode, micmode.ts, SL-P1-10; processing requested
// exactly); the call gets its own capture of the same device
// with the call's processing. When the choice matches the capture track, the
// call keeps a plain clone. Chrome shares one processing config per device
// when the device was first opened *with* processing (a later raw request
// gets processed audio), but processes per track when it was opened raw, which
// is why the capture opens raw. A browser that still returns the wrong
// processing raises SharedProcessingError: the caller then reopens the capture
// with the call's processing (settings then apply to both, stated in the UI).

/** Browser audio processing for the call microphone. */
export interface CallAudioProcessing {
  noiseSuppression: boolean;
  echoCancellation: boolean;
  autoGainControl: boolean;
}

/** Meet's defaults: everything on for the call. */
export const DEFAULT_CALL_AUDIO_PROCESSING: Readonly<CallAudioProcessing> = { noiseSuppression: true, echoCancellation: true, autoGainControl: true };
export const CALL_AUDIO_STORAGE_KEY = 'studiolink.callAudioProcessing';

type KV = Pick<Storage, 'getItem' | 'setItem'>;

/** The saved choice, with defaults for anything missing or unreadable. */
export function loadCallAudioProcessing(storage: KV | null | undefined): CallAudioProcessing {
  const out = { ...DEFAULT_CALL_AUDIO_PROCESSING };
  try {
    const saved = JSON.parse(storage?.getItem(CALL_AUDIO_STORAGE_KEY) ?? 'null');
    if (saved && typeof saved === 'object') {
      for (const key of Object.keys(out) as Array<keyof CallAudioProcessing>) if (typeof saved[key] === 'boolean') out[key] = saved[key];
    }
  } catch { /* Defaults. */ }
  return out;
}

export function saveCallAudioProcessing(storage: KV | null | undefined, p: CallAudioProcessing): void {
  try { storage?.setItem(CALL_AUDIO_STORAGE_KEY, JSON.stringify(p)); } catch { /* Private mode: this call only. */ }
}

/**
 * Whether a track already applies this processing. A setting the browser
 * doesn't report counts as matching (a second capture couldn't do better).
 */
export function processingMatches(settings: Partial<Record<keyof CallAudioProcessing, boolean>>, p: CallAudioProcessing): boolean {
  return (Object.keys(p) as Array<keyof CallAudioProcessing>).every(k => settings[k] === undefined || settings[k] === p[k]);
}

/**
 * getUserMedia audio constraints for a capture of deviceId with processing p.
 * The processing is requested *exactly* [SL-P1-10]: with plain (ideal)
 * booleans Chromium may hand back an already-open capture of the same device
 * with different processing as the "best fit" (measured: a clean recording
 * capture made a later AGC-on call request come back with AGC off). Exact
 * values make it open a matching capture. Browsers that can't honour a value
 * reject with OverconstrainedError; openAudio() then retries with ideal values.
 */
export function callAudioConstraints(deviceId: string | undefined, p: CallAudioProcessing, exact = true): MediaTrackConstraints {
  const v = (b: boolean) => (exact ? { exact: b } : b);
  return { ...(deviceId ? { deviceId: { exact: deviceId } } : {}), echoCancellation: v(p.echoCancellation), noiseSuppression: v(p.noiseSuppression), autoGainControl: v(p.autoGainControl) };
}

type GetUserMedia = (c: MediaStreamConstraints) => Promise<MediaStream>;

/** Opens audio with exact processing, falling back to ideal values where the browser refuses exact ones. */
export async function openAudio(constraints: (exact: boolean) => MediaTrackConstraints, getUserMedia: GetUserMedia = c => navigator.mediaDevices.getUserMedia(c)): Promise<MediaStream> {
  try {
    return await getUserMedia({ audio: constraints(true), video: false });
  } catch (err) {
    if ((err as { name?: string })?.name !== 'OverconstrainedError') throw err;
    return getUserMedia({ audio: constraints(false), video: false });
  }
}

/** The raw recording capture: no browser processing (ARCHITECTURE §4.2); see micmode.ts for clean. */
export const RAW_CAPTURE_PROCESSING: Readonly<CallAudioProcessing> = { noiseSuppression: false, echoCancellation: false, autoGainControl: false };

/** getUserMedia audio constraints for the capture (recording) microphone. */
export function captureAudioConstraints(deviceId: string | undefined, p: CallAudioProcessing = RAW_CAPTURE_PROCESSING, exact = true): MediaTrackConstraints {
  return { ...callAudioConstraints(deviceId, p, exact), channelCount: 2 };
}

/** Opens the recording (capture) microphone with processing p (raw or clean, SL-P1-10). */
export function openCaptureMic(deviceId: string | undefined, p: CallAudioProcessing = RAW_CAPTURE_PROCESSING, getUserMedia?: GetUserMedia): Promise<MediaStream> {
  return openAudio(exact => captureAudioConstraints(deviceId, p, exact), getUserMedia);
}

/** This browser gave the call capture the capture mic's processing, not the one asked for. */
export class SharedProcessingError extends Error {
  constructor() { super('This browser applies microphone processing per device'); this.name = 'SharedProcessingError'; }
}

export class CallMicrophone {
  private source: MediaStreamTrack | null = null;
  private clone: MediaStreamTrack | null = null;
  private selfMuted = false;
  private hostMuted = false;
  private wanted: CallAudioProcessing = { ...DEFAULT_CALL_AUDIO_PROCESSING };
  private dedicated = false;

  /** The processing the call microphone should use. */
  get processing(): CallAudioProcessing { return { ...this.wanted }; }
  /** True when the call uses its own capture (processing differs from the recording mic). */
  get usesDedicatedCapture(): boolean { return this.dedicated; }
  /** True once this browser has refused separate call processing (see SharedProcessingError). */
  get sharesRecordingProcessing(): boolean { return this.shared; }
  private shared = false;
  /**
   * Set while the recording capture is itself processed (clean recording mic,
   * SL-P1-10). Chromium allows one processed capture per microphone (measured:
   * a second processed config of the same deviceId is refused with
   * OverconstrainedError; only a fully raw capture coexists), so when the
   * call's own capture is refused the call shares the recording capture's
   * processing instead of changing the recording.
   */
  shareWhenRefused = false;
  private following = false;
  /** True while the call uses the (processed) recording capture's processing, not its own choice. */
  get followsRecording(): boolean { return this.following; }

  /** Stops a dedicated call capture so the device can be reopened with other processing. */
  releaseCallCapture(): void {
    if (this.dedicated) this.clone?.stop();
  }

  /**
   * Makes the call track match `processing` (or the current choice) for the
   * microphone in `local`. Call before callStream() whenever the microphone
   * or the choice changes. Returns the previous call track if it was replaced.
   */
  async prepare(local: MediaStream, processing?: CallAudioProcessing, getUserMedia = (c: MediaStreamConstraints) => navigator.mediaDevices.getUserMedia(c)): Promise<MediaStreamTrack | null> {
    if (processing) this.wanted = { ...processing };
    const audio = local.getAudioTracks()[0] ?? null;
    const matches = !audio || processingMatches(audio.getSettings() as Partial<Record<keyof CallAudioProcessing, boolean>>, this.wanted);
    if (audio === this.source && this.clone && this.clone.readyState === 'live') {
      // Still sharing the clean recording capture: asking again would be refused again.
      if (this.following && this.shareWhenRefused && !matches) return null;
      if (matches === !this.dedicated && (!this.dedicated || processingMatches(this.clone.getSettings() as Partial<Record<keyof CallAudioProcessing, boolean>>, this.wanted))) return null;
    }
    let next: MediaStreamTrack | null = null;
    let follow = false;
    if (audio && !matches) {
      // Chrome refuses (or shares) a second processed capture of a device, so
      // the previous call capture goes first, and so does a clone of a
      // replaced recording capture (a moment of call silence; the recording
      // track is untouched).
      if (this.dedicated || audio !== this.source) this.clone?.stop();
      const deviceId = audio.getSettings().deviceId;
      const stream = await openAudio(exact => callAudioConstraints(deviceId, this.wanted, exact), getUserMedia);
      next = stream.getAudioTracks()[0] ?? null;
      if (next && !processingMatches(next.getSettings() as Partial<Record<keyof CallAudioProcessing, boolean>>, this.wanted)) {
        next.stop();
        next = null;
        if (!this.shareWhenRefused) {
          this.shared = true;
          throw new SharedProcessingError();
        }
        follow = true; // the call clones the clean recording capture
      }
    }
    const previous = this.clone;
    this.source = audio;
    this.dedicated = !!next;
    this.following = follow;
    this.clone = next ?? (audio ? audio.clone() : null);
    previous?.stop();
    this.apply();
    return previous;
  }

  /** A stream for the call: original video tracks + the cloned microphone. */
  callStream(local: MediaStream): MediaStream {
    const audio = local.getAudioTracks()[0] ?? null;
    if (audio !== this.source) {
      this.clone?.stop();
      this.source = audio;
      this.clone = audio ? audio.clone() : null;
      this.dedicated = false;
      this.following = false;
    }
    this.apply();
    return new MediaStream([...local.getVideoTracks(), ...(this.clone ? [this.clone] : [])]);
  }

  /** Self-mute: silences the call AND local recording of this microphone. */
  setSelfMuted(muted: boolean, recordingStreams: Array<MediaStream | null | undefined> = []): void {
    this.selfMuted = muted;
    if (!muted) this.hostMuted = false;
    if (this.source) this.source.enabled = !muted;
    for (const stream of recordingStreams) stream?.getAudioTracks().forEach(t => (t.enabled = !muted));
    this.apply();
  }

  /** Host request: silences the call only; recording continues (see ARCHITECTURE §5). */
  setHostMuted(muted: boolean): void {
    this.hostMuted = muted;
    this.apply();
  }

  /** Stops the call's microphone track (leaving the call); the next callStream() starts again. */
  release(): void {
    this.clone?.stop();
    this.clone = null;
    this.source = null;
    this.dedicated = false;
    this.following = false;
  }

  /** The cloned track currently sent to the call (null before callStream()). */
  get callTrack(): MediaStreamTrack | null { return this.clone; }
  get isSelfMuted(): boolean { return this.selfMuted; }
  get isHostMuted(): boolean { return this.hostMuted && !this.selfMuted; }
  /** Whether others in the call hear nothing from this microphone. */
  get callMuted(): boolean { return this.selfMuted || this.hostMuted; }

  private apply(): void {
    if (this.clone) this.clone.enabled = !this.callMuted;
  }
}
