import { ICONS } from './icons';

export interface DevicePickerHost {
  /** The capture stream (call + recording source). */
  localStream(): MediaStream | null;
  /**
   * Why camera/microphone switching is unavailable right now, or '' when it
   * is allowed. While a take records (or arms) inputs are locked: see
   * docs/DECISIONS.md "Device switching while recording".
   */
  inputsLocked(): string;
  /** Replace the camera or microphone in the call without rejoining. */
  switchInput(kind: 'videoinput' | 'audioinput', deviceId: string): Promise<void>;
  toast(message: string): void;
}

type SinkElement = HTMLMediaElement & { setSinkId?: (id: string) => Promise<void>; sinkId?: string };

/** Whether this browser can route call audio to a chosen output device. */
export function canSelectSpeaker(): boolean {
  return typeof HTMLMediaElement !== 'undefined' && 'setSinkId' in HTMLMediaElement.prototype;
}

// In-call device settings (ROADMAP P1): camera, microphone and speaker output.
// Camera/mic are swapped with RTCRtpSender.replaceTrack (no renegotiation, no
// rejoin). Speaker output uses setSinkId on every remote media element in the
// grid, including ones added later.
export class DevicePicker {
  readonly button: HTMLButtonElement;
  readonly popover: HTMLElement;
  private readonly camera: HTMLSelectElement;
  private readonly mic: HTMLSelectElement;
  private readonly speaker: HTMLSelectElement;
  private readonly note: HTMLElement;
  private sinkId = '';
  private busy = false;

  constructor(private readonly host: DevicePickerHost, private readonly grid: HTMLElement) {
    this.button = document.createElement('button');
    this.button.type = 'button';
    this.button.id = 'devices-btn';
    this.button.className = 'btn btn-secondary collab-btn collab-compact';
    this.button.setAttribute('aria-label', 'Devices');
    this.button.title = 'Camera, microphone and speaker';
    this.button.setAttribute('aria-expanded', 'false');
    this.button.setAttribute('aria-controls', 'device-popover');
    this.button.innerHTML = `<span class="collab-icon" aria-hidden="true">${ICONS.sliders}</span><span class="collab-label">Devices</span>`;
    this.button.onclick = () => this.toggle();

    this.popover = document.createElement('div');
    this.popover.id = 'device-popover';
    this.popover.className = 'device-popover';
    this.popover.setAttribute('role', 'dialog');
    this.popover.setAttribute('aria-label', 'Call devices');
    this.popover.hidden = true;
    this.popover.innerHTML = `
      <div class="device-popover-head"><h2>Devices</h2><button type="button" class="icon-button" data-close aria-label="Close devices">×</button></div>
      <label class="device-field"><span>Camera</span><select id="call-camera-select"></select></label>
      <label class="device-field"><span>Microphone</span><select id="call-mic-select"></select></label>
      <label class="device-field" data-speaker-field><span>Speaker</span><select id="call-speaker-select"></select></label>
      <p class="device-note" role="status"></p>`;
    this.camera = this.popover.querySelector('#call-camera-select')!;
    this.mic = this.popover.querySelector('#call-mic-select')!;
    this.speaker = this.popover.querySelector('#call-speaker-select')!;
    this.note = this.popover.querySelector('.device-note')!;
    this.popover.querySelector<HTMLButtonElement>('[data-close]')!.onclick = () => this.close(true);
    document.addEventListener('keydown', e => { if (e.key === 'Escape' && !this.popover.hidden) { e.stopPropagation(); this.close(true); } });
    this.camera.onchange = () => void this.switch('videoinput', this.camera.value);
    this.mic.onchange = () => void this.switch('audioinput', this.mic.value);
    this.speaker.onchange = () => void this.setSpeaker(this.speaker.value);
    if (!canSelectSpeaker()) (this.popover.querySelector('[data-speaker-field]') as HTMLElement).hidden = true;
    navigator.mediaDevices?.addEventListener?.('devicechange', () => { if (!this.popover.hidden) void this.refresh(); });
    // Remote media added later (new participants, screen shares) follows the chosen output.
    new MutationObserver(() => { if (this.sinkId) void this.applySink(); }).observe(grid, { childList: true, subtree: true });
    document.addEventListener('pointerdown', e => {
      if (!this.popover.hidden && !this.popover.contains(e.target as Node) && !this.button.contains(e.target as Node)) this.close(false);
    });
  }

  toggle(): void {
    if (this.popover.hidden) void this.open();
    else this.close(true);
  }

  async open(): Promise<void> {
    this.popover.hidden = false;
    this.button.setAttribute('aria-expanded', 'true');
    this.button.classList.add('active');
    await this.refresh();
    // Inputs are disabled while recording; focus what can be used.
    (this.camera.disabled ? this.popover.querySelector<HTMLButtonElement>('[data-close]')! : this.camera).focus();
  }

  close(restoreFocus: boolean): void {
    if (this.popover.hidden) return;
    this.popover.hidden = true;
    this.button.setAttribute('aria-expanded', 'false');
    this.button.classList.remove('active');
    if (restoreFocus) this.button.focus();
  }

  /** Re-evaluates the recording lock (call when a take starts or stops). */
  refreshLock(): void {
    const locked = this.host.inputsLocked();
    this.camera.disabled = this.mic.disabled = !!locked || this.busy;
    this.note.textContent = locked || (canSelectSpeaker() ? 'Changes apply to the call right away.' : 'Changes apply to the call right away. This browser uses the system speaker.');
    this.popover.classList.toggle('device-locked', !!locked);
  }

  async refresh(): Promise<void> {
    let devices: MediaDeviceInfo[] = [];
    try { devices = await navigator.mediaDevices.enumerateDevices(); } catch { /* Keep the lists empty. */ }
    const stream = this.host.localStream();
    const currentCam = stream?.getVideoTracks()[0]?.getSettings().deviceId ?? '';
    const currentMic = stream?.getAudioTracks()[0]?.getSettings().deviceId ?? '';
    const fill = (select: HTMLSelectElement, kind: MediaDeviceKind, current: string, fallback: string) => {
      select.replaceChildren();
      for (const d of devices.filter(d => d.kind === kind)) {
        const o = document.createElement('option');
        o.value = d.deviceId;
        o.textContent = d.label || fallback;
        select.append(o);
      }
      if ([...select.options].some(o => o.value === current)) select.value = current;
    };
    fill(this.camera, 'videoinput', currentCam, 'Camera');
    fill(this.mic, 'audioinput', currentMic, 'Microphone');
    fill(this.speaker, 'audiooutput', this.sinkId || 'default', 'Speaker');
    if (this.speaker.options.length === 0) {
      const o = document.createElement('option');
      o.value = '';
      o.textContent = 'System default';
      this.speaker.append(o);
    }
    this.refreshLock();
  }

  private async switch(kind: 'videoinput' | 'audioinput', deviceId: string): Promise<void> {
    const locked = this.host.inputsLocked();
    if (locked) {
      this.host.toast(locked);
      await this.refresh();
      return;
    }
    this.busy = true;
    this.refreshLock();
    try {
      await this.host.switchInput(kind, deviceId);
      this.host.toast(kind === 'videoinput' ? 'Camera switched' : 'Microphone switched');
    } catch (error) {
      this.host.toast(`Could not switch ${kind === 'videoinput' ? 'camera' : 'microphone'}: ${error instanceof Error ? error.message : String(error)}`);
    } finally {
      this.busy = false;
      await this.refresh();
    }
  }

  /** Ready to join's speaker choice: applied to the call when it starts (no toast). */
  presetSpeaker(deviceId: string): void {
    this.sinkId = deviceId;
    void this.applySink().catch(() => { /* applied again as call media arrives */ });
  }

  private async setSpeaker(deviceId: string): Promise<void> {
    this.sinkId = deviceId;
    try {
      await this.applySink();
      this.host.toast('Speaker changed');
    } catch (error) {
      this.host.toast(`Could not change speaker: ${error instanceof Error ? error.message : String(error)}`);
    }
  }

  private async applySink(): Promise<void> {
    if (!canSelectSpeaker()) return;
    const elements = [...this.grid.querySelectorAll<SinkElement>('video, audio')].filter(el => !el.muted);
    await Promise.all(elements.map(el => (el.sinkId === this.sinkId ? undefined : el.setSinkId?.(this.sinkId))));
  }
}
