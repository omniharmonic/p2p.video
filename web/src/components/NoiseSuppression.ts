// Call audio processing switches in the Devices popover [SL-P1-8]: noise
// suppression (Meet's "noise cancellation"), echo cancellation and automatic
// gain for what others hear. They apply to the call microphone only; the
// local recording and the uncompressed PCM track keep their own settings. The
// choice is remembered on this device.
import '../call-audio.css';
import { loadCallAudioProcessing, saveCallAudioProcessing, type CallAudioProcessing } from '../lib/callmic';

export interface NoiseSuppressionHost {
  /** Applies the processing to the call microphone (replaceTrack, no renegotiation). */
  apply(processing: CallAudioProcessing): Promise<void>;
  /** True when this browser applies these settings to the recording mic too. */
  sharedWithRecording(): boolean;
  /**
   * The call's actual processing while it shares a clean recording mic's
   * capture (one processed capture per microphone in Chromium, SL-P1-10);
   * null when the call has its own.
   */
  followsRecording?(): CallAudioProcessing | null;
  toast(message: string): void;
}

const ROWS: Array<{ key: keyof CallAudioProcessing; label: string; hint: string }> = [
  { key: 'noiseSuppression', label: 'Noise suppression', hint: 'Filters background noise from what others hear' },
  { key: 'echoCancellation', label: 'Echo cancellation', hint: 'Keep on unless everyone wears headphones' },
  { key: 'autoGainControl', label: 'Automatic volume', hint: 'Evens out your level in the call' },
];

function storage(): Storage | null {
  try { return window.localStorage; } catch { return null; }
}

export class NoiseSuppression {
  readonly element: HTMLElement;
  private value: CallAudioProcessing;
  private busy = false;

  constructor(private readonly host: NoiseSuppressionHost) {
    this.value = loadCallAudioProcessing(storage());
    const section = document.createElement('section');
    section.className = 'call-audio-processing';
    section.setAttribute('aria-labelledby', 'call-audio-heading');
    section.innerHTML = `<h3 id="call-audio-heading">Call audio</h3>
      ${ROWS.map(r => `<div class="call-audio-row"><span class="call-audio-text"><span id="call-audio-${r.key}-label">${r.label}</span><small>${r.hint}</small></span>
        <button type="button" class="switch" role="switch" id="call-audio-${r.key}" data-key="${r.key}" aria-labelledby="call-audio-${r.key}-label" aria-checked="false"><span></span></button></div>`).join('')}
      <p class="call-audio-note" data-note>Only affects the call. Your recording microphone has its own setting above.</p>`;
    for (const btn of section.querySelectorAll<HTMLButtonElement>('[data-key]')) {
      btn.addEventListener('click', () => void this.toggle(btn.dataset.key as keyof CallAudioProcessing));
    }
    this.element = section;
    this.render();
  }

  /** The remembered choice (applied by main.ts before the first offer). */
  get processing(): CallAudioProcessing { return { ...this.value }; }

  private async toggle(key: keyof CallAudioProcessing): Promise<void> {
    if (this.busy) return;
    const next = { ...this.value, [key]: !this.value[key] };
    this.busy = true;
    this.render();
    try {
      await this.host.apply(next);
      this.value = next;
      saveCallAudioProcessing(storage(), next);
      const row = ROWS.find(r => r.key === key)!;
      this.host.toast(`${row.label} ${next[key] ? 'on' : 'off'} for the call`);
    } catch (err) {
      this.host.toast(`Couldn’t change call audio: ${err instanceof Error ? err.message : String(err)}`);
    } finally {
      this.busy = false;
      this.render();
    }
  }

  /** Re-renders after the call microphone changed (e.g. the recording mic mode). */
  refresh(): void { this.render(); }

  private render(): void {
    const follows = this.host.followsRecording?.() ?? null;
    this.element.dataset.follows = String(!!follows);
    this.element.querySelector<HTMLElement>('[data-note]')!.textContent = follows
      ? 'Your recording mic is clean, and this browser allows one processed capture per microphone, so the call uses the same echo cancellation and noise suppression. Wear headphones (raw recording mic) to set call audio separately.'
      : this.host.sharedWithRecording()
        ? 'This browser applies these to your recording microphone too, so they lock while a take records.'
        : 'Only affects the call. Your recording microphone has its own setting above.';
    for (const btn of this.element.querySelectorAll<HTMLButtonElement>('[data-key]')) {
      const key = btn.dataset.key as keyof CallAudioProcessing;
      btn.setAttribute('aria-checked', String(follows ? follows[key] : this.value[key]));
      btn.disabled = this.busy || !!follows;
    }
  }
}
