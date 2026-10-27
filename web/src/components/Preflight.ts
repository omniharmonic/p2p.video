import { escapeHtml as h } from '../lib/html';
import { recordingMicLabel } from '../lib/micmode';
import { OPFSStorage, type StorageBackend } from '../lib/opfs';
import type { DashboardParticipantState, PreflightItem } from '../lib/protocol';
import {
  PREFLIGHT_PLANNED_MINUTES, blockingProblems, connectionPathFromStats, evaluateCamera, evaluateHeadphones,
  evaluateMic, evaluateOPFS, evaluatePath, evaluatePersist, evaluatePower, evaluateResolution, evaluateStorage,
  overallStatus, remoteWorst,
} from '../lib/preflight';

/**
 * Pre-flight "ready to record" checklist [SL-P0-5]. Shown in the green room
 * before joining and as a studio chip/popover afterwards; the host's popover
 * also lists every participant's checklist from the dashboard. Probes are
 * read-only taps on the capture stream: nothing here touches recorders,
 * OPFS journals or uploads.
 */
export interface PreflightOptions {
  getStream(): MediaStream | null;
  /** A visible element playing the local camera, used to count decoded frames. */
  getPreviewVideo(): HTMLVideoElement | null;
  getPeer(): RTCPeerConnection | null;
  /** Expected OPFS write rate for one take on this device. */
  bytesPerSecond(): number;
  inCall(): boolean;
  /** Periodic checks pause while a take is recording. */
  isRecording(): boolean;
  /** Called with the new checklist whenever it changes. */
  onReport?(items: PreflightItem[]): void;
  /** Host only: ask admitted participants to re-check and report now. */
  requestRefresh?(items: PreflightItem[]): void;
  /** Items computed elsewhere, appended to every run (the recording mic mode, SL-P1-10). */
  extraItems?(): PreflightItem[];
  /** False for a producer: its own checklist never blocks a take it isn't in (SL-P1-11). */
  recordsLocally?(): boolean;
}

type BatteryManagerLike = { charging: boolean; level: number };
const PERIOD_MS = 5_000;
const SAMPLE_MS = 1_000;
type FrameCallbackVideo = HTMLVideoElement & { requestVideoFrameCallback?: (cb: () => void) => number };
const presentedCounters = new WeakMap<HTMLVideoElement, () => number>();
/** Frames presented by a video element so far (requestVideoFrameCallback), or null where unsupported. */
function presentedFrames(video: HTMLVideoElement): (() => number) | null {
  const v = video as FrameCallbackVideo;
  if (typeof v.requestVideoFrameCallback !== 'function') return null;
  let read = presentedCounters.get(video);
  if (!read) {
    let count = 0;
    const tick = () => { count++; v.requestVideoFrameCallback!(tick); };
    v.requestVideoFrameCallback(tick);
    read = () => count;
    presentedCounters.set(video, read);
  }
  return read;
}

/** Upper bound on waiting for participants' fresh reports when Record is pressed. */
const REFRESH_WAIT_MS = 4_000;
const sleep = (ms: number) => new Promise(r => setTimeout(r, ms));
const STATUS_TEXT: Record<string, string> = { ok: 'Ready', warn: 'Check', fail: 'Problem', info: 'Info' };

export class Preflight {
  public items: PreflightItem[] = [];
  private participants: DashboardParticipantState[] = [];
  private panel: HTMLElement | null = null;
  private chip: HTMLButtonElement | null = null;
  private popover: HTMLElement | null = null;
  private confirmEl: HTMLElement | null = null;
  private timer: number | null = null;
  private running: Promise<PreflightItem[]> | null = null;
  private followUp: Promise<PreflightItem[]> | null = null;
  private followUpPersist = false;
  private lastReport = '';
  private audio: { ctx: AudioContext; analyser: AnalyserNode; source: MediaStreamAudioSourceNode; track: MediaStreamTrack } | null = null;
  private opfs: { ok: boolean; error?: string; backend?: StorageBackend } | null = null;
  private persistRequested = false;
  private isHost = false;
  /** Hub time of the host's latest refresh request, from the dashboard. */
  private refreshAtMs = 0;
  /** Ready to join's one-line summary is expanded to the full list. */
  private panelOpen = false;

  constructor(private readonly opts: PreflightOptions) {}

  /** Full checklist panel, e.g. in the green room. */
  public mountPanel(container: HTMLElement, before?: Element | null): void {
    const panel = document.createElement('div');
    panel.id = 'preflight-panel';
    panel.className = 'preflight-panel';
    panel.setAttribute('role', 'region');
    panel.setAttribute('aria-label', 'Ready to record');
    container.insertBefore(panel, before ?? null);
    this.panel = panel;
    panel.addEventListener('click', e => {
      const target = e.target as HTMLElement;
      if (target.closest('[data-preflight-recheck]')) void this.run({ requestPersist: true });
      else if (target.closest('[data-preflight-toggle]')) {
        this.panelOpen = !this.panelOpen;
        this.render();
        if (this.panelOpen) void this.run({ requestPersist: true });
      }
    });
    this.render();
  }

  /** Compact chip + popover for the studio header. */
  public mountChip(anchor: HTMLElement): void {
    const chip = document.createElement('button');
    chip.id = 'preflight-chip';
    chip.type = 'button';
    chip.className = 'btn btn-quiet preflight-chip';
    chip.setAttribute('aria-expanded', 'false');
    chip.setAttribute('aria-controls', 'preflight-popover');
    const popover = document.createElement('div');
    popover.id = 'preflight-popover';
    popover.className = 'preflight-popover';
    popover.hidden = true;
    anchor.prepend(chip);
    anchor.append(popover);
    chip.onclick = () => {
      popover.hidden = !popover.hidden;
      chip.setAttribute('aria-expanded', String(!popover.hidden));
      this.render();
      if (!popover.hidden) void this.run({ requestPersist: true });
    };
    popover.addEventListener('click', e => {
      if ((e.target as HTMLElement).closest('[data-preflight-recheck]')) void this.run({ requestPersist: true });
    });
    this.chip = chip;
    this.popover = popover;
    this.render();
  }

  public setHost(isHost: boolean): void { this.isHost = isHost; this.render(); }

  /** Items changed outside a probe (e.g. the recording mic mode): re-report now. */
  public refreshExtras(): void {
    if (!this.items.length) return;
    this.items = [...this.items.filter(it => !(this.opts.extraItems?.() ?? []).some(x => x.id === it.id)), ...(this.opts.extraItems?.() ?? [])];
    this.render();
    const report = JSON.stringify(this.items);
    if (report !== this.lastReport) { this.lastReport = report; this.opts.onReport?.(this.items); }
  }

  /** Participants a take would record (pending guests and producers are not in the take). */
  private recorded(): DashboardParticipantState[] { return this.participants.filter(p => p.status === 'admitted' && p.role !== 'producer'); }

  /** Host only: dashboard participants carry their latest checklists. */
  public setParticipants(participants: DashboardParticipantState[], selfId: string, refreshAtMs = 0): void {
    this.refreshAtMs = refreshAtMs;
    this.participants = participants.filter(p => p.participantId !== selfId && p.status !== 'denied' && p.status !== 'disconnected');
    this.render();
  }

  public start(): void {
    if (this.timer !== null) return;
    void this.run();
    this.timer = window.setInterval(() => { if (!this.opts.isRecording() && document.visibilityState === 'visible') void this.run(); }, PERIOD_MS);
  }

  public stop(): void {
    if (this.timer !== null) { clearInterval(this.timer); this.timer = null; }
  }

  /**
   * Runs every probe. A request made while a run is in flight gets one fresh
   * follow-up run (shared by all such callers), so state that changed during
   * the current run (new devices, a "Check again" click) is always observed.
   */
  public run(opts: { requestPersist?: boolean } = {}): Promise<PreflightItem[]> {
    if (opts.requestPersist) this.persistRequested = false;
    if (this.running) {
      this.followUpPersist ||= !!opts.requestPersist;
      this.followUp ??= this.running.catch(() => undefined).then(() => {
        const persist = this.followUpPersist;
        this.followUp = null;
        this.followUpPersist = false;
        return this.run({ requestPersist: persist });
      });
      return this.followUp;
    }
    this.running = this.probe(!!opts.requestPersist).finally(() => { this.running = null; });
    return this.running;
  }

  private async probe(requestPersist: boolean): Promise<PreflightItem[]> {
    const stream = this.opts.getStream();
    const video = stream?.getVideoTracks()[0];
    const audio = stream?.getAudioTracks()[0];
    const frames = this.frameCounter(video);
    const framesBefore = frames?.();
    const [estimate, opfs, persisted, peakDb, battery, outputs, path] = await Promise.all([
      navigator.storage?.estimate ? navigator.storage.estimate().catch(() => null) : Promise.resolve(null),
      this.checkOPFS(),
      this.checkPersist(requestPersist),
      this.samplePeakDb(audio),
      this.readBattery(),
      this.outputLabels(),
      this.readPath(),
    ]);
    const framesAfter = frames?.();
    const framesAdvancing = framesBefore == null || framesAfter == null ? null : framesAfter > framesBefore;
    const items: PreflightItem[] = [
      evaluateStorage(estimate, this.opts.bytesPerSecond()),
      evaluateOPFS(opfs.ok, opfs.error, opfs.backend),
      evaluatePersist(persisted),
      evaluateCamera({ hasTrack: !!video, live: video?.readyState === 'live', enabled: !!video?.enabled, framesAdvancing }),
      evaluateMic({ hasTrack: !!audio, live: audio?.readyState === 'live', enabled: !!audio?.enabled, peakDb }),
      evaluateResolution(video ? video.getSettings() : null),
      evaluateHeadphones({ outputs, input: audio?.label }),
      evaluatePath(path, this.opts.inCall()),
      evaluatePower(battery),
      ...(this.opts.extraItems?.() ?? []),
    ];
    this.items = items;
    this.render();
    const report = JSON.stringify(items);
    if (report !== this.lastReport) { this.lastReport = report; this.opts.onReport?.(items); }
    return items;
  }

  /**
   * Frame counter for the camera track: the track's own stats where supported
   * (Chromium), otherwise the one preview element chosen now, so a view switch
   * mid-probe (green room → waiting room → studio) can't compare two elements.
   */
  private frameCounter(track: MediaStreamTrack | undefined): (() => number | null) | null {
    if (!track) return null;
    const read = () => (track as MediaStreamTrack & { stats?: { totalFrames?: number } }).stats?.totalFrames;
    if (typeof read() === 'number') return () => read() ?? null;
    const v = this.opts.getPreviewVideo();
    if (!v) return null;
    // WebKit reports 0 decoded frames for MediaStream playback, so frames
    // presented (requestVideoFrameCallback) count too [SL-P1-12].
    const presented = presentedFrames(v);
    if (!presented && typeof v.getVideoPlaybackQuality !== 'function') return null;
    return () => (v.srcObject && v.offsetParent !== null ? Math.max(presented?.() ?? 0, v.getVideoPlaybackQuality?.().totalVideoFrames ?? 0) : null);
  }

  private async checkOPFS(): Promise<{ ok: boolean; error?: string; backend?: StorageBackend }> {
    if (this.opfs?.ok) return this.opfs;
    // The recording storage backend (opfs.ts): OPFS, OPFS via a worker, or
    // memory in a private window [SL-P1-12].
    const storage = OPFSStorage.getInstance();
    const backend = await storage.backend();
    this.opfs = backend === 'memory' ? { ok: false, error: storage.unavailableReason, backend } : { ok: true, backend };
    return this.opfs;
  }

  /** Requests persistence once per explicit check (a user gesture helps Safari/Firefox). */
  private async checkPersist(request: boolean): Promise<boolean | null> {
    if (typeof navigator.storage?.persisted !== 'function') return null;
    try {
      let persisted = await navigator.storage.persisted();
      if (!persisted && request && !this.persistRequested && typeof navigator.storage.persist === 'function') {
        this.persistRequested = true;
        persisted = await navigator.storage.persist();
      }
      return persisted;
    } catch { return false; }
  }

  /** Peak level over SAMPLE_MS in dBFS, or null if audio processing is not allowed yet. */
  private async samplePeakDb(track: MediaStreamTrack | undefined): Promise<number | null> {
    if (!track || track.readyState !== 'live' || !track.enabled || typeof AudioContext === 'undefined') { await sleep(SAMPLE_MS); return null; }
    try {
      if (!this.audio || this.audio.track !== track) {
        this.audio?.source.disconnect();
        const ctx = this.audio?.ctx ?? new AudioContext();
        const analyser = this.audio?.analyser ?? ctx.createAnalyser();
        analyser.fftSize = 2048;
        const source = ctx.createMediaStreamSource(new MediaStream([track]));
        source.connect(analyser);
        this.audio = { ctx, analyser, source, track };
      }
      const { ctx, analyser } = this.audio;
      if (ctx.state === 'suspended') await Promise.race([ctx.resume().catch(() => undefined), sleep(200)]);
      if (ctx.state !== 'running') { await sleep(SAMPLE_MS); return null; }
      const buf = new Float32Array(analyser.fftSize);
      let peak = 0;
      const end = performance.now() + SAMPLE_MS;
      while (performance.now() < end) {
        analyser.getFloatTimeDomainData(buf);
        for (let i = 0; i < buf.length; i++) peak = Math.max(peak, Math.abs(buf[i]));
        await sleep(40);
      }
      return peak > 0 ? 20 * Math.log10(peak) : -Infinity;
    } catch { return null; }
  }

  private async readBattery(): Promise<BatteryManagerLike | null> {
    const nav = navigator as Navigator & { getBattery?: () => Promise<BatteryManagerLike> };
    if (typeof nav.getBattery !== 'function') return null;
    try { const b = await nav.getBattery(); return { charging: b.charging, level: b.level }; } catch { return null; }
  }

  private async outputLabels(): Promise<string[]> {
    try { return (await navigator.mediaDevices.enumerateDevices()).filter(d => d.kind === 'audiooutput' && d.label).map(d => d.label); } catch { return []; }
  }

  private async readPath() {
    const pc = this.opts.getPeer();
    if (!pc || !this.opts.inCall()) return null;
    try { return { ...connectionPathFromStats((await pc.getStats()).values() as Iterable<{ id: string; type: string }>), state: pc.connectionState }; } catch { return null; }
  }

  /**
   * Before a take: re-checks and, if this device or any participant has a red
   * item, asks the host to confirm. Never a hard block (OPFS/storage blocks
   * are enforced by the recorders themselves).
   */
  public async confirmStart(): Promise<boolean> {
    // Ask everyone to re-check while this device does, then wait (bounded)
    // until the hub has a report from each participant made after the
    // request (both times are hub clock, so no client clock is trusted).
    const previousRefresh = this.refreshAtMs;
    const localRun = this.run({ requestPersist: true });
    if (this.opts.requestRefresh && this.recorded().length) {
      this.opts.requestRefresh(this.items);
      const deadline = performance.now() + REFRESH_WAIT_MS;
      const fresh = () => this.refreshAtMs > previousRefresh && this.recorded().every(p => (p.preflight?.receivedAtMs ?? 0) >= this.refreshAtMs);
      while (!fresh() && performance.now() < deadline) await sleep(100);
    }
    const local = await localRun;
    const problems = blockingProblems(this.opts.recordsLocally?.() === false ? [] : local, this.recorded());
    if (problems.length === 0) return true;
    return new Promise(resolve => {
      this.confirmEl?.remove();
      const el = document.createElement('div');
      el.id = 'preflight-confirm';
      el.className = 'modal-backdrop preflight-confirm';
      el.setAttribute('role', 'alertdialog');
      el.setAttribute('aria-modal', 'true');
      el.setAttribute('aria-labelledby', 'preflight-confirm-title');
      el.style.display = 'flex';
      el.innerHTML = `<div class="preflight-dialog">
        <span class="section-kicker">Pre-flight</span>
        <h2 id="preflight-confirm-title">${problems.length === 1 ? '1 problem' : `${problems.length} problems`} before recording</h2>
        <ul class="preflight-problems">${problems.map(p => `<li><strong>${h(p.who)}</strong> · ${h(p.item.label)}: ${h(p.item.detail ?? '')}</li>`).join('')}</ul>
        <div class="preflight-dialog-actions"><button type="button" class="btn btn-secondary" id="preflight-confirm-cancel">Cancel</button><button type="button" class="btn btn-danger" id="preflight-confirm-record">Record anyway</button></div>
      </div>`;
      document.body.appendChild(el);
      this.confirmEl = el;
      const done = (ok: boolean) => { el.remove(); if (this.confirmEl === el) this.confirmEl = null; resolve(ok); };
      el.querySelector<HTMLButtonElement>('#preflight-confirm-cancel')!.onclick = () => done(false);
      el.querySelector<HTMLButtonElement>('#preflight-confirm-record')!.onclick = () => done(true);
      el.addEventListener('keydown', e => { if (e.key === 'Escape') done(false); });
      el.querySelector<HTMLButtonElement>('#preflight-confirm-cancel')!.focus();
    });
  }

  private listHTML(items: PreflightItem[]): string {
    return `<ul class="preflight-list">${items.map(it => `<li class="pf-${h(it.status)}" data-preflight-id="${h(it.id)}"><span class="pf-dot" aria-hidden="true"></span><span class="pf-label">${h(it.label)}</span><span class="pf-status">${h(STATUS_TEXT[it.status] ?? it.status)}</span><span class="pf-detail">${h(it.detail ?? '')}</span></li>`).join('')}</ul>`;
  }

  private headline(overall: string): string {
    return overall === 'ok' ? 'Ready to record' : overall === 'warn' ? 'Ready, with warnings' : 'Not ready to record';
  }

  private render(): void {
    const overall = this.items.length ? overallStatus(this.items) : 'info';
    const head = this.items.length ? this.headline(overall) : 'Checking…';
    const plan = `Planned take ${PREFLIGHT_PLANNED_MINUTES / 60} h`;
    if (this.panel) {
      // Ready to join [SL-UX-2a]: one calm line ("Ready to record" or "2 things
      // to check before recording"), expanding to the full checklist.
      const toCheck = this.items.filter(it => it.status === 'fail' || it.status === 'warn').length;
      const line = !this.items.length ? 'Checking your setup…'
        : toCheck ? `${toCheck} ${toCheck === 1 ? 'thing' : 'things'} to check before recording` : 'Ready to record';
      const hadFocus = this.panel.contains(document.activeElement);
      const focusKey = (document.activeElement as HTMLElement | null)?.matches?.('[data-preflight-recheck]') ? '[data-preflight-recheck]' : '[data-preflight-toggle]';
      this.panel.dataset.overall = overall;
      this.panel.dataset.open = String(this.panelOpen);
      this.panel.innerHTML = `<button type="button" class="pf-summary" data-preflight-toggle aria-expanded="${this.panelOpen}" aria-controls="preflight-details"><span class="pf-summary-mark" aria-hidden="true"></span><span class="pf-overall pf-${overall}">${h(line)}</span><span class="pf-summary-more">${this.panelOpen ? 'Hide' : 'Details'}</span></button>
        <div class="pf-details" id="preflight-details"${this.panelOpen ? '' : ' hidden'}>${this.listHTML(this.items)}<div class="pf-details-foot"><p class="preflight-note">Planned for a recording of ${h(String(PREFLIGHT_PLANNED_MINUTES / 60))} hours. These checks are hints, not guarantees.</p><button type="button" class="btn btn-quiet btn-compact" data-preflight-recheck>Check again</button></div></div>`;
      if (hadFocus) this.panel.querySelector<HTMLElement>(focusKey)?.focus();
    }
    if (this.chip && this.popover) {
      const rank: Record<string, number> = { ok: 0, warn: 1, fail: 2 };
      const remoteRank = this.isHost ? rank[remoteWorst(this.participants)] : 0;
      const worst = (['ok', 'warn', 'fail'] as const)[Math.max(rank[overall] ?? 0, remoteRank)];
      this.chip.dataset.overall = this.items.length ? worst : 'info';
      this.chip.innerHTML = `<span class="pf-dot" aria-hidden="true"></span>Pre-flight: ${h(this.items.length ? STATUS_TEXT[worst] : 'Checking')}`;
      if (this.popover.hidden) return;
      const others = this.isHost ? this.participants.map(p => {
        const pf = p.preflight;
        const flagged = (pf?.items ?? []).filter(it => it.status === 'fail' || it.status === 'warn');
        // The recording mic mode is always shown, so the host knows whose track is raw [SL-P1-10].
        const mic = p.role === 'producer' ? 'Producer · not recorded' : pf?.recordingMic ? recordingMicLabel(pf.recordingMic) : '';
        return `<section class="preflight-participant" data-participant-id="${h(p.participantId)}"><div class="preflight-head"><strong>${h(p.name || 'Participant')}</strong><span class="pf-overall pf-${h(pf?.overall ?? 'info')}">${h(pf ? STATUS_TEXT[pf.overall ?? 'info'] : 'No report yet')}</span></div>${mic ? `<p class="preflight-recmic" data-recmic="${h(pf?.recordingMic?.resolved ?? '')}">${h(mic)}</p>` : ''}${flagged.length ? this.listHTML(flagged) : ''}</section>`;
      }).join('') : '';
      this.popover.innerHTML = `<div class="preflight-head"><strong>You</strong><span class="pf-overall pf-${overall}">${h(head)}</span><button type="button" class="btn btn-quiet" data-preflight-recheck>Check again</button></div>${this.listHTML(this.items)}${others ? `<h3 class="preflight-subhead">Participants</h3>${others}` : ''}<p class="preflight-note">${h(plan)} · checks are hints, not guarantees</p>`;
    }
  }
}
