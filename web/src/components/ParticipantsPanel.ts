import { handQueue, roleLabel } from '../lib/callstate';
import type { NetStatsPayload, RosterEntry, RosterPayload } from '../lib/protocol';
import { backlogChip, signalBars } from './NetQuality';

// Everyone in the call, with mic/camera/hand state. Host rows get Mute,
// Lower hand and Remove; the room lock toggle is host-only. The host enforces
// all of these by role; hiding them for guests is only presentation.
export class ParticipantsPanel {
  readonly element: HTMLElement;
  private readonly list: HTMLElement;
  private readonly queue: HTMLElement;
  private readonly queueList: HTMLElement;
  private readonly lockRow: HTMLElement;
  private readonly lockToggle: HTMLButtonElement;
  private readonly lockState: HTMLElement;
  private readonly heading: HTMLElement;
  private roster: RosterPayload = { participants: [], locked: false };

  constructor(
    private readonly opts: {
      selfId: () => string;
      isHost: () => boolean;
      /** A producer gets Mute (not the host) and Remove (guests only) [SL-P1-11]. */
      isProducer?: () => boolean;
      onMute: (p: RosterEntry) => void;
      onRemove: (p: RosterEntry) => void;
      onLowerHand: (p: RosterEntry) => void;
      onLock: (locked: boolean) => void;
      onClose: () => void;
      /** Latest connection report per participant (self included) [SL-P0-8]. */
      netStats?: (participantId: string) => NetStatsPayload | undefined;
    },
  ) {
    const el = document.createElement('section');
    el.className = 'call-panel-body people-panel';
    el.setAttribute('aria-labelledby', 'people-panel-title');
    el.innerHTML = `
      <div class="call-panel-head">
        <h2 id="people-panel-title">People</h2>
        <button type="button" class="icon-button" data-close aria-label="Close people">×</button>
      </div>
      <div class="people-lock" hidden>
        <div><strong>Lock call</strong><span class="people-lock-state">New people can ask to join</span></div>
        <button type="button" class="switch" role="switch" aria-checked="false" aria-label="Lock call to new join requests"><span></span></button>
      </div>
      <div class="hand-queue" hidden>
        <h3 class="people-subhead">Raised hands</h3>
        <ol class="hand-queue-list"></ol>
      </div>
      <h3 class="people-subhead people-count">In the meeting</h3>
      <ul class="people-list"></ul>`;
    this.element = el;
    this.list = el.querySelector('.people-list')!;
    this.queue = el.querySelector('.hand-queue')!;
    this.queueList = el.querySelector('.hand-queue-list')!;
    this.lockRow = el.querySelector('.people-lock')!;
    this.lockToggle = el.querySelector('.switch')!;
    this.lockState = el.querySelector('.people-lock-state')!;
    this.heading = el.querySelector('.people-count')!;
    el.querySelector<HTMLButtonElement>('[data-close]')!.onclick = () => this.opts.onClose();
    this.lockToggle.dataset.focusKey = 'lock';
    this.lockToggle.onclick = () => this.opts.onLock(!this.roster.locked);
  }

  focusFirst(): void {
    (this.element.querySelector<HTMLElement>('.people-list button, .switch:not([hidden])') ?? this.element.querySelector<HTMLElement>('[data-close]'))?.focus();
  }

  update(roster: RosterPayload): void {
    // Keep keyboard focus on the same control across re-renders.
    const active = document.activeElement as HTMLElement | null;
    const focusKey = active && this.element.contains(active) ? active.dataset.focusKey : undefined;
    this.roster = roster;
    const host = this.opts.isHost();
    const self = this.opts.selfId();

    this.lockRow.hidden = !host;
    this.lockToggle.setAttribute('aria-checked', String(roster.locked));
    this.lockState.textContent = roster.locked ? 'Locked. Nobody new can ask to join' : 'New people can ask to join';
    this.lockRow.classList.toggle('locked', roster.locked);

    const hands = handQueue(roster.participants);
    this.queue.hidden = hands.length === 0;
    this.queueList.replaceChildren(...hands.map(p => {
      const li = document.createElement('li');
      li.textContent = p.participantId === self ? `${p.name || 'You'} (you)` : p.name || 'Participant';
      return li;
    }));

    this.heading.textContent = `In the meeting (${roster.participants.length})`;
    const producer = !host && !!this.opts.isProducer?.();
    this.list.replaceChildren(...roster.participants.map(p => this.row(p, host, p.participantId === self, producer)));
    if (focusKey) this.element.querySelector<HTMLElement>(`[data-focus-key="${CSS.escape(focusKey)}"]`)?.focus();
  }

  private row(p: RosterEntry, host: boolean, isSelf: boolean, producer = false): HTMLElement {
    const li = document.createElement('li');
    li.className = 'people-row';
    li.dataset.participantId = p.participantId;

    const avatar = document.createElement('span');
    avatar.className = 'people-avatar';
    avatar.setAttribute('aria-hidden', 'true');
    avatar.textContent = (p.name || '?').trim().charAt(0).toUpperCase() || '?';

    const who = document.createElement('div');
    who.className = 'people-who';
    const name = document.createElement('strong');
    name.textContent = (p.name || 'Participant') + (isSelf ? ' (you)' : '');
    const role = document.createElement('span');
    role.className = `people-role people-role-${p.role === 'producer' ? 'producer' : p.role === 'host' ? 'host' : 'guest'}`;
    role.textContent = roleLabel(p.role) + (p.role === 'producer' ? ' · not recorded' : '');
    who.append(name, role);
    const net = this.opts.netStats?.(p.participantId);
    if (this.opts.netStats) who.append(signalBars(net, 'net-bars people-net'));

    const state = document.createElement('div');
    state.className = 'people-state';
    const chip = (cls: string, text: string, on: boolean) => {
      const s = document.createElement('span');
      s.className = `people-chip ${cls}${on ? ' on' : ''}`;
      s.textContent = text;
      return s;
    };
    if (p.handRaised) state.append(chip('chip-hand', '✋ Hand raised', true));
    state.append(chip('chip-mic', p.micMuted ? 'Mic off' : 'Mic on', p.micMuted));
    state.append(chip('chip-cam', p.camOff ? 'Camera off' : 'Camera on', p.camOff));
    // The host needs to know who is still uploading before ending the session.
    const backlog = host || producer ? backlogChip(net) : null;
    if (backlog) state.append(chip('chip-upload', backlog, true));

    li.append(avatar, who, state);
    // The hub enforces the same rules; producers see only what they may do.
    const canMute = host || (producer && p.role !== 'host');
    const canRemove = (host && p.role !== 'host') || (producer && p.role !== 'host' && p.role !== 'producer');
    if ((host || canMute) && !isSelf) {
      const actions = document.createElement('div');
      actions.className = 'people-actions';
      const btn = (label: string, key: string, aria: string, fn: () => void, cls = 'btn-secondary', disabled = false) => {
        const b = document.createElement('button');
        b.type = 'button';
        b.className = `btn ${cls} btn-compact`;
        b.textContent = label;
        b.setAttribute('aria-label', aria);
        b.dataset.focusKey = `${key}:${p.participantId}`;
        b.dataset.action = key;
        b.disabled = disabled;
        b.onclick = fn;
        return b;
      };
      const label = p.name || 'participant';
      if (host && p.handRaised) actions.append(btn('Lower hand', 'lower', `Lower ${label}'s hand`, () => this.opts.onLowerHand(p), 'btn-quiet'));
      if (canMute) actions.append(btn(p.micMuted ? 'Muted' : 'Mute', 'mute', p.micMuted ? `${label} is muted` : `Mute ${label} in the call`, () => this.opts.onMute(p), 'btn-secondary', p.micMuted));
      if (canRemove) actions.append(btn('Remove', 'remove', `Remove ${label} from the call`, () => this.opts.onRemove(p), 'btn-remove'));
      li.append(actions);
    }
    return li;
  }
}
