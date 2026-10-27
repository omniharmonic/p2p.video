// Recording microphone mode [SL-P1-10]: "I'm wearing headphones" plus the
// participant's own choice (Automatic / Raw / Clean) in the green room, the
// same controls in the Devices popover during the call (locked while a take
// records), and the resolved mode as a status line. The decision logic is in
// lib/micmode.ts; main.ts reopens the recording microphone when the
// resolved processing changes and reports the measured state.
import '../call-audio.css';
import { escapeHtml as h } from '../lib/html';
import {
  RECORDING_PROCESSING, isRecordingMicMode, loadRecordingMicMode, recordingMicExplanation, recordingMicLabel,
  recordingMicState, resolveRecordingMic, saveRecordingMicMode,
} from '../lib/micmode';
import type { CallAudioProcessing } from '../lib/callmic';
import type { RecordingMicMode, RecordingMicState, ResolvedMicMode } from '../lib/protocol';

export interface RecordingMicHost {
  /** Non-empty while the mode can't change (a take is arming, recording or stopping). */
  lockedReason(): string;
  /** Reopens the recording microphone if its processing must change. */
  apply(): Promise<void>;
  toast(message: string): void;
}

function storage(): Storage | null {
  try { return window.localStorage; } catch { return null; }
}

const MODE_OPTIONS: Array<[RecordingMicMode, string]> = [
  ['auto', 'Automatic (recommended)'],
  ['raw', 'Untouched (wear headphones)'],
  ['clean', 'Cleaned up (echo and noise removed)'],
];

export class RecordingMic {
  readonly greenRoom: HTMLElement;
  readonly inCall: HTMLElement;
  private own: RecordingMicMode;
  private headphones = false;
  private headphonesTouched = false;
  private sessionDefault: RecordingMicMode = 'auto';
  private heuristicActive = true;
  private busy = false;
  /** The last measured state of the recording track (set by main.ts). */
  private measured: RecordingMicState | null = null;
  /** Each view's "I'm wearing headphones" row; Ready to join shows it outside Recording options. */
  private readonly headphoneRows = new Map<HTMLElement, HTMLElement>();

  constructor(private readonly host: RecordingMicHost) {
    this.own = loadRecordingMicMode(storage());
    this.greenRoom = this.build('gr');
    this.inCall = this.build('call');
    this.inCall.classList.add('call-audio-processing');
    // Ready to join keeps the headphones question visible (it decides untouched
    // or cleaned up); the rest lives under Recording options [SL-UX-2a].
    document.getElementById('ready-headphones')?.append(this.headphoneRows.get(this.greenRoom)!);
    this.render();
  }

  private build(suffix: 'gr' | 'call'): HTMLElement {
    const el = document.createElement('section');
    el.className = 'recording-mic';
    el.dataset.view = suffix;
    const id = (s: string) => (suffix === 'gr' ? s : `${s}-call`);
    el.innerHTML = `${suffix === 'call' ? '<h3>Recording microphone</h3>' : ''}
      <label class="pcm-option recording-mic-headphones"><input type="checkbox" id="${id('headphones-toggle')}"><span><strong>I’m wearing headphones</strong><span>Your recorded voice then stays untouched, best for editing. Without headphones, echo removal keeps other voices out of it.</span></span></label>
      <div class="form-group recording-mic-mode"><label for="${id('recmic-mode')}">Microphone for recording</label><select id="${id('recmic-mode')}">${MODE_OPTIONS.map(([v, l]) => `<option value="${v}">${h(l)}</option>`).join('')}</select></div>
      <p class="field-hint recording-mic-status" id="${id('recmic-status')}" role="status" aria-live="polite"></p>`;
    const box = el.querySelector<HTMLInputElement>('input[type=checkbox]')!;
    const select = el.querySelector<HTMLSelectElement>('select')!;
    this.headphoneRows.set(el, el.querySelector<HTMLElement>('.recording-mic-headphones')!);
    box.addEventListener('change', () => void this.change(() => { this.headphones = box.checked; this.headphonesTouched = true; }));
    select.addEventListener('change', () => void this.change(() => {
      if (isRecordingMicMode(select.value)) { this.own = select.value; saveRecordingMicMode(storage(), this.own); }
    }));
    return el;
  }

  /** The participant's own setting. */
  get mode(): RecordingMicMode { return this.own; }
  get wearingHeadphones(): boolean { return this.headphones; }
  get resolved(): ResolvedMicMode { return resolveRecordingMic(this.own, this.sessionDefault, this.headphones).resolved; }
  /** Processing the recording microphone should open with. */
  get processing(): CallAudioProcessing { return { ...RECORDING_PROCESSING[this.resolved] }; }

  /** What to report for a recording track with these measured settings. */
  state(settings: MediaTrackSettings | undefined): RecordingMicState {
    return recordingMicState(this.own, this.sessionDefault, this.headphones, (settings ?? {}) as Partial<Record<keyof CallAudioProcessing, boolean>>);
  }

  /** main.ts reports the recording track's measured state after (re)opening it. */
  setMeasured(state: RecordingMicState | null): void { this.measured = state; this.render(); }

  /** Pre-flight's headphone label heuristic pre-checks the box until the person decides. */
  setHeuristic(looksLikeHeadphones: boolean): void {
    if (!this.heuristicActive || this.headphonesTouched || looksLikeHeadphones === this.headphones) return;
    void this.change(() => { this.headphones = looksLikeHeadphones; });
  }

  /** After joining, only explicit choices change the mode (no surprise reopen mid-call). */
  freezeHeuristic(): void { this.heuristicActive = false; }

  setSessionDefault(mode: unknown): void {
    const next = isRecordingMicMode(mode) ? mode : 'auto';
    if (next === this.sessionDefault) return;
    void this.change(() => { this.sessionDefault = next; }, true);
  }

  /** Re-renders the lock state (take started or stopped). */
  refreshLock(): void { this.render(); }

  /** Applies again (e.g. a deferred host default once a take stops). */
  async reapply(): Promise<void> {
    if (this.host.lockedReason()) return;
    try { await this.host.apply(); } catch (err) { this.host.toast(`Couldn’t change the recording microphone: ${err instanceof Error ? err.message : String(err)}`); }
    this.render();
  }

  private async change(mutate: () => void, fromHost = false): Promise<void> {
    const before = this.resolved;
    const snapshot = { own: this.own, headphones: this.headphones };
    const locked = this.host.lockedReason();
    if (locked && !fromHost) { this.render(); this.host.toast(locked); return; }
    mutate();
    if (this.resolved === before || locked) { this.render(); return; } // a host change waits for the take to end
    this.busy = true;
    this.render();
    try {
      await this.host.apply();
      if (fromHost) this.host.toast(`The host set recording microphones to ${this.sessionDefault}: yours is now ${this.resolved}.`);
    } catch (err) {
      // The person's own change is undone; the host's default stays what the server says.
      if (!fromHost) { this.own = snapshot.own; this.headphones = snapshot.headphones; }
      this.host.toast(`Couldn’t change the recording microphone: ${err instanceof Error ? err.message : String(err)}`);
    } finally {
      this.busy = false;
      this.render();
    }
  }

  private render(): void {
    const locked = this.host.lockedReason();
    const resolved = this.resolved;
    const label = recordingMicLabel(this.measured ?? this.state(undefined));
    const pendingHost = this.measured && this.measured.resolved !== resolved;
    for (const el of [this.greenRoom, this.inCall]) {
      el.dataset.resolved = resolved;
      const row = this.headphoneRows.get(el)!;
      const box = row.querySelector<HTMLInputElement>('input[type=checkbox]')!;
      const select = el.querySelector<HTMLSelectElement>('select')!;
      box.checked = this.headphones;
      select.value = this.own;
      box.disabled = select.disabled = this.busy || !!locked;
      // Headphones only matter for Automatic without a host default.
      row.classList.toggle('is-moot', this.own !== 'auto' || this.sessionDefault !== 'auto');
      const status = el.querySelector<HTMLElement>('.recording-mic-status')!;
      status.textContent = `${label}. ${recordingMicExplanation(resolved)}${this.sessionDefault !== 'auto' && this.own === 'auto' ? ` The host set ${this.sessionDefault} for everyone.` : ''}${pendingHost ? ' Changes after this take.' : ''}${locked ? ` ${locked}` : ''}`;
      status.dataset.mode = this.measured?.resolved ?? resolved;
    }
  }
}
