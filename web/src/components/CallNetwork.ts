import { NetMonitor } from '../lib/netmonitor';
import { TypeNetStats, type DashboardParticipantState, type DashboardPayload, type Envelope, type NetQuality, type NetStatsPayload } from '../lib/protocol';
import type { WebRTCClient } from '../lib/rtc';
import { etaRateBps, formatBacklog, UploadScheduler } from '../lib/uploadpacer';
import { NetQualityView } from './NetQuality';

export interface CallNetworkHost {
  rtc: WebRTCClient;
  grid: HTMLElement;
  send(env: Envelope): void;
  selfId(): string;
  inCall(): boolean;
  /** A take is arming or recording on this device. */
  recording(): boolean;
  toast(message: string): void;
  /** The people panel should re-render (bars or backlog changed). */
  peopleChanged(): void;
}

/** How often the "unstable connection" toast may repeat. */
export const UNSTABLE_TOAST_INTERVAL_MS = 60_000;

/**
 * Connection quality + upload pacing in the call [SL-P0-8]: publishes this
 * device's net_stats, shows everyone's signal bars, warns the local user when
 * their connection turns poor, and owns the page's shared UploadScheduler.
 * main.ts only forwards messages, tile changes and take state.
 */
export class CallNetwork {
  readonly scheduler = new UploadScheduler();
  readonly view: NetQualityView;
  private readonly monitor: NetMonitor;
  private lastToastAt = -Infinity;
  private lastLocalLevel: NetQuality = 'unknown';
  private peopleKeys = new Map<string, string>();

  constructor(private readonly host: CallNetworkHost) {
    this.view = new NetQualityView(host.grid);
    this.monitor = new NetMonitor({
      peer: () => host.rtc.pc,
      callPolicy: () => host.rtc.callPolicy,
      setNetworkAdaptation: net => void host.rtc.setNetworkAdaptation(net),
      screenTrackIds: () => host.rtc.screenTrackIds,
      recording: () => host.recording(),
      scheduler: this.scheduler,
      onReport: (report, level) => this.onLocalReport(report, level),
    });
  }

  start(): void { this.monitor.start(); }

  /** Re-evaluate now, e.g. after Stop so the backlog drains at full speed at once. */
  kick(): void { void this.monitor.tick(); }

  /** Report for the people panel (self included). */
  netStats(id: string): NetStatsPayload | undefined {
    return id && id === this.host.selfId() ? this.view.getLocal() : this.view.get(id);
  }

  /** Tiles were added or removed. */
  tilesChanged(): void { this.view.render(); }

  /** Returns true when the envelope was a net_stats report. */
  handle(env: Envelope): boolean {
    if (env.t !== TypeNetStats) return false;
    const p = env.payload as NetStatsPayload;
    if (!p?.participantId || p.participantId === this.host.selfId()) return true;
    this.view.set(p);
    this.notePeople(p.participantId, p);
    return true;
  }

  private onLocalReport(report: NetStatsPayload, level: NetQuality): void {
    this.view.setLocal(report);
    this.notePeople(this.host.selfId(), report);
    if (this.host.inCall()) this.host.send({ t: TypeNetStats, payload: report });
    const now = performance.now();
    if (level === 'poor' && this.lastLocalLevel !== 'poor' && now - this.lastToastAt >= UNSTABLE_TOAST_INTERVAL_MS) {
      this.lastToastAt = now;
      this.host.toast(this.host.recording()
        ? 'Your connection is unstable. Others may see and hear you poorly, but your recording is safe on this device and keeps uploading.'
        : 'Your connection is unstable. Others may see and hear you poorly.');
    }
    this.lastLocalLevel = level;
  }

  // Re-render the people panel only when what it shows changes.
  private notePeople(id: string, p: NetStatsPayload): void {
    const u = p.upload;
    const key = `${p.quality}|${u && u.pendingBytes > 0 ? formatBacklog(u.pendingBytes, etaRateBps(u)) : ''}`;
    if (this.peopleKeys.get(id) === key) return;
    this.peopleKeys.set(id, key);
    this.host.peopleChanged();
  }

  /** "Uploading 18 MB · ~3 min left" for one participant, or null when caught up. */
  static participantBacklog(p: DashboardParticipantState): string | null {
    // Track reports and the net_stats snapshot arrive independently: take the larger.
    const fromTracks = Object.values(p.tracks || {}).reduce((sum, t) => sum + (t.state === 'done' ? 0 : Math.max(0, t.bufferedBytes || 0)), 0);
    const bytes = Math.max(fromTracks, p.netStats?.upload?.pendingBytes ?? 0);
    const u = p.netStats?.upload;
    return bytes > 0 ? formatBacklog(bytes, u ? etaRateBps(u) : 0) : null;
  }

  /**
   * Host dashboard: who still has recorded media to upload. The host should
   * not end the session until this is empty (the OPFS copy is the only one).
   */
  static backlogSummary(dash: DashboardPayload): string {
    const pending = (dash.participants || []).flatMap(p => {
      const text = CallNetwork.participantBacklog(p);
      return text ? [`${p.name || 'Participant'}: ${text.replace(/^Uploading /, '')}`] : [];
    });
    return pending.length ? `Still uploading, so keep the meeting open. ${pending.join("; ")}` : '';
  }
}
