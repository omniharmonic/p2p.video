// Call-first entry [SL-P1-5, SL-UX-2a]. Decides where "join" goes for this page load:
//  - host home (/): a Meet-style home (top bar with "Paste a link or room
//    name" + Join and a New menu; Meetings / Rooms / Recordings rail). Every
//    way into a call (New, Start on a room or meeting, Join, ?room=) opens
//    Ready to join (#green-room) first; its button starts the meeting.
//  - room guest (/join?room=&token=): Ready to join, then "Waiting for <host>
//    to start <room>" until the host is in the call, then straight on to the
//    waiting room (admission) without a reload.
// It also owns the host's End meeting control and the meeting-ended screen.
import { BRAND_MARK_SVG, SOURCE_URL, WORDMARK_HTML, annotateSourceLinks } from '../lib/brandmark';
import { escapeHtml } from '../lib/html';
import {
  ApiError, RoomsApi, entryMode, formatDuration, hostLabel, takeStatusLabel,
  type EntryMode, type HomeData, type HomeSession, type OpenedSession, type RoomLobby, type RoomView, type TakeSummary,
} from '../lib/rooms';
import { HOME_ICONS } from './homeIcons';
import { ICONS } from './icons';
import { RecordLinksPanel } from './RecordLinks';
import { RoomsPanel, confirmDialog, promptDialog } from './Rooms';
import { UpcomingPanel } from './Upcoming';

/** Thrown when the guest backs out of waiting for the host. */
export class EntryCancelled extends Error {
  constructor() { super('cancelled'); this.name = 'EntryCancelled'; }
}

export interface EntryDeps {
  toast: (message: string) => void;
  joinButton: HTMLButtonElement;
  /** The local preview stream, once devices are ready. */
  stream: () => MediaStream | null;
  api?: RoomsApi;
  /** Lobby poll interval while a guest waits for the host. */
  pollMs?: number;
  /** The link is a producer link (green room says so) [SL-P1-11]. */
  joinRole?: (role: 'producer') => void;
  /** Ready to join opened on the host home: open the camera and mic. */
  onReadyShown?: () => void;
  /** Back on the host home: release the camera and mic. */
  onHomeShown?: () => void;
}

type HomeView = 'meetings' | 'rooms' | 'recordings';

const $ = <T extends HTMLElement>(sel: string) => document.querySelector<T>(sel);
const NAME_KEY = 'studiolink.displayName';
const VIEW_KEY = 'studiolink.homeView';

/** The name this browser last joined with ("" when unknown or storage is off). */
export function rememberedName(): string {
  try { return localStorage.getItem(NAME_KEY) || ''; } catch { return ''; }
}
export function rememberName(name: string): void {
  try { if (name) localStorage.setItem(NAME_KEY, name); } catch { /* private mode */ }
}

/** "Meeting with Ben", or "Meeting" without a name (the glossary's "Untitled session"). */
export function instantTitle(name: string): string {
  const n = name.trim();
  return n ? `Meeting with ${n}` : 'Meeting';
}

/** Heading of a setup screen (Ready to join, waiting). A trailing full stop is added only to plain statements. */
export function setHeading(section: HTMLElement | null, kicker: string, title: string, body: string, index?: string): void {
  if (!section) return;
  const heading = section.querySelector<HTMLElement>('.workspace-heading');
  if (!heading) return;
  const k = heading.querySelector('.section-kicker'), h = heading.querySelector('h1'), p = heading.querySelector('p');
  if (k) k.textContent = kicker;
  // Statements about the call (waiting, locked, declined) end with a full stop; Ready to join headings do not.
  if (h) h.innerHTML = index === undefined || /[.?!…]$/.test(title) ? escapeHtml(title) : `${escapeHtml(title)}<span class="heading-point">.</span>`;
  if (p) p.textContent = body;
  const i = heading.querySelector('.workspace-index, .badge');
  if (i && index !== undefined) i.textContent = index;
}

/** "Today, 2:07 PM", "Yesterday, 9:30 AM", "Oct 3, 2:07 PM" (the same clock as Meetings). */
function whenLabel(iso: string, now = new Date()): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const time = d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  const day = Math.round((new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime() - new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime()) / 86_400_000);
  if (day === 0) return `Today, ${time}`;
  if (day === 1) return `Yesterday, ${time}`;
  return `${d.toLocaleDateString([], { month: "short", day: "numeric", ...(d.getFullYear() !== now.getFullYear() ? { year: "numeric" } : {}) })}, ${time}`;
}

/** "1 recording", "2 recordings, 1 ready to download". */
function recordingsSummary(takes: TakeSummary[]): string {
  if (!takes.length) return 'No recording';
  const done = takes.filter(t => t.status === 'complete').length;
  const base = `${takes.length} recording${takes.length === 1 ? '' : 's'}`;
  return done ? `${base}, ${done} ready to download` : `${base}, ${takeStatusLabel(takes[takes.length - 1].status).toLowerCase()}`;
}

/** A small accessible menu: button toggles, Escape and outside clicks close, arrows move. */
function bindMenu(button: HTMLButtonElement, menu: HTMLElement): { close: (focus?: boolean) => void } {
  const items = () => [...menu.querySelectorAll<HTMLElement>('[role="menuitem"]')];
  const close = (focus = false) => {
    if (menu.hidden) return;
    menu.hidden = true;
    button.setAttribute('aria-expanded', 'false');
    if (focus) button.focus();
  };
  const open = () => {
    menu.hidden = false;
    button.setAttribute('aria-expanded', 'true');
    items()[0]?.focus();
  };
  button.addEventListener('click', () => (menu.hidden ? open() : close()));
  button.addEventListener('keydown', e => { if (e.key === 'ArrowDown' && menu.hidden) { e.preventDefault(); open(); } });
  menu.addEventListener('keydown', e => {
    const list = items(), i = list.indexOf(document.activeElement as HTMLElement);
    if (e.key === 'Escape') { e.preventDefault(); close(true); }
    else if (e.key === 'ArrowDown' || e.key === 'ArrowUp') { e.preventDefault(); list[(i + (e.key === 'ArrowDown' ? 1 : -1) + list.length) % list.length]?.focus(); }
    else if (e.key === 'Home' || e.key === 'End') { e.preventDefault(); (e.key === 'Home' ? list[0] : list[list.length - 1])?.focus(); }
    else if (e.key === 'Tab') close();
  });
  document.addEventListener('click', e => { if (!menu.hidden && !menu.contains(e.target as Node) && !button.contains(e.target as Node)) close(); });
  return { close };
}

export class MeetingEntry {
  readonly mode: EntryMode;
  private readonly api: RoomsApi;
  private readonly pollMs: number;
  private rooms: RoomView[] = [];
  private target: RoomView | null = null;
  private pendingRoomId: string | undefined;
  private home: HTMLElement | null = null;
  private roomsPanel: RoomsPanel | null = null;
  private recordingsPanel: RecordLinksPanel | null = null;
  private upcomingPanel: UpcomingPanel | null = null;
  private recentList: HTMLElement | null = null;
  private liveSection: HTMLElement | null = null;
  private meetingRecordings: HTMLElement | null = null;
  private lobbyState: RoomLobby | null = null;
  private linkError = '';
  private lobbyTimer: number | null = null;
  private joining = false;
  private endBtn: HTMLButtonElement | null = null;
  private call: { sessionId: () => string; token: () => string } | null = null;

  constructor(private readonly deps: EntryDeps) {
    this.mode = entryMode(location.pathname, location.search);
    this.api = deps.api ?? new RoomsApi();
    this.pollMs = deps.pollMs ?? 2000;
    if (this.mode.kind === 'host-home') this.mountHome(this.mode.roomId);
    if (this.mode.kind === 'guest-room') this.mountGuestRoom();
    if (this.mode.kind === 'session') this.mountSession();
    this.mountEndMeeting();
  }

  static isCancel(err: unknown): boolean { return err instanceof EntryCancelled; }

  // ---------------------------------------------------------------- host home

  private mountHome(roomId?: string): void {
    document.body.classList.add('is-home');
    document.title = 'p2p.video';
    this.pendingRoomId = roomId;

    const home = document.createElement('section');
    home.id = 'home-view';
    home.className = 'home';
    home.setAttribute('aria-label', 'Home');
    home.innerHTML = `<header class="home-top">
        <a class="home-brand" href="/" aria-label="p2p.video home"><span class="brand-mark" aria-hidden="true">${BRAND_MARK_SVG}</span><span class="home-brand-name">${WORDMARK_HTML}</span></a>
        <form class="home-join" id="home-join-form" autocomplete="off">
          <label class="visually-hidden" for="home-join-input">Meeting link or room name</label>
          <span class="home-join-icon ui-icon" aria-hidden="true">${HOME_ICONS.keyboard}</span>
          <input id="home-join-input" placeholder="Paste a link or room name" spellcheck="false" autocapitalize="off">
          <button type="submit" class="btn btn-quiet home-join-btn" id="home-join-btn" disabled>Join</button>
        </form>
        <div class="home-new">
          <button type="button" class="btn btn-primary home-new-btn" id="home-new-btn" aria-haspopup="menu" aria-expanded="false" aria-controls="home-new-menu"><span class="ui-icon" aria-hidden="true">${HOME_ICONS.plus}</span><span>New</span><span class="ui-icon home-new-chevron" aria-hidden="true">${HOME_ICONS.chevronDown}</span></button>
          <div class="home-menu popover-surface" id="home-new-menu" role="menu" aria-label="New" hidden>
            <button type="button" role="menuitem" data-new="now"><span class="ui-icon" aria-hidden="true">${HOME_ICONS.video}</span><span><strong>Start a meeting now</strong><span>Check your camera, then go in</span></span></button>
            <button type="button" role="menuitem" data-new="schedule"><span class="ui-icon" aria-hidden="true">${HOME_ICONS.calendar}</span><span><strong>Schedule for later</strong><span>Pick a time and send an invite</span></span></button>
            <button type="button" role="menuitem" data-new="room"><span class="ui-icon" aria-hidden="true">${HOME_ICONS.door}</span><span><strong>Create a room</strong><span>One link for every episode</span></span></button>
            <button type="button" role="menuitem" data-new="record"><span class="ui-icon" aria-hidden="true">${HOME_ICONS.solo}</span><span><strong>Ask someone to record on their own</strong><span>They record in their own time</span></span></button>
          </div>
        </div>
        <div class="home-tools">
          <div class="home-settings">
            <button type="button" class="icon-button home-tool" id="home-settings-btn" aria-label="Settings" aria-haspopup="menu" aria-expanded="false" aria-controls="home-settings-menu"><span class="ui-icon" aria-hidden="true">${ICONS.settings}</span></button>
            <div class="home-menu home-menu-right popover-surface" id="home-settings-menu" role="menu" aria-label="Settings" hidden>
              <button type="button" role="menuitem" data-setting="devices"><span class="ui-icon" aria-hidden="true">${ICONS.sliders}</span><span><strong>Camera and microphone</strong><span>Check how you look and sound</span></span></button>
              <button type="button" role="menuitem" data-setting="feed"><span class="ui-icon" aria-hidden="true">${HOME_ICONS.calendar}</span><span><strong>Calendar feed</strong><span>See meetings in your calendar app</span></span></button>
              <button type="button" role="menuitem" data-setting="setup"><span class="ui-icon" aria-hidden="true">${HOME_ICONS.link}</span><span><strong>Private connection</strong><span>Let guests reach this Mac</span></span></button>
              <a role="menuitem" class="home-menu-link" data-source-link href="${SOURCE_URL}" target="_blank" rel="noopener noreferrer"><span class="ui-icon" aria-hidden="true">${HOME_ICONS.link}</span><span><strong>Source code</strong><span>p2p.video is open source (AGPL-3.0)</span></span></a>
            </div>
          </div>
          <button type="button" class="icon-button home-tool" id="home-help-btn" aria-label="Setup guide" title="Setup guide"><span class="ui-icon" aria-hidden="true">${ICONS.guide}</span></button>
        </div>
      </header>
      <div class="home-body">
        <nav class="home-rail" aria-label="Home sections">
          <button type="button" class="rail-item" id="rail-meetings" data-view="meetings" aria-current="page"><span class="ui-icon" aria-hidden="true">${HOME_ICONS.calendar}</span><span>Meetings</span></button>
          <button type="button" class="rail-item" id="rail-rooms" data-view="rooms"><span class="ui-icon" aria-hidden="true">${HOME_ICONS.door}</span><span>Rooms</span></button>
          <button type="button" class="rail-item" id="rail-recordings" data-view="recordings"><span class="ui-icon" aria-hidden="true">${HOME_ICONS.recordings}</span><span>Recordings</span></button>
        </nav>
        <div class="home-views" id="home-panels">
          <div class="home-view" id="home-meetings" data-view="meetings"></div>
          <div class="home-view" id="home-rooms" data-view="rooms" hidden></div>
          <div class="home-view" id="home-recordings" data-view="recordings" hidden></div>
        </div>
      </div>`;
    this.home = home;
    void annotateSourceLinks(home);

    // Meetings: the day (scheduled + live rooms), then recent meetings.
    this.upcomingPanel = new UpcomingPanel({
      roomsApi: this.api,
      toast: this.deps.toast,
      rooms: () => this.rooms,
      onStart: id => {
        const room = this.rooms.find(r => r.id === id);
        if (room) this.startRoom(room);
        else this.deps.toast('That room is no longer available.');
      },
      onStartNow: () => this.startNow(),
      onRoomsChanged: () => this.refresh(),
    });
    const live = document.createElement('section');
    live.className = 'home-section live-section';
    live.setAttribute('aria-labelledby', 'live-heading');
    live.hidden = true;
    live.innerHTML = `<h2 class="list-heading" id="live-heading">Happening now</h2><ul class="live-list" role="list"></ul>`;
    this.liveSection = live;
    const recent = document.createElement('section');
    recent.className = 'home-section recent-panel';
    recent.setAttribute('aria-labelledby', 'recent-heading');
    recent.innerHTML = `<h2 class="list-heading" id="recent-heading">Recent meetings</h2><ul class="recent-list" role="list"><li class="room-empty"><span>Loading…</span></li></ul>`;
    this.recentList = recent.querySelector('.recent-list');
    this.upcomingPanel.el.querySelector('.upcoming-later')?.before(live);
    home.querySelector('#home-meetings')!.append(this.upcomingPanel.el, recent);

    // Rooms.
    this.roomsPanel = new RoomsPanel({
      api: this.api,
      toast: this.deps.toast,
      onStart: room => this.startRoom(room),
      onChange: () => void this.refresh(),
    });
    home.querySelector('#home-rooms')!.append(this.roomsPanel.el);

    // Recordings: meetings' recordings, then record links and drafts [SL-P5-3].
    const recHead = document.createElement('div');
    recHead.className = 'view-head';
    recHead.innerHTML = `<div><h1 class="view-title" id="home-recordings-heading">Recordings</h1><p class="home-sub">Everything recorded in your meetings, and what people recorded on their own.</p></div>`;
    const meetingRecs = document.createElement('section');
    meetingRecs.className = 'home-section meeting-recordings';
    meetingRecs.setAttribute('aria-labelledby', 'meeting-recordings-heading');
    meetingRecs.innerHTML = `<h2 class="list-heading" id="meeting-recordings-heading">From your meetings</h2><ul class="recent-list meeting-recordings-list" role="list"><li class="room-empty"><span>Loading…</span></li></ul>`;
    this.meetingRecordings = meetingRecs.querySelector('ul');
    this.recordingsPanel = new RecordLinksPanel({ toast: this.deps.toast, rooms: () => this.rooms.map(r => ({ id: r.id, name: r.name })) });
    home.querySelector('#home-recordings')!.append(recHead, meetingRecs, this.recordingsPanel.el);

    $('#main-container')?.prepend(home);
    this.bindHome(home);
    // This tab's last rail view survives reloads (a per-viewer convenience).
    let saved: string | null = null;
    try { saved = sessionStorage.getItem(VIEW_KEY); } catch { saved = null; }
    if (saved === 'rooms' || saved === 'recordings') this.setView(saved);

    // Ready to join is its own step: hidden until New / Start / Join / ?room=.
    const green = $('#green-room');
    if (green) green.style.display = 'none';
    $('#ready-back-btn')?.addEventListener('click', () => this.showHome(true));
    if (roomId) this.showReady();

    void this.refresh();
    void this.recordingsPanel.refresh();
    void this.upcomingPanel.refresh();
  }

  private bindHome(home: HTMLElement): void {
    const newBtn = home.querySelector<HTMLButtonElement>('#home-new-btn')!;
    const newMenu = home.querySelector<HTMLElement>('#home-new-menu')!;
    const newCtl = bindMenu(newBtn, newMenu);
    newMenu.addEventListener('click', e => {
      const item = (e.target as HTMLElement).closest<HTMLElement>('[data-new]');
      if (!item) return;
      newCtl.close();
      switch (item.dataset.new) {
        case 'now': this.startNow(); break;
        case 'schedule': this.setView('meetings'); void this.upcomingPanel?.schedule(); break;
        case 'room': void this.createRoom(); break;
        case 'record': this.setView('recordings'); void this.recordingsPanel?.create(); break;
      }
    });
    const setBtn = home.querySelector<HTMLButtonElement>('#home-settings-btn')!;
    const setMenu = home.querySelector<HTMLElement>('#home-settings-menu')!;
    const setCtl = bindMenu(setBtn, setMenu);
    setMenu.addEventListener('click', e => {
      const item = (e.target as HTMLElement).closest<HTMLElement>('[data-setting]');
      if (!item) { if ((e.target as HTMLElement).closest('[data-source-link]')) setCtl.close(); return; }
      setCtl.close();
      if (item.dataset.setting === 'devices') this.startNow();
      else if (item.dataset.setting === 'feed') void this.upcomingPanel?.openFeed();
      else $('#wizard-btn')?.click();
    });
    home.querySelector('#home-help-btn')!.addEventListener('click', () => $('#wizard-btn')?.click());

    const rail = home.querySelector<HTMLElement>('.home-rail')!;
    rail.addEventListener('click', e => {
      const item = (e.target as HTMLElement).closest<HTMLElement>('[data-view]');
      if (item) this.setView(item.dataset.view as HomeView);
    });
    rail.addEventListener('keydown', e => {
      const keys: Record<string, number> = { ArrowDown: 1, ArrowRight: 1, ArrowUp: -1, ArrowLeft: -1 };
      if (!(e.key in keys)) return;
      e.preventDefault();
      const items = [...rail.querySelectorAll<HTMLButtonElement>('[data-view]')];
      const i = items.indexOf(document.activeElement as HTMLButtonElement);
      items[(Math.max(0, i) + keys[e.key] + items.length) % items.length].focus();
    });

    const form = home.querySelector<HTMLFormElement>('#home-join-form')!;
    const input = form.querySelector<HTMLInputElement>('#home-join-input')!;
    const join = form.querySelector<HTMLButtonElement>('#home-join-btn')!;
    input.addEventListener('input', () => { join.disabled = !input.value.trim(); });
    form.addEventListener('submit', e => { e.preventDefault(); this.joinFromBox(input.value); });

    this.recentList?.addEventListener('click', e => {
      const btn = (e.target as HTMLElement).closest<HTMLElement>('[data-room-start]');
      const room = this.rooms.find(r => r.id === btn?.dataset.roomStart);
      if (room) this.startRoom(room);
    });
    this.liveSection?.addEventListener('click', e => {
      const btn = (e.target as HTMLElement).closest<HTMLElement>('[data-room-start]');
      const room = this.rooms.find(r => r.id === btn?.dataset.roomStart);
      if (room) this.startRoom(room);
    });
  }

  /** Shows one of the rail's views. */
  setView(view: HomeView): void {
    if (!this.home) return;
    try { sessionStorage.setItem(VIEW_KEY, view); } catch { /* storage off: the view just resets */ }
    for (const el of this.home.querySelectorAll<HTMLElement>('.home-view')) el.hidden = el.dataset.view !== view;
    for (const el of this.home.querySelectorAll<HTMLElement>('.rail-item')) {
      if (el.dataset.view === view) el.setAttribute('aria-current', 'page'); else el.removeAttribute('aria-current');
    }
  }

  /** "Paste a link or room name": a p2p.video link opens; a room name or slug starts that room. */
  private joinFromBox(raw: string): void {
    const value = raw.trim();
    if (!value) return;
    let url: URL | null = null;
    try { url = new URL(value); } catch { if (value.startsWith('/')) { try { url = new URL(value, location.origin); } catch { url = null; } } }
    if (url) {
      const meetingLink = /^https?:$/.test(url.protocol) && (url.pathname === '/join' || url.pathname === '/record' || url.searchParams.has('session') || url.searchParams.has('room'));
      if (!meetingLink) { this.deps.toast('That isn’t a p2p.video meeting link. Check it and paste it again.'); return; }
      location.assign(url.href);
      return;
    }
    const key = value.toLowerCase();
    const slug = key.replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '');
    const room = this.rooms.find(r => !r.archived && (r.slug === slug || r.name.toLowerCase() === key));
    if (room) { this.startRoom(room); return; }
    this.deps.toast(`There’s no room called “${value}”. Check the name, or create it with New.`);
  }

  private async createRoom(): Promise<void> {
    this.setView('rooms');
    const name = await promptDialog('Create a room', 'Room name', '', 'Create room');
    if (!name) { this.roomsPanel?.focusCreate(); return; }
    const input = $<HTMLInputElement>('#room-name-input');
    if (!input) return;
    input.value = name;
    input.form?.requestSubmit();
  }

  /** Ready to join for an instant meeting. */
  private startNow(): void {
    this.target = null;
    this.showReady();
  }

  /** Opens Ready to join (#green-room) for the current target. */
  showReady(): void {
    if (this.mode.kind !== 'host-home') return;
    if (this.home) this.home.hidden = true;
    document.body.classList.add('is-ready');
    const green = $('#green-room');
    if (green) green.style.display = '';
    const back = $('#ready-back-btn');
    if (back) back.hidden = false;
    this.renderTarget();
    this.deps.onReadyShown?.();
    window.scrollTo({ top: 0 });
    const name = $<HTMLInputElement>('#name-input');
    (name && !name.value.trim() ? name : this.deps.joinButton).focus({ preventScroll: true });
  }

  /** Back from Ready to join to the home. */
  showHome(focus = false): void {
    if (this.mode.kind !== 'host-home' || !this.home) return;
    this.target = null;
    this.pendingRoomId = undefined;
    const green = $('#green-room');
    if (green) green.style.display = 'none';
    document.body.classList.remove('is-ready');
    this.home.hidden = false;
    this.deps.onHomeShown?.();
    if (focus) $<HTMLButtonElement>('#home-new-btn')?.focus();
  }

  /** Joining: the home gives way to the call screens. */
  leaveHome(): void {
    if (this.home) this.home.hidden = true;
    document.body.classList.remove('is-home', 'is-ready');
  }

  private async refresh(): Promise<void> {
    if (this.mode.kind !== 'host-home') return;
    let data: HomeData;
    try { data = await this.api.home(); }
    catch (err) {
      const msg = err instanceof ApiError && err.status === 403
        ? 'Rooms and recent meetings are managed on the host Mac’s own dashboard.'
        : 'Rooms are unavailable right now. You can still start a meeting.';
      this.roomsPanel?.el.querySelector('.room-list')?.replaceChildren(Object.assign(document.createElement('li'), { className: 'room-empty', textContent: msg }));
      if (this.recentList) this.recentList.innerHTML = '';
      if (this.meetingRecordings) this.meetingRecordings.innerHTML = '';
      return;
    }
    const roomKey = (list: RoomView[]) => JSON.stringify(list.map(r => [r.id, r.name, r.linkEnabled, r.live, r.archived]));
    const roomsChanged = roomKey(this.rooms) !== roomKey(data.rooms);
    this.rooms = data.rooms;
    this.roomsPanel?.render(data.rooms, data.guestLinksRemote);
    // Room names and links appear in invites: refresh them with the rooms.
    if (roomsChanged) void this.upcomingPanel?.refresh();
    this.renderLive(data.rooms);
    this.renderRecent(data.recent);
    this.renderMeetingRecordings(data.recent);
    if (this.pendingRoomId) {
      this.target = this.rooms.find(r => r.id === this.pendingRoomId) ?? null;
      if (!this.target) this.deps.toast('That room is no longer available.');
      this.pendingRoomId = undefined;
    } else if (this.target) {
      this.target = this.rooms.find(r => r.id === this.target!.id) ?? null;
    }
    this.renderTarget();
  }

  private renderLive(rooms: RoomView[]): void {
    if (!this.liveSection) return;
    const live = rooms.filter(r => r.live && !r.archived);
    this.liveSection.hidden = !live.length;
    this.liveSection.querySelector('.live-list')!.innerHTML = live.map(r => `<li class="live-row">
      <span class="live-mark" aria-hidden="true"></span>
      <div class="recent-main"><strong>${escapeHtml(r.name)}</strong><span class="recent-meta"><span>${r.hostInCall ? 'You’re in this meeting' : 'Meeting open'}</span></span></div>
      <button type="button" class="btn btn-primary" data-room-start="${escapeHtml(r.id)}" aria-label="Rejoin ${escapeHtml(r.name)}">Rejoin</button></li>`).join('');
  }

  private renderRecent(list: HomeSession[]): void {
    if (!this.recentList) return;
    if (!list.length) {
      this.recentList.innerHTML = `<li class="room-empty home-empty-small"><strong>No meetings yet</strong><span>Meetings you hold show up here, so you can rejoin or find their recordings.</span></li>`;
      return;
    }
    this.recentList.innerHTML = list.map(s => {
      const token = encodeURIComponent(s.hostToken);
      const dur = formatDuration(s.createdAt, s.endedAt);
      const room = s.roomId ? this.rooms.find(r => r.id === s.roomId && !r.archived) : undefined;
      const rejoin = !s.endedAt ? `<a class="btn btn-secondary" href="/?session=${encodeURIComponent(s.id)}&amp;token=${token}">${s.live ? 'Rejoin' : 'Reopen'}</a>`
        : room ? `<button type="button" class="btn btn-quiet" data-room-start="${escapeHtml(room.id)}" aria-label="Start ${escapeHtml(room.name)} again">Start again</button>` : '';
      return `<li class="recent-row">
        <span class="recent-icon ui-icon" aria-hidden="true">${s.live ? HOME_ICONS.live : HOME_ICONS.history}</span>
        <div class="recent-main"><strong>${escapeHtml(s.title)}</strong>
          <span class="recent-meta"><span>${escapeHtml(whenLabel(s.createdAt))}</span>${dur ? `<span>${dur}</span>` : ''}${s.roomName ? `<span class="recent-room">${escapeHtml(s.roomName)}</span>` : ''}${s.live ? '<span class="recent-live">Live</span>' : ''}<span class="recent-takes">${escapeHtml(recordingsSummary(s.takes))}</span></span></div>
        ${rejoin}</li>`;
    }).join('');
  }

  /** Recordings view: meetings that have recordings, with their downloads. */
  private renderMeetingRecordings(list: HomeSession[]): void {
    if (!this.meetingRecordings) return;
    const withTakes = list.filter(s => s.takes.length);
    if (!withTakes.length) {
      this.meetingRecordings.innerHTML = `<li class="room-empty home-empty-small"><strong>Nothing recorded yet</strong><span>In a meeting, press Record. Each person is recorded on their own device at full quality, and the files arrive here.</span></li>`;
      return;
    }
    this.meetingRecordings.innerHTML = withTakes.map(s => {
      const token = encodeURIComponent(s.hostToken);
      const files = s.takes.filter(t => t.status === 'complete').map(t =>
        `<a class="take-link" href="/api/v1/takes/${encodeURIComponent(t.id)}/download?token=${token}" download><span class="ui-icon" aria-hidden="true">${HOME_ICONS.download}</span>Recording ${t.number}</a>`).join('');
      const pending = s.takes.filter(t => t.status !== 'complete').map(t => `<span class="take-chip">Recording ${t.number}: ${escapeHtml(takeStatusLabel(t.status).toLowerCase())}</span>`).join('');
      const open = !s.endedAt ? `<a class="btn btn-secondary" href="/?session=${encodeURIComponent(s.id)}&amp;token=${token}">${s.live ? 'Rejoin' : 'Open'}</a>` : '';
      return `<li class="recent-row">
        <span class="recent-icon ui-icon" aria-hidden="true">${HOME_ICONS.recordings}</span>
        <div class="recent-main"><strong>${escapeHtml(s.title)}</strong>
          <span class="recent-meta"><span>${escapeHtml(whenLabel(s.createdAt))}</span>${s.roomName ? `<span class="recent-room">${escapeHtml(s.roomName)}</span>` : ''}<span class="recent-takes">${escapeHtml(recordingsSummary(s.takes))}</span></span>
          <div class="recent-files">${files}${pending}</div></div>
        ${open}</li>`;
    }).join('');
  }

  /** Ready to join's heading and button for the chosen room (or an instant meeting). */
  private renderTarget(): void {
    if (this.mode.kind !== 'host-home') return;
    const btn = this.deps.joinButton;
    const green = $('#green-room');
    if (this.pendingRoomId && !this.target) {
      setHeading(green, 'Room', 'Opening your room…', 'Check your camera and microphone while it opens.');
      btn.textContent = 'Start meeting';
      return;
    }
    if (!this.target) {
      setHeading(green, '', 'Ready to join?', 'Check your camera and microphone, then start. You can add others once you’re in.');
      btn.textContent = 'Start meeting';
      document.title = 'p2p.video';
      return;
    }
    const r = this.target;
    setHeading(green, r.name, r.live ? `Rejoin ${r.name}` : `Start ${r.name}`,
      r.live ? 'The meeting is still open. Check your camera and microphone, then go back in.' : 'Guests with the room link can join once you start. Check your camera and microphone first.');
    btn.textContent = `${r.live ? 'Rejoin' : 'Start'} ${r.name}`;
    document.title = `${r.name} — p2p.video`;
  }

  private startRoom(room: RoomView): void {
    this.target = room;
    this.showReady();
  }

  // --------------------------------------------------------- session links

  /** /?session=&token= (host rejoin) and /join?session=&token= (guest invite). */
  private mountSession(): void {
    const guest = location.pathname === '/join';
    setHeading($('#green-room'), '', 'Ready to join?', guest ? 'Check your camera and microphone, then ask to join. The host lets you in.' : 'Check your camera and microphone, then go back in.');
    this.deps.joinButton.textContent = guest ? 'Ask to join' : 'Join now';
  }

  // --------------------------------------------------------------- room guest

  private mountGuestRoom(): void {
    document.body.classList.add('is-room-guest');
    setHeading($('#green-room'), 'Room', 'Join the room', 'Check your camera and microphone before you join.');
    void this.pollLobby();
    this.lobbyTimer = window.setInterval(() => { if (!this.joining) void this.pollLobby(); }, Math.max(this.pollMs, 3000));
  }

  private async fetchLobby(): Promise<RoomLobby | null> {
    if (this.mode.kind !== 'guest-room') return null;
    try {
      this.lobbyState = await this.api.lobby(this.mode.slug, this.mode.token);
      this.linkError = '';
    } catch (err) {
      this.lobbyState = null;
      this.linkError = err instanceof ApiError && err.status === 401
        ? 'This room link doesn’t work any more. Ask the host for the current link.'
        : 'Can’t reach the host right now. Retrying…';
    }
    return this.lobbyState;
  }

  private async pollLobby(): Promise<void> {
    const lobby = await this.fetchLobby();
    const btn = this.deps.joinButton;
    if (!lobby) {
      if (this.linkError.startsWith('This room link')) this.showInvalidLink();
      else { const status = $('#device-status'); if (status) status.textContent = this.linkError; }
      return;
    }
    const host = hostLabel(lobby.hostName);
    document.title = `${lobby.name} — p2p.video`;
    setHeading($('#green-room'), lobby.name, `Join ${lobby.name}`,
      lobby.live ? `${host[0].toUpperCase()}${host.slice(1)} is here. Check your camera and microphone, then ask to join.`
        : `Get ready now. ${host[0].toUpperCase()}${host.slice(1)} lets you in once the meeting starts.`);
    btn.textContent = lobby.live ? 'Ask to join' : `Join when ${host} starts`;
    if (lobby.role === 'producer') this.deps.joinRole?.('producer');
  }

  /** The link was replaced, turned off or never existed: say so and lock join. */
  private showInvalidLink(): void {
    if (this.lobbyTimer !== null) { clearInterval(this.lobbyTimer); this.lobbyTimer = null; }
    setHeading($('#green-room'), 'Room link', 'This link doesn’t work any more',
      'The host replaced or turned off this room’s link. Ask them for the current one.');
    const status = $('#device-status');
    if (status) status.textContent = this.linkError;
    const btn = this.deps.joinButton;
    btn.textContent = 'Link no longer works';
    btn.disabled = true;
    // Device setup re-enables join when the camera is ready; keep it locked.
    new MutationObserver(() => { if (!btn.disabled) btn.disabled = true; }).observe(btn, { attributes: true, attributeFilter: ['disabled'] });
  }

  /** Waits (on the waiting-room screen) until the host is in the call. */
  private waitForHost(): Promise<OpenedSession> {
    const mode = this.mode as Extract<EntryMode, { kind: 'guest-room' }>;
    const green = $('#green-room'), waiting = $('#waiting-room');
    const heading = waiting?.querySelector<HTMLElement>('.workspace-heading');
    const original = heading?.innerHTML ?? '';
    const lobby = this.lobbyState!;
    setHeading(waiting, lobby.name, `Waiting for ${hostLabel(lobby.hostName)} to start ${lobby.name}`,
      'Keep this tab open. You’ll ask to join by yourself the moment the meeting starts.', 'Host not here yet');
    waiting?.classList.add('is-waiting-host');
    const back = document.createElement('button');
    back.type = 'button';
    back.className = 'btn btn-quiet waiting-host-back';
    back.innerHTML = `<span class="ui-icon" aria-hidden="true">${HOME_ICONS.back}</span><span>Back to camera and mic</span>`;
    (heading?.firstElementChild ?? heading)?.appendChild(back);
    if (green) green.style.display = 'none';
    if (waiting) waiting.style.display = 'block';
    const preview = $('#waiting-preview-video') as HTMLVideoElement | null;
    if (preview) preview.srcObject = this.deps.stream();

    return new Promise<OpenedSession>((resolve, reject) => {
      let timer: number | null = null;
      const restore = () => {
        if (timer !== null) clearTimeout(timer);
        if (heading) heading.innerHTML = original;
        waiting?.classList.remove('is-waiting-host');
      };
      back.addEventListener('click', () => {
        restore();
        if (waiting) waiting.style.display = 'none';
        if (green) green.style.display = '';
        reject(new EntryCancelled());
      }, { once: true });
      const tick = async () => {
        const l = await this.fetchLobby();
        if (!waiting?.classList.contains('is-waiting-host')) return; // cancelled
        if (l?.live && l.sessionId) { restore(); resolve({ sessionId: l.sessionId, token: mode.token, host: false, title: l.name }); return; }
        if (!l && this.linkError.startsWith('This room link')) {
          restore();
          if (waiting) waiting.style.display = 'none';
          if (green) green.style.display = '';
          reject(new Error(this.linkError));
          return;
        }
        timer = window.setTimeout(tick, this.pollMs);
      };
      timer = window.setTimeout(tick, this.pollMs);
    });
  }

  // ------------------------------------------------------------ open session

  /**
   * Resolves the session this page should join when the user presses join
   * without a session in the URL: an instant meeting, a room's meeting
   * (host), or the room's live meeting once the host is in (guest).
   */
  async openSession(name: string): Promise<OpenedSession> {
    if (this.mode.kind === 'guest-room') {
      this.joining = true;
      try {
        const lobby = await this.fetchLobby();
        if (!lobby) throw new Error(this.linkError || 'This room is unavailable.');
        if (lobby.live && lobby.sessionId) return { sessionId: lobby.sessionId, token: this.mode.token, host: false, title: lobby.name };
        return await this.waitForHost();
      } catch (err) { this.joining = false; throw err; }
    }
    if (this.target) {
      const room = this.target;
      try {
        const started = await this.api.start(room.id, name);
        return { sessionId: started.sessionId, token: started.hostToken, host: true, title: started.title };
      } catch (err) {
        this.target = null;
        this.renderTarget();
        void this.refresh();
        throw new Error(err instanceof ApiError ? err.message : `Couldn’t open ${room.name}.`);
      }
    }
    const created = await this.api.instant(instantTitle(name));
    return { sessionId: created.id, token: created.hostToken, host: true, title: created.title };
  }

  // ------------------------------------------------------- end of a meeting

  private mountEndMeeting(): void {
    const dock = $('.transport-actions');
    if (!dock) return;
    const btn = document.createElement('button');
    btn.id = 'end-meeting-btn';
    btn.className = 'btn btn-end';
    btn.textContent = 'End meeting';
    btn.hidden = true;
    btn.addEventListener('click', () => void this.endMeeting());
    dock.appendChild(btn);
    this.endBtn = btn;
  }

  /** Shows the host's End meeting control once admitted as host. */
  setInCall(host: boolean, sessionId: () => string, token: () => string): void {
    this.call = { sessionId, token };
    if (this.endBtn) this.endBtn.hidden = !host;
    if (this.lobbyTimer !== null) { clearInterval(this.lobbyTimer); this.lobbyTimer = null; }
  }

  private async endMeeting(): Promise<void> {
    if (!this.call) return;
    if (!await confirmDialog('End the meeting for everyone?', 'Everyone leaves the call. Recordings that are still uploading keep going while their tabs stay open.', 'End meeting', true)) return;
    try { await this.api.end(this.call.sessionId(), this.call.token()); }
    catch (err) { this.deps.toast(err instanceof ApiError ? err.message.replace(/^./, c => c.toUpperCase()) : 'Couldn’t end the meeting.'); }
  }

  /** The meeting is over (session_end): replace the call with a summary. */
  sessionEnded(host: boolean): void {
    if (this.endBtn) this.endBtn.hidden = true;
    for (const id of ['home-view', 'green-room', 'waiting-room', 'studio-view']) { const el = document.getElementById(id); if (el) el.style.display = 'none'; }
    document.getElementById('meeting-ended')?.remove();
    const roomGuest = this.mode.kind === 'guest-room';
    const roomName = this.lobbyState?.name;
    const sec = document.createElement('section');
    sec.id = 'meeting-ended';
    sec.className = 'meeting-ended';
    sec.setAttribute('role', 'status');
    sec.innerHTML = `<div class="workspace-heading"><div><span class="section-kicker">${escapeHtml(roomName || 'Meeting')}</span>
      <h1 tabindex="-1">${host ? 'Meeting ended' : 'The host ended the meeting'}</h1>
      <p>${host ? 'Everyone has left the call.' : roomGuest ? 'You can rejoin with the same link next time this room opens.' : 'Thanks for joining.'} Keep this tab open until any recording finishes uploading.</p></div></div>
      <div class="ended-card glass-panel"><span class="field-label">Uploads</span><p data-ended-uploads>Nothing was recorded on this device.</p>
      <div class="ended-actions">${host ? '<a class="btn btn-primary" href="/">Back to home</a>' : roomGuest ? `<button class="btn btn-primary" data-rejoin>Wait for the next meeting in ${escapeHtml(roomName || 'this room')}</button>` : ''}</div></div>`;
    sec.querySelector('[data-rejoin]')?.addEventListener('click', () => location.reload());
    document.getElementById('main-container')?.appendChild(sec);
    const chunks = document.getElementById('telemetry-chunks');
    const out = sec.querySelector<HTMLElement>('[data-ended-uploads]')!;
    const mirror = () => { const t = chunks?.textContent?.trim(); if (t && !/^No track/.test(t)) out.textContent = t; };
    mirror();
    if (chunks) new MutationObserver(mirror).observe(chunks, { childList: true, characterData: true, subtree: true });
    (sec.querySelector('h1') as HTMLElement | null)?.focus?.();
  }
}
