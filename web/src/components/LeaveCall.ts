// Leave call for guests [SL-P1-6]. A red "Leave call" control in the dock
// (the host keeps "End meeting"), a confirmation when this device still has
// recording to upload, and a post-leave screen with Rejoin and live upload
// progress. Uploads are page-wide (OPFS journal + one scheduler), so they keep
// going after the call is left for as long as the tab stays open.
//
// The host gets no "Leave and keep the meeting running": the hub has no
// host-less meeting (no host transfer; waiting guests could not be admitted
// and a take could not be stopped), so the host ends the meeting instead.
import { confirmDialog } from './Rooms';
import { leaveConfirmation, leftUploadFraction, leftUploadText, nextLeaveState, uploadsPending, type LeaveState, type UploadBacklog } from '../lib/leave';
import '../leave.css';

export interface LeaveCallHost {
  isHost(): boolean;
  backlog(): UploadBacklog;
  /** Tear down call media and signaling; recording finalization and uploads continue. */
  leave(): Promise<void>;
  /** Reacquire devices and reconnect; admission arrives later via admitted(). */
  rejoin(): Promise<'connecting' | 'ended'>;
  toast(message: string): void;
}

export class LeaveCall {
  readonly button: HTMLButtonElement;
  private state: LeaveState = 'in-call';
  private screen: HTMLElement | null = null;
  private timer: number | null = null;

  constructor(private readonly host: LeaveCallHost) {
    const btn = document.createElement('button');
    btn.id = 'leave-btn';
    btn.type = 'button';
    btn.className = 'btn btn-end';
    btn.textContent = 'Leave call';
    btn.hidden = true;
    btn.addEventListener('click', () => void this.requestLeave());
    this.button = btn;
  }

  mount(dock: HTMLElement): void { dock.querySelector('.transport-actions')?.append(this.button) ?? dock.append(this.button); }

  /** Admission (first join or rejoin): guests get Leave; the screen goes away. */
  admitted(host: boolean): void {
    this.button.hidden = host;
    if (this.state === 'rejoining') this.transition({ type: 'admitted' });
  }

  /** The host ended the meeting while we were in it or had left. */
  meetingEnded(): void {
    this.button.hidden = true;
    if (this.state !== 'in-call') this.transition({ type: 'meeting-ended' });
  }

  /** A rejoin attempt was refused (e.g. removed by the host). */
  rejoinFailed(): void { if (this.state === 'rejoining') this.transition({ type: 'rejoin-failed' }); }

  get current(): LeaveState { return this.state; }

  private async requestLeave(): Promise<void> {
    if (this.state !== 'in-call' || this.host.isHost()) return;
    const backlog = this.host.backlog();
    this.transition({ type: 'leave', pending: uploadsPending(backlog) });
    if ((this.state as LeaveState) === 'confirming') {
      const copy = leaveConfirmation(backlog)!;
      const ok = await confirmDialog(copy.title, copy.body, copy.confirmLabel, true);
      this.transition({ type: ok ? 'confirm' : 'cancel' });
      if (!ok) { this.button.focus(); return; }
    }
    this.button.disabled = true;
    try { await this.host.leave(); } finally { this.button.disabled = false; }
  }

  private async rejoin(): Promise<void> {
    if (this.state !== 'left') return;
    this.transition({ type: 'rejoin' });
    try {
      if (await this.host.rejoin() === 'ended') this.transition({ type: 'meeting-ended' });
    } catch (err) {
      this.host.toast(`Couldn’t rejoin: ${err instanceof Error ? err.message : String(err)}`);
      this.transition({ type: 'rejoin-failed' });
    }
  }

  private transition(event: Parameters<typeof nextLeaveState>[1]): void {
    const next = nextLeaveState(this.state, event);
    if (next === this.state) return;
    this.state = next;
    if (next === 'left' || next === 'rejoining' || next === 'ended') this.renderScreen();
    else if (next === 'in-call') this.removeScreen();
  }

  private renderScreen(): void {
    if (!this.screen) {
      for (const id of ['green-room', 'waiting-room', 'studio-view']) { const el = document.getElementById(id); if (el) el.style.display = 'none'; }
      document.getElementById('meeting-ended')?.remove();
      const sec = document.createElement('section');
      sec.id = 'left-call';
      sec.className = 'left-call';
      sec.innerHTML = `<div class="workspace-heading"><div>
        <h1 tabindex="-1" data-left-heading></h1><p data-left-intro></p></div></div>
        <div class="left-card glass-panel"><span class="field-label">Your recording</span>
          <p data-left-uploads role="status" aria-live="polite"></p>
          <div class="left-progress" data-left-progress hidden><div class="left-progress-fill"></div></div>
          <div class="left-actions"><button type="button" class="btn btn-primary" data-left-rejoin>Rejoin</button></div></div>`;
      sec.querySelector('[data-left-rejoin]')!.addEventListener('click', () => void this.rejoin());
      document.getElementById('main-container')?.appendChild(sec);
      this.screen = sec;
      this.timer = window.setInterval(() => this.renderUploads(), 1000);
      sec.querySelector<HTMLElement>('[data-left-heading]')?.focus();
    }
    const heading = this.screen.querySelector<HTMLElement>('[data-left-heading]')!;
    const intro = this.screen.querySelector<HTMLElement>('[data-left-intro]')!;
    const rejoin = this.screen.querySelector<HTMLButtonElement>('[data-left-rejoin]')!;
    if (this.state === 'ended') {
      heading.innerHTML = 'The meeting has ended<span class="heading-point">.</span>';
      intro.textContent = 'Keep this tab open until your recording has reached the host.';
      rejoin.hidden = true;
    } else {
      heading.innerHTML = 'You left the call<span class="heading-point">.</span>';
      intro.textContent = 'Nobody in the call can see or hear you now. You can rejoin while the meeting is still on.';
      rejoin.hidden = false;
      rejoin.disabled = this.state === 'rejoining';
      rejoin.textContent = this.state === 'rejoining' ? 'Rejoining…' : 'Rejoin';
    }
    this.renderUploads();
  }

  private renderUploads(): void {
    if (!this.screen) return;
    const backlog = this.host.backlog();
    this.screen.querySelector<HTMLElement>('[data-left-uploads]')!.textContent = leftUploadText(backlog);
    const bar = this.screen.querySelector<HTMLElement>('[data-left-progress]')!;
    const fraction = leftUploadFraction(backlog);
    bar.hidden = fraction === null || !uploadsPending(backlog);
    if (fraction !== null) bar.querySelector<HTMLElement>('.left-progress-fill')!.style.width = `${Math.round(fraction * 100)}%`;
  }

  private removeScreen(): void {
    if (this.timer !== null) { clearInterval(this.timer); this.timer = null; }
    this.screen?.remove();
    this.screen = null;
  }
}
