// Settings / Fine-Tuning Drawer for p2p.video
// Controls bitrate, codecs, conformed frame rate, audio geometry, GCC-PHAT threshold, and WebCodecs engine.

export interface SessionSettingsData {
  video_bitrate_bps: number;
  video_codec: string;
  target_fps: number;
  audio_sample_rate: number;
  audio_bit_depth: number;
  xcorr_threshold: number;
  xcorr_window_ms: number;
  engine: string;
  /** Editor package video: compact H.264 or faster, larger ProRes Proxy [SL-P0-9]. */
  export_codec?: 'h264' | 'prores_proxy';
  /** Recording mic default for everyone: auto (headphones decide), raw or clean [SL-P1-10]. */
  recording_mic_mode?: 'auto' | 'raw' | 'clean';
}

export class SettingsDrawer {
  private container: HTMLElement;
  private isOpen = false;
  private sessionId: string;
  private token: string;
  private settings: SessionSettingsData;
  private onSettingsChange: (settings: SessionSettingsData) => void;

  constructor(
    sessionId: string,
    initialSettings: Partial<SessionSettingsData> = {},
    onChange: (settings: SessionSettingsData) => void,
    token = ''
  ) {
    this.sessionId = sessionId;
    this.token = token;
    this.onSettingsChange = onChange;
    this.settings = {
      video_bitrate_bps: initialSettings.video_bitrate_bps || 12_000_000,
      video_codec: initialSettings.video_codec || 'h264',
      target_fps: initialSettings.target_fps || 30.0,
      audio_sample_rate: initialSettings.audio_sample_rate || 48000,
      audio_bit_depth: initialSettings.audio_bit_depth || 24,
      xcorr_threshold: initialSettings.xcorr_threshold || 0.65,
      xcorr_window_ms: initialSettings.xcorr_window_ms || 500,
      engine: 'mediarecorder',
      export_codec: initialSettings.export_codec === 'prores_proxy' ? 'prores_proxy' : 'h264',
      recording_mic_mode: micMode(initialSettings.recording_mic_mode),
    };

    this.container = document.createElement('div');
    this.container.id = 'settings-drawer-panel';
    this.container.className = 'settings-drawer-backdrop';
    this.render();
    document.body.appendChild(this.container);
    this.container.addEventListener('click',event=>{if(event.target===this.container)this.close();});
    document.addEventListener('keydown',event=>{if(event.key==='Escape'&&this.isOpen)this.close();});
  }

  public setSession(id:string, token:string):void {this.sessionId=id;this.token=token;}

  public setSettings(settings: Partial<SessionSettingsData>): void {
    this.settings = {...this.settings,...settings,engine:'mediarecorder'};
    if(!['h264','vp9'].includes(this.settings.video_codec))this.settings.video_codec='h264';
    if(this.settings.export_codec!=='prores_proxy')this.settings.export_codec='h264';
    this.settings.recording_mic_mode=micMode(this.settings.recording_mic_mode);
    this.render();
  }

  public getSettings(): SessionSettingsData {
    return { ...this.settings };
  }

  public open(): void {
    this.isOpen = true;
    this.container.classList.add('open');
    this.container.querySelector<HTMLButtonElement>('#drawer-close-btn')?.focus();
  }

  public close(): void {
    this.isOpen = false;
    this.container.classList.remove('open');
    document.getElementById('settings-btn')?.focus();
  }

  public toggle(): void {
    if (this.isOpen) this.close();
    else this.open();
  }

  private render(): void {
    const mbps = (this.settings.video_bitrate_bps / 1_000_000).toFixed(0);

    this.container.innerHTML = `
      <div class="drawer-panel" role="dialog" aria-modal="true" aria-labelledby="settings-heading">
        <div class="drawer-header"><div class="drawer-title-group"><h3 id="settings-heading">Recording quality</h3></div><button id="drawer-close-btn" class="close-icon-btn" aria-label="Close settings">×</button></div>
        <div class="drawer-content">
          <p class="field-hint">Capture targets apply to the next recording. Export frame rate applies during preparation; originals are preserved.</p>
          <div class="drawer-section"><span class="section-label">Recording engine</span><div class="tactile-toggle-group" id="engine-toggle-group"><button class="tactile-pill active" data-engine="mediarecorder">MediaRecorder / Container recording</button><button class="tactile-pill" data-engine="webcodecs" disabled>WebCodecs / Not available</button></div><p class="field-hint">Actual format depends on browser support. The recorded container is reported with each track.</p></div>
          <div class="drawer-section"><div class="slider-header"><label class="section-label" for="bitrate-slider">Video bitrate target</label><span class="slider-value" id="bitrate-val-display">${mbps} Mbps</span></div><input type="range" id="bitrate-slider" min="5" max="50" step="1" value="${mbps}" class="range-slider"><div class="slider-ticks"><span>5 Mbps</span><span>50 Mbps</span></div><p class="field-hint">A target, not a guaranteed measured bitrate. Higher values need more storage and upload bandwidth.</p></div>
          <div class="drawer-section"><label class="section-label" for="codec-select">Preferred recording format</label><select id="codec-select"><option value="h264" ${this.settings.video_codec==='h264'?'selected':''}>H.264 / MP4 preferred</option><option value="vp9" ${this.settings.video_codec==='vp9'?'selected':''}>VP9 / WebM preferred</option><option value="av1" disabled>AV1 / Not available</option></select><p class="field-hint">Unsupported preferences fall back to a supported browser container.</p></div>
          <div class="drawer-section"><label class="section-label" for="fps-select">Editor output frame rate</label><select id="fps-select">${[[23.976,'23.976 / 24000÷1001'],[24,'24'],[29.97,'29.97 / 30000÷1001'],[30,'30'],[60,'60']].map(([value,label])=>`<option value="${value}" ${Math.abs(this.settings.target_fps-Number(value))<.01?'selected':''}>${label} fps</option>`).join('')}</select><p class="field-hint">CFR conversion during export. This does not change the camera's measured capture rate.</p></div>
          <div class="drawer-section"><label class="section-label" for="export-codec-select">Editor video format</label><select id="export-codec-select"><option value="h264" ${this.settings.export_codec!=='prores_proxy'?'selected':''}>H.264 / compact</option><option value="prores_proxy" ${this.settings.export_codec==='prores_proxy'?'selected':''}>ProRes 422 Proxy / fastest, larger files</option></select><p class="field-hint">This Mac's hardware encodes one H.264 camera at a time. ProRes encodes cameras in parallel, about 4× faster with three cameras, with roughly 2–9× larger files. Originals are always available first.</p></div>
          <div class="drawer-section"><label class="section-label" for="recording-mic-default">Recording microphone</label><select id="recording-mic-default"><option value="auto" ${this.settings.recording_mic_mode==='auto'?'selected':''}>Automatic / raw with headphones, clean without</option><option value="raw" ${this.settings.recording_mic_mode==='raw'?'selected':''}>Always raw / no browser processing</option><option value="clean" ${this.settings.recording_mic_mode==='clean'?'selected':''}>Always clean / echo cancellation + noise suppression</option></select><p class="field-hint">Raw gives the editor untouched isolated tracks but needs headphones. Clean keeps the voices from someone's speakers out of their track. A guest can still choose raw or clean for themselves. Applies between takes.</p></div>
          <div class="drawer-section"><span class="section-label">Audio delivery</span><div class="settings-static">48 kHz / 24-bit PCM WAV<p class="field-hint">Enable the optional uncompressed microphone track in Source setup for Float32 WAV capture. Otherwise audio comes from the compressed camera recording; conversion cannot restore lost fidelity.</p></div><select id="audio-rate-select" hidden disabled><option value="48000">48000</option></select><select id="audio-depth-select" hidden disabled><option value="24">24</option></select></div>
          <div class="drawer-section"><span class="section-label">Synchronization</span><div class="settings-static">Clock estimate / Review required<p class="field-hint">Fine acoustic alignment is not enabled. Verify synchronization in the editor before delivery.</p></div><input id="xcorr-slider" type="hidden" value="${this.settings.xcorr_threshold}"><span id="xcorr-val-display" hidden></span><input id="window-slider" type="hidden" value="${this.settings.xcorr_window_ms}"><span id="window-val-display" hidden></span></div>
          <p id="settings-save-status" class="field-hint" role="status"></p>
        </div>
        <div class="drawer-footer"><button id="save-settings-btn" class="btn btn-primary btn-block">Save session settings</button></div>
      </div>
    `;

    this.attachEvents();
  }

  private attachEvents(): void {
    const closeBtn = this.container.querySelector('#drawer-close-btn');
    if (closeBtn) closeBtn.addEventListener('click', () => this.close());


    // Engine toggle
    const engineBtns = this.container.querySelectorAll('#engine-toggle-group button:not(:disabled)');
    engineBtns.forEach((btn) => {
      btn.addEventListener('click', () => {
        engineBtns.forEach((b) => b.classList.remove('active'));
        btn.classList.add('active');
        const eng = 'mediarecorder';
        this.settings.engine = eng;
      });
    });

    // Bitrate
    const bitrateSlider = this.container.querySelector('#bitrate-slider') as HTMLInputElement;
    const bitrateVal = this.container.querySelector('#bitrate-val-display');
    if (bitrateSlider && bitrateVal) {
      bitrateSlider.addEventListener('input', () => {
        const val = parseInt(bitrateSlider.value, 10);
        bitrateVal.textContent = `${val} Mbps`;
        this.settings.video_bitrate_bps = val * 1_000_000;
      });
    }

    // Codec
    const codecSelect = this.container.querySelector('#codec-select') as HTMLSelectElement;
    if (codecSelect) {
      codecSelect.addEventListener('change', () => {
        this.settings.video_codec = codecSelect.value;
      });
    }

    const exportCodecSelect = this.container.querySelector('#export-codec-select') as HTMLSelectElement;
    if (exportCodecSelect) {
      exportCodecSelect.addEventListener('change', () => {
        this.settings.export_codec = exportCodecSelect.value === 'prores_proxy' ? 'prores_proxy' : 'h264';
      });
    }

    const micSelect = this.container.querySelector('#recording-mic-default') as HTMLSelectElement;
    if (micSelect) micSelect.addEventListener('change', () => { this.settings.recording_mic_mode = micMode(micSelect.value); });

    // FPS
    const fpsSelect = this.container.querySelector('#fps-select') as HTMLSelectElement;
    if (fpsSelect) {
      fpsSelect.addEventListener('change', () => {
        this.settings.target_fps = parseFloat(fpsSelect.value);
      });
    }

    // Audio rate
    const audioRateSelect = this.container.querySelector('#audio-rate-select') as HTMLSelectElement;
    if (audioRateSelect) {
      audioRateSelect.addEventListener('change', () => {
        this.settings.audio_sample_rate = parseInt(audioRateSelect.value, 10);
      });
    }

    // Audio depth
    const audioDepthSelect = this.container.querySelector('#audio-depth-select') as HTMLSelectElement;
    if (audioDepthSelect) {
      audioDepthSelect.addEventListener('change', () => {
        this.settings.audio_bit_depth = parseInt(audioDepthSelect.value, 10);
      });
    }

    // Xcorr threshold
    const xcorrSlider = this.container.querySelector('#xcorr-slider') as HTMLInputElement;
    const xcorrVal = this.container.querySelector('#xcorr-val-display');
    if (xcorrSlider && xcorrVal) {
      xcorrSlider.addEventListener('input', () => {
        const val = parseFloat(xcorrSlider.value);
        xcorrVal.textContent = val.toFixed(2);
        this.settings.xcorr_threshold = val;
      });
    }

    // Xcorr window
    const windowSlider = this.container.querySelector('#window-slider') as HTMLInputElement;
    const windowVal = this.container.querySelector('#window-val-display');
    if (windowSlider && windowVal) {
      windowSlider.addEventListener('input', () => {
        const val = parseInt(windowSlider.value, 10);
        windowVal.textContent = `±${val} ms`;
        this.settings.xcorr_window_ms = val;
      });
    }

    // Save button
    const saveBtn = this.container.querySelector('#save-settings-btn');
    if (saveBtn) {
      saveBtn.addEventListener('click', async () => {
        if (await this.persistSettings()) {this.onSettingsChange(this.settings);this.close();}
        else {const status=this.container.querySelector('#settings-save-status');if(status)status.textContent='Settings were not saved. Check host authorization and connection.';}
      });
    }
  }

  public async persistSettings(): Promise<boolean> {
    try {
      const res = await fetch(`/api/v1/sessions/${this.sessionId}/settings`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json', Authorization:`Bearer ${this.token}` },
        body: JSON.stringify(this.settings),
      });
      return res.ok;
    } catch (err) {
      console.warn('Failed to persist session settings to backend:', err);
      return false;
    }
  }
}

function micMode(v: unknown): 'auto' | 'raw' | 'clean' {
  return v === 'raw' || v === 'clean' ? v : 'auto';
}
