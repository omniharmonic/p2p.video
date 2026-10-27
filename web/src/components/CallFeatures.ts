import { ChatPanel } from './ChatPanel';
import { ParticipantsPanel } from './ParticipantsPanel';
import { Reactions } from './Reactions';
import { Shortcuts } from './Shortcuts';
import { ICONS } from './icons';
import { denialMessage, handQueue } from '../lib/callstate';
import {
  TypeActionError,
  TypeChat,
  TypeChatHistory,
  TypeHand,
  TypeMuteRequest,
  TypeReaction,
  TypeRemove,
  TypeRoomLock,
  TypeRoomState,
  TypeRoster,
  type ActionErrorPayload,
  type AdmissionPayload,
  type ChatHistoryPayload,
  type ChatMessage,
  type Envelope,
  type MuteRequestPayload,
  type NetStatsPayload,
  type ReactionPayload,
  type RoomStatePayload,
  type RosterEntry,
  type RosterPayload,
} from '../lib/protocol';

export interface CallFeaturesHost {
  send(env: Envelope): void;
  selfId(): string;
  isHost(): boolean;
  /** This device joined as a producer [SL-P1-11]. */
  isProducer?(): boolean;
  toggleMic(): void;
  toggleCam(): void;
  /** Host asked us to mute: mute the call microphone only (recording continues). */
  hostMuteCall(by: string): void;
  /** The `c` shortcut: show or hide live captions [SL-P4-2]. */
  toggleCaptions?(): void;
  toast(message: string): void;
  /** Latest connection report per participant, for the people panel [SL-P0-8]. */
  netStats?(participantId: string): NetStatsPayload | undefined;
}

type PanelName = 'chat' | 'people';

// In-call collaboration UI (ROADMAP P1): chat, people + host controls, raise
// hand, reactions, recording consent banner and keyboard shortcuts. main.ts
// forwards signaling messages to handle() and admission results to
// onAdmission(); everything else lives here so main.ts stays small.
export class CallFeatures {
  private readonly chat: ChatPanel;
  private readonly people: ParticipantsPanel;
  private readonly reactions: Reactions;
  private readonly shortcuts: Shortcuts;
  private readonly panel: HTMLElement;
  private readonly dockGroup: HTMLElement;
  private readonly handBtn: HTMLButtonElement;
  private readonly chatBtn: HTMLButtonElement;
  private readonly peopleBtn: HTMLButtonElement;
  private readonly chatBadge: HTMLElement;
  private readonly peopleCount: HTMLElement;
  private readonly banner: HTMLElement;
  private readonly bannerDetail: HTMLElement;
  private readonly waitingNotice: HTMLElement;
  private readonly announcer: HTMLElement;
  private readonly confirmDialog: HTMLDialogElement;
  private readonly removedDialog: HTMLDialogElement;
  private openPanelName: PanelName | null = null;
  private roster: RosterPayload = { participants: [], locked: false };
  private room: RoomStatePayload | null = null;
  private inCall = false;
  private knownHands = new Set<string>();

  constructor(private readonly host: CallFeaturesHost) {
    this.chat = new ChatPanel({
      send: text => host.send({ t: TypeChat, payload: { text } }),
      selfId: () => host.selfId(),
      onUnreadChange: n => this.setUnread(n),
      onClose: () => this.closePanel(true),
    });
    this.people = new ParticipantsPanel({
      selfId: () => host.selfId(),
      isHost: () => host.isHost(),
      isProducer: () => !!host.isProducer?.(),
      onMute: p => {
        host.send({ t: TypeMuteRequest, payload: { participantId: p.participantId } });
        host.toast(`Asked ${p.name || 'participant'} to mute. Their own recording keeps running.`);
      },
      onRemove: p => this.confirmRemove(p),
      onLowerHand: p => host.send({ t: TypeHand, payload: { participantId: p.participantId, raised: false } }),
      onLock: locked => host.send({ t: TypeRoomLock, payload: { locked } }),
      onClose: () => this.closePanel(true),
      netStats: host.netStats ? id => host.netStats?.(id) : undefined,
    });
    this.reactions = new Reactions({ send: emoji => host.send({ t: TypeReaction, payload: { emoji } }) });
    this.shortcuts = new Shortcuts({
      enabled: () => this.inCall,
      run: action => {
        if (action === 'toggle-mic') host.toggleMic();
        else if (action === 'toggle-cam') host.toggleCam();
        else if (action === 'toggle-captions') host.toggleCaptions?.();
        else this.toggleHand();
      },
    });

    // Dock controls.
    this.dockGroup = document.createElement('div');
    this.dockGroup.className = 'transport-collab';
    this.dockGroup.hidden = true;
    this.dockGroup.setAttribute('role', 'group');
    this.dockGroup.setAttribute('aria-label', 'Call collaboration');
    this.handBtn = this.dockButton('hand-btn', 'hand', 'Raise hand');
    this.handBtn.classList.add('collab-compact');
    this.handBtn.setAttribute('aria-label', 'Raise hand');
    this.handBtn.title = 'Raise hand';
    this.handBtn.setAttribute('aria-pressed', 'false');
    this.handBtn.onclick = () => this.toggleHand();
    this.chatBtn = this.dockButton('chat-btn', 'chat', 'Chat');
    this.chatBtn.setAttribute('aria-controls', 'call-panel');
    this.chatBtn.setAttribute('aria-expanded', 'false');
    this.chatBadge = document.createElement('span');
    this.chatBadge.className = 'collab-badge';
    this.chatBadge.hidden = true;
    this.chatBtn.append(this.chatBadge);
    this.chatBtn.onclick = () => this.togglePanel('chat');
    this.peopleBtn = this.dockButton('people-btn', 'people', 'People');
    this.peopleBtn.setAttribute('aria-controls', 'call-panel');
    this.peopleBtn.setAttribute('aria-expanded', 'false');
    this.peopleCount = document.createElement('span');
    this.peopleCount.className = 'collab-count';
    this.peopleBtn.append(this.peopleCount);
    this.peopleBtn.onclick = () => this.togglePanel('people');
    const helpBtn = this.dockButton('shortcuts-btn', 'keys', 'Shortcuts');
    helpBtn.classList.add('collab-icon-only');
    helpBtn.setAttribute('aria-label', 'Keyboard shortcuts');
    helpBtn.title = 'Keyboard shortcuts (?)';
    helpBtn.onclick = () => this.shortcuts.showHelp();
    const reactWrap = document.createElement('div');
    reactWrap.className = 'reaction-wrap';
    reactWrap.append(this.reactions.button, this.reactions.popover);
    this.dockGroup.append(this.handBtn, reactWrap, this.chatBtn, this.peopleBtn, helpBtn);

    // Side panel (desktop) / slide-over (mobile).
    this.panel = document.createElement('aside');
    this.panel.id = 'call-panel';
    this.panel.className = 'call-panel';
    this.panel.setAttribute('aria-label', 'Call panel');
    this.panel.append(this.chat.element, this.people.element);
    this.chat.element.hidden = this.people.element.hidden = true;
    this.panel.addEventListener('keydown', e => {
      if (e.key === 'Escape') { e.stopPropagation(); this.closePanel(true); }
    });

    // Recording consent banner + screen-reader announcements.
    this.banner = document.createElement('div');
    this.banner.id = 'recording-banner';
    this.banner.className = 'recording-banner';
    this.banner.hidden = true;
    this.banner.innerHTML = '<span class="recording-banner-dot" aria-hidden="true"></span><strong>Recording</strong><span class="recording-banner-detail"></span>';
    this.bannerDetail = this.banner.querySelector('.recording-banner-detail')!;
    this.waitingNotice = document.createElement('div');
    this.waitingNotice.id = 'waiting-recording-notice';
    this.waitingNotice.className = 'recording-banner recording-notice';
    this.waitingNotice.setAttribute('role', 'status');
    this.waitingNotice.hidden = true;
    this.announcer = document.createElement('div');
    this.announcer.id = 'recording-announcer';
    this.announcer.className = 'visually-hidden';
    this.announcer.setAttribute('aria-live', 'assertive');
    this.announcer.setAttribute('aria-atomic', 'true');

    this.confirmDialog = document.createElement('dialog');
    this.confirmDialog.className = 'confirm-dialog';
    this.confirmDialog.setAttribute('aria-labelledby', 'confirm-title');
    this.removedDialog = document.createElement('dialog');
    this.removedDialog.id = 'removed-dialog';
    this.removedDialog.className = 'confirm-dialog';
    this.removedDialog.setAttribute('role', 'alertdialog');
    this.removedDialog.setAttribute('aria-labelledby', 'removed-title');
    this.removedDialog.setAttribute('aria-describedby', 'removed-detail');
  }

  /** Insert the UI into the static page. Safe to call once. */
  mount(): void {
    const studio = document.getElementById('studio-view');
    const dock = studio?.querySelector('.dock');
    dock?.insertBefore(this.dockGroup, dock.querySelector('.transport-record'));
    studio?.querySelector('.stage-with-inspector')?.append(this.panel);
    studio?.prepend(this.banner);
    document.querySelector('#waiting-room .workspace-heading')?.after(this.waitingNotice);
    document.body.append(this.announcer, this.confirmDialog, this.removedDialog);
    // Tiles are rebuilt on renegotiation; keep hand badges on them.
    const grid = document.getElementById('video-grid');
    if (grid) new MutationObserver(() => this.paintTiles()).observe(grid, { childList: true });
  }

  /** Admission results: in-call state, and honest messages for denials. */
  onAdmission(adm: AdmissionPayload): void {
    this.inCall = adm.status === 'admitted';
    this.dockGroup.hidden = !this.inCall;
    if (!this.inCall) this.closePanel(false);
    if (adm.status === 'denied') {
      this.waitingNotice.hidden = this.banner.hidden = true;
      if (adm.reason === 'removed' || adm.reason === 'room_locked') this.showDenied(adm.reason);
    }
  }

  /** Returns true when the message was a collaboration message. */
  handle(env: Envelope): boolean {
    switch (env.t) {
      case TypeChat:
        this.chat.add(env.payload as ChatMessage);
        return true;
      case TypeChatHistory:
        this.chat.setHistory((env.payload as ChatHistoryPayload).messages ?? []);
        return true;
      case TypeRoster:
        this.applyRoster(env.payload as RosterPayload);
        return true;
      case TypeRoomState:
        this.applyRoomState(env.payload as RoomStatePayload);
        return true;
      case TypeReaction: {
        const r = env.payload as ReactionPayload;
        const self = r.participantId === this.host.selfId();
        Reactions.float(this.tileFor(r.participantId ?? ''), r.emoji, self ? 'You' : r.name || '');
        return true;
      }
      case TypeMuteRequest: {
        const m = env.payload as MuteRequestPayload;
        if (m.participantId === this.host.selfId()) this.host.hostMuteCall(m.by || 'The host');
        return true;
      }
      case TypeActionError:
        this.host.toast((env.payload as ActionErrorPayload).message);
        return true;
    }
    return false;
  }

  private dockButton(id: string, icon: keyof typeof ICONS, label: string): HTMLButtonElement {
    const b = document.createElement('button');
    b.type = 'button';
    b.id = id;
    b.className = 'btn btn-secondary collab-btn';
    const i = document.createElement('span');
    i.setAttribute('aria-hidden', 'true');
    i.className = 'collab-icon';
    i.innerHTML = ICONS[icon]; // static, trusted markup
    const l = document.createElement('span');
    l.className = 'collab-label';
    l.textContent = label;
    b.append(i, l);
    return b;
  }

  private selfEntry(): RosterEntry | undefined {
    return this.roster.participants.find(p => p.participantId === this.host.selfId());
  }

  private toggleHand(): void {
    if (!this.inCall) return;
    this.host.send({ t: TypeHand, payload: { raised: !this.selfEntry()?.handRaised } });
  }

  private togglePanel(name: PanelName): void {
    if (this.openPanelName === name) this.closePanel(true);
    else this.openPanel(name);
  }

  private openPanel(name: PanelName): void {
    this.reactions.close(false);
    this.openPanelName = name;
    this.panel.classList.add('open');
    this.chat.element.hidden = name !== 'chat';
    this.people.element.hidden = name !== 'people';
    this.chatBtn.setAttribute('aria-expanded', String(name === 'chat'));
    this.peopleBtn.setAttribute('aria-expanded', String(name === 'people'));
    this.chatBtn.classList.toggle('active', name === 'chat');
    this.peopleBtn.classList.toggle('active', name === 'people');
    if (name === 'chat') { this.chat.markRead(); this.chat.focusInput(); }
    else this.people.focusFirst();
  }

  private closePanel(restoreFocus: boolean): void {
    const was = this.openPanelName;
    if (!was) return;
    this.openPanelName = null;
    this.panel.classList.remove('open');
    this.chat.element.hidden = this.people.element.hidden = true;
    for (const b of [this.chatBtn, this.peopleBtn]) { b.setAttribute('aria-expanded', 'false'); b.classList.remove('active'); }
    if (restoreFocus) (was === 'chat' ? this.chatBtn : this.peopleBtn).focus();
  }

  private setUnread(n: number): void {
    this.chatBadge.hidden = n === 0;
    this.chatBadge.textContent = n > 99 ? '99+' : String(n);
    this.chatBtn.setAttribute('aria-label', n ? `Chat, ${n} unread message${n === 1 ? '' : 's'}` : 'Chat');
  }

  /** Re-renders the people list (connection bars / upload backlog changed). */
  refreshPeople(): void {
    this.people.update(this.roster);
  }

  private applyRoster(roster: RosterPayload): void {
    this.roster = roster;
    this.people.update(roster);
    this.peopleCount.textContent = String(roster.participants.length);
    this.peopleBtn.setAttribute('aria-label', `People, ${roster.participants.length} in the call`);
    const raised = !!this.selfEntry()?.handRaised;
    this.handBtn.setAttribute('aria-pressed', String(raised));
    this.handBtn.classList.toggle('active', raised);
    this.handBtn.querySelector('.collab-label')!.textContent = raised ? 'Lower hand' : 'Raise hand';
    // Tell the host when someone new raises a hand.
    const now = new Set(handQueue(roster.participants).map(p => p.participantId));
    if (this.host.isHost()) {
      for (const p of roster.participants) {
        if (now.has(p.participantId) && !this.knownHands.has(p.participantId) && p.participantId !== this.host.selfId()) {
          this.host.toast(`${p.name || 'A participant'} raised their hand`);
        }
      }
    }
    this.knownHands = now;
    this.paintTiles();
  }

  private applyRoomState(state: RoomStatePayload): void {
    const was = this.room?.recording;
    this.room = state;
    const take = state.takeNumber ? ` (recording ${state.takeNumber})` : '';
    this.banner.hidden = !state.recording;
    this.bannerDetail.textContent = 'Everyone is recorded on their own device.';
    this.banner.setAttribute('aria-label', `Recording${take}`);
    this.waitingNotice.hidden = !state.recording && !state.locked;
    this.waitingNotice.replaceChildren();
    if (state.recording) {
      const dot = document.createElement('span');
      dot.className = 'recording-banner-dot';
      dot.setAttribute('aria-hidden', 'true');
      const strong = document.createElement('strong');
      strong.textContent = 'This meeting is being recorded';
      const detail = document.createElement('span');
      detail.className = 'recording-banner-detail';
      detail.textContent = 'When the host lets you in, your camera and microphone are recorded on this device and sent to the host.';
      this.waitingNotice.append(dot, strong, detail);
    } else if (state.locked) {
      const strong = document.createElement('strong');
      strong.textContent = 'The host has locked this call';
      const detail = document.createElement('span');
      detail.className = 'recording-banner-detail';
      detail.textContent = 'You can keep waiting; the host can still let you in.';
      this.waitingNotice.append(strong, detail);
    }
    this.waitingNotice.classList.toggle('recording-notice-locked', !state.recording && state.locked);
    if (was !== undefined && was !== state.recording) {
      this.announcer.textContent = state.recording ? `Recording started${take}. This call is being recorded.` : 'Recording stopped.';
    } else if (was === undefined && state.recording) {
      this.announcer.textContent = 'This call is being recorded.';
    }
  }

  private tileFor(participantId: string): Element | null {
    if (!participantId) return null;
    if (participantId === this.host.selfId()) return document.getElementById('local-tile');
    return document.querySelector(`#video-grid .tile[data-participant-id="${CSS.escape(participantId)}"]`);
  }

  private paintTiles(): void {
    const raised = new Set(this.roster.participants.filter(p => p.handRaised).map(p => p.participantId));
    const producers = new Set(this.roster.participants.filter(p => p.role === 'producer').map(p => p.participantId));
    const tiles = [document.getElementById('local-tile'), ...document.querySelectorAll<HTMLElement>('#video-grid .tile[data-participant-id]')];
    for (const tile of tiles) {
      if (!tile) continue;
      const id = tile.id === 'local-tile' ? this.host.selfId() : tile.dataset.participantId || '';
      let badge = tile.querySelector<HTMLElement>('.tile-hand');
      if (raised.has(id)) {
        if (!badge) {
          badge = document.createElement('span');
          badge.className = 'tile-hand';
          badge.innerHTML = '<span aria-hidden="true">✋</span> Hand raised';
          tile.append(badge);
        }
      } else badge?.remove();
      // Producer badge: in the call, never recorded [SL-P1-11].
      let role = tile.querySelector<HTMLElement>('.tile-role');
      if (producers.has(id)) {
        if (!role) {
          role = document.createElement('span');
          role.className = 'tile-role tile-role-producer';
          role.textContent = 'Producer';
          role.title = 'Producer: in the call, not recorded';
          tile.append(role);
        }
      } else role?.remove();
    }
  }

  private confirmRemove(p: RosterEntry): void {
    const d = this.confirmDialog;
    const name = p.name || 'this participant';
    d.replaceChildren();
    const head = document.createElement('h2');
    head.id = 'confirm-title';
    head.textContent = `Remove ${name}?`;
    const body = document.createElement('p');
    body.textContent = 'They leave the call now. Any of their recording that hasn’t reached you yet stays on their device. They can ask to join again unless the call is locked.';
    const row = document.createElement('div');
    row.className = 'confirm-actions';
    const cancel = document.createElement('button');
    cancel.type = 'button';
    cancel.className = 'btn btn-quiet';
    cancel.textContent = 'Cancel';
    cancel.onclick = () => d.close();
    const ok = document.createElement('button');
    ok.type = 'button';
    ok.className = 'btn btn-remove';
    ok.dataset.confirm = 'remove';
    ok.textContent = 'Remove';
    ok.onclick = () => {
      this.host.send({ t: TypeRemove, payload: { participantId: p.participantId } });
      d.close();
    };
    row.append(cancel, ok);
    d.append(head, body, row);
    const back = document.activeElement as HTMLElement | null;
    d.addEventListener('close', () => back?.isConnected && back.focus(), { once: true });
    d.showModal();
    cancel.focus();
  }

  private showDenied(reason: 'removed' | 'room_locked'): void {
    const msg = denialMessage(reason);
    const d = this.removedDialog;
    d.replaceChildren();
    const head = document.createElement('h2');
    head.id = 'removed-title';
    head.textContent = reason === 'removed' ? 'You were removed from the call' : 'This call is locked';
    const body = document.createElement('p');
    body.id = 'removed-detail';
    body.textContent = msg.detail;
    const row = document.createElement('div');
    row.className = 'confirm-actions';
    const ok = document.createElement('button');
    ok.type = 'button';
    ok.className = 'btn btn-primary';
    ok.textContent = 'OK';
    ok.onclick = () => d.close();
    row.append(ok);
    d.append(head, body, row);
    if (!d.open) d.showModal();
    ok.focus();
  }
}
