// Upcoming panel on the host home [SL-P6-1]: schedule a meeting in a room,
// invite guests (copy text, .ics download, Google/Outlook template links,
// email from the host's own mail app), edit, cancel, start; and the host's
// calendar feed. No calendar service is involved: invites are files and
// plain URLs. Invite texts and links carry the room's guest link: clipboard,
// downloads and the host's own calendar only; never rendered as text.
import '../upcoming.css';
import { escapeHtml } from '../lib/html';
import { ApiError, RoomsApi, type RoomView } from '../lib/rooms';
import {
  DURATIONS, ScheduleApi, defaultStart, durationLabel, localParts, localTimeZone, meetingPhase, parseEmails,
  relativeStart, toStartsAt, type CalendarFeed, type MeetingDraft, type ScheduledMeeting,
} from '../lib/schedule';
import { EMPTY_DAY_ART, HOME_ICONS } from './homeIcons';
import { confirmDialog, copyText, promptDialog } from './Rooms';

export interface UpcomingPanelOptions {
  api?: ScheduleApi;
  roomsApi?: RoomsApi;
  toast: (message: string) => void;
  /** Rooms to schedule in (the home's current list). */
  rooms: () => RoomView[];
  /** Start (or rejoin) the meeting in this room. */
  onStart: (roomId: string) => void;
  /** The empty day's "Start a meeting" (an instant meeting). */
  onStartNow?: () => void;
  /** A room was created from the schedule form; the home refreshes. */
  onRoomsChanged?: () => Promise<void> | void;
  /** Re-render interval for relative times. */
  tickMs?: number;
}

const ORGANIZER_KEY = 'studiolink.calendar.organizerEmail';
const errorText = (err: unknown) => (err instanceof ApiError ? err.message.replace(/^./, c => c.toUpperCase()) : 'Something went wrong. Try again.');

function remembered(): string {
  try { return localStorage.getItem(ORGANIZER_KEY) || ''; } catch { return ''; }
}
function remember(email: string): void {
  try { if (email) localStorage.setItem(ORGANIZER_KEY, email); else localStorage.removeItem(ORGANIZER_KEY); } catch { /* private mode */ }
}

const startOfDay = (d: Date) => new Date(d.getFullYear(), d.getMonth(), d.getDate());
const addDays = (d: Date, n: number) => new Date(d.getFullYear(), d.getMonth(), d.getDate() + n);
const sameDay = (a: Date, b: Date) => a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
const dayKey = (d: Date) => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
const timeOf = (iso: string) => new Date(iso).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });

/** "today", "tomorrow", "Tuesday" (this week) or "Oct 30" for the empty-day copy. */
function dayWord(day: Date, now: Date): string {
  if (sameDay(day, now)) return 'today';
  if (sameDay(day, addDays(now, 1))) return 'tomorrow';
  const diff = (startOfDay(day).getTime() - startOfDay(now).getTime()) / 86_400_000;
  if (diff > 0 && diff < 7) return day.toLocaleDateString([], { weekday: 'long' });
  return day.toLocaleDateString([], { month: 'short', day: 'numeric' });
}

/**
 * The Meetings view of the home [SL-UX-2a]: a day heading and date strip, that
 * day's scheduled meetings (Meet-style), and on today a short "Coming up" list.
 */
export class UpcomingPanel {
  readonly el: HTMLElement;
  private readonly api: ScheduleApi;
  private readonly roomsApi: RoomsApi;
  private meetings: ScheduledMeeting[] = [];
  private readonly list: HTMLElement;
  private readonly later: HTMLElement;
  private readonly laterList: HTMLElement;
  private readonly strip: HTMLElement;
  private day = startOfDay(new Date());
  private loaded = false;
  private timer: number | null = null;

  constructor(private readonly opts: UpcomingPanelOptions) {
    this.api = opts.api ?? new ScheduleApi();
    this.roomsApi = opts.roomsApi ?? new RoomsApi();
    this.el = document.createElement('section');
    this.el.className = 'home-panel upcoming-panel';
    this.el.id = 'upcoming-panel';
    this.el.setAttribute('aria-labelledby', 'upcoming-heading');
    this.el.innerHTML = `<div class="day-head">
        <div class="day-title-wrap"><h1 class="day-title" id="upcoming-heading" aria-live="polite"></h1><button type="button" class="btn btn-quiet btn-compact day-today" data-day-today hidden>Back to today</button></div>
        <div class="upcoming-head-actions"><button type="button" class="btn btn-quiet btn-compact" id="calendar-feed-btn" aria-label="Calendar feed settings"><span class="ui-icon" aria-hidden="true">${HOME_ICONS.calendar}</span><span>Calendar feed</span></button><button type="button" class="btn btn-secondary btn-compact" id="schedule-btn">Schedule</button></div>
      </div>
      <div class="date-strip" role="group" aria-label="Choose a day">
        <button type="button" class="icon-button date-step" data-week="-1" aria-label="Previous week">${HOME_ICONS.chevronLeft}</button>
        <ol class="date-days" role="list"></ol>
        <button type="button" class="icon-button date-step" data-week="1" aria-label="Next week">${HOME_ICONS.chevronRight}</button>
      </div>
      <ul class="upcoming-list upcoming-day-list" role="list" aria-labelledby="upcoming-heading"><li class="room-empty"><span>Loading…</span></li></ul>
      <section class="upcoming-later" aria-labelledby="upcoming-later-heading" hidden><h2 class="list-heading" id="upcoming-later-heading">Coming up</h2><ul class="upcoming-list upcoming-later-list" role="list"></ul></section>`;
    this.list = this.el.querySelector('.upcoming-day-list')!;
    this.later = this.el.querySelector('.upcoming-later')!;
    this.laterList = this.el.querySelector('.upcoming-later-list')!;
    this.strip = this.el.querySelector('.date-days')!;
    this.el.querySelector('#schedule-btn')!.addEventListener('click', () => void this.form());
    this.el.querySelector('#calendar-feed-btn')!.addEventListener('click', () => void this.feedDialog());
    this.el.querySelector('[data-day-today]')!.addEventListener('click', () => this.setDay(new Date(), true));
    for (const b of this.el.querySelectorAll<HTMLButtonElement>('[data-week]')) b.addEventListener('click', () => this.setDay(addDays(this.day, 7 * Number(b.dataset.week))));
    this.strip.addEventListener('click', e => {
      const b = (e.target as HTMLElement).closest<HTMLButtonElement>('[data-date]');
      if (b) this.setDay(new Date(`${b.dataset.date}T00:00:00`), true);
    });
    this.strip.addEventListener('keydown', e => {
      const step = ({ ArrowLeft: -1, ArrowRight: 1 } as Record<string, number>)[e.key];
      if (!step) return;
      e.preventDefault();
      this.setDay(addDays(this.day, step), true);
    });
    this.el.addEventListener('click', e => {
      const target = e.target as HTMLElement;
      const empty = target.closest<HTMLElement>('[data-empty]');
      if (empty) { if (empty.dataset.empty === 'schedule') void this.form(); else this.opts.onStartNow?.(); return; }
      const btn = target.closest<HTMLElement>('[data-act]');
      const row = btn?.closest<HTMLElement>('[data-meeting-id]');
      const m = this.meetings.find(x => x.id === row?.dataset.meetingId);
      if (btn && m) void this.act(btn.dataset.act!, m, btn);
    });
    document.addEventListener('click', e => {
      for (const d of this.el.querySelectorAll<HTMLDetailsElement>('.upcoming-list details[open]')) if (!d.contains(e.target as Node)) d.open = false;
    });
    this.el.addEventListener('keydown', e => {
      if (e.key !== 'Escape') return;
      const open = this.el.querySelector<HTMLDetailsElement>('.upcoming-list details[open]');
      if (open) { open.open = false; open.querySelector('summary')?.focus(); }
    });
    this.renderStrip();
    this.timer = window.setInterval(() => this.render(), opts.tickMs ?? 30_000);
  }

  destroy(): void { if (this.timer !== null) clearInterval(this.timer); }

  /** New, then Schedule for later. */
  schedule(): Promise<void> { return this.form(); }
  /** Settings, then Calendar feed. */
  openFeed(): Promise<void> { return this.feedDialog(); }

  /** Shows `day` (local) in the list and the date strip. */
  setDay(day: Date, focus = false): void {
    this.day = startOfDay(day);
    this.renderStrip();
    if (this.loaded) this.render();
    if (focus) this.strip.querySelector<HTMLButtonElement>('[aria-pressed="true"]')?.focus();
  }

  async refresh(): Promise<void> {
    try {
      this.meetings = (await this.api.list()).meetings;
    } catch (err) {
      this.list.innerHTML = `<li class="room-empty"><span>${escapeHtml(err instanceof ApiError && err.status === 403 ? 'Scheduling is managed on the host Mac’s own dashboard.' : 'Scheduled meetings are unavailable right now.')}</span></li>`;
      return;
    }
    this.loaded = true;
    this.renderStrip();
    this.render();
  }

  private renderStrip(): void {
    const now = new Date();
    const first = addDays(this.day, -this.day.getDay());
    const busy = new Set(this.meetings.filter(m => m.status !== 'cancelled').map(m => dayKey(new Date(m.startsAt))));
    const title = this.el.querySelector('.day-title')!;
    title.textContent = this.day.toLocaleDateString([], { weekday: 'long', month: 'long', day: 'numeric', ...(this.day.getFullYear() !== now.getFullYear() ? { year: 'numeric' } : {}) });
    this.el.querySelector<HTMLElement>('[data-day-today]')!.hidden = sameDay(this.day, now);
    this.strip.innerHTML = Array.from({ length: 7 }, (_, i) => {
      const d = addDays(first, i);
      const sel = sameDay(d, this.day), today = sameDay(d, now);
      const label = d.toLocaleDateString([], { weekday: 'long', month: 'long', day: 'numeric' });
      return `<li><button type="button" class="date-day${today ? ' is-today' : ''}" data-date="${dayKey(d)}" aria-pressed="${sel}" tabindex="${sel ? 0 : -1}" aria-label="${escapeHtml(`${label}${today ? ', today' : ''}${busy.has(dayKey(d)) ? ', has meetings' : ''}`)}">
        <span class="date-dow" aria-hidden="true">${escapeHtml(d.toLocaleDateString([], { weekday: 'narrow' }))}</span><span class="date-num" aria-hidden="true">${d.getDate()}</span><span class="date-dot${busy.has(dayKey(d)) ? ' is-busy' : ''}" aria-hidden="true"></span></button></li>`;
    }).join('');
  }

  private render(): void {
    if (!this.loaded) return;
    const now = new Date();
    const isToday = sameDay(this.day, now);
    const byStart = (a: ScheduledMeeting, b: ScheduledMeeting) => a.startsAt.localeCompare(b.startsAt);
    const shown = this.meetings.filter(m => sameDay(new Date(m.startsAt), this.day) || (isToday && meetingPhase(m, now) === 'now')).sort(byStart);
    const tomorrow = addDays(startOfDay(now), 1);
    const later = isToday ? this.meetings.filter(m => new Date(m.startsAt) >= tomorrow).sort(byStart).slice(0, 5) : [];
    // Keep an open menu (and focus inside it) open across refreshes and the clock tick.
    const open = this.el.querySelector<HTMLDetailsElement>('.upcoming-list details[open]');
    const openId = open?.closest<HTMLElement>('[data-meeting-id]')?.dataset.meetingId;
    const openInvite = open?.classList.contains('upcoming-invite');
    const hadFocus = !!open && open.contains(document.activeElement);
    const word = dayWord(this.day, now);
    this.list.innerHTML = shown.length ? shown.map(m => this.rowHtml(m, now)).join('')
      : `<li class="room-empty home-empty">${EMPTY_DAY_ART}<strong>Nothing scheduled for ${escapeHtml(word)}</strong><span>${this.day < startOfDay(now) ? 'No meetings were scheduled on this day.' : 'Schedule a meeting or enjoy the free time.'}</span>
        <span class="home-empty-actions"><button type="button" class="btn btn-primary" data-empty="now"><span class="ui-icon" aria-hidden="true">${HOME_ICONS.video}</span><span>Start a meeting</span></button><button type="button" class="btn btn-secondary" data-empty="schedule">Schedule</button></span></li>`;
    this.later.hidden = !later.length;
    this.laterList.innerHTML = later.map(m => this.rowHtml(m, now, true)).join('');
    if (openId) {
      const row = this.el.querySelector<HTMLElement>(`[data-meeting-id="${CSS.escape(openId)}"]`);
      const again = row?.querySelector<HTMLDetailsElement>(openInvite ? 'details.upcoming-invite' : 'details:not(.upcoming-invite)');
      if (again) { again.open = true; if (hadFocus) again.querySelector('summary')?.focus(); }
    }
  }

  private rowHtml(m: ScheduledMeeting, now: Date, withDay = false): string {
    const phase = meetingPhase(m, now);
    const rel = phase === 'cancelled' ? '' : relativeStart(m, now);
    const badge = phase === 'cancelled' ? '<span class="badge">Cancelled</span>'
      : phase === 'now' ? '<span class="badge badge-success">Now</span>'
        : phase === 'soon' ? '<span class="badge badge-waiting">Soon</span>' : '';
    const linkNote = phase !== 'cancelled' && !m.linkReady ? `<span class="room-link-off">${m.roomArchived ? 'Room archived' : 'Room link off'}</span>` : '';
    const guests = m.guestEmails.length ? `<span>${m.guestEmails.length} guest${m.guestEmails.length === 1 ? '' : 's'}</span>` : '';
    const name = escapeHtml(m.title);
    const cancelled = phase === 'cancelled';
    const startable = !cancelled && !m.roomArchived;
    const prominent = phase === 'now' || phase === 'soon' || m.roomLive;
    const inviteItems = cancelled
      ? `<a role="menuitem" href="${escapeHtml(m.invite.icsPath)}" download data-act="ics">Download cancellation (.ics)</a>
         ${m.invite.mailtoUrl ? `<a role="menuitem" href="${escapeHtml(m.invite.mailtoUrl)}" data-act="mail">Email guests</a>` : ''}
         <button role="menuitem" data-act="copy">Copy cancellation text</button>`
      : m.linkReady
        ? `<button role="menuitem" data-act="copy">Copy invite text</button>
           <a role="menuitem" href="${escapeHtml(m.invite.icsPath)}" download data-act="ics">Download invite (.ics)</a>
           <a role="menuitem" href="${escapeHtml(m.invite.googleUrl)}" target="_blank" rel="noopener noreferrer" data-act="google">Add to Google Calendar</a>
           <a role="menuitem" href="${escapeHtml(m.invite.outlookUrl)}" target="_blank" rel="noopener noreferrer" data-act="outlook">Add to Outlook</a>
           ${m.invite.mailtoUrl ? `<a role="menuitem" href="${escapeHtml(m.invite.mailtoUrl)}" data-act="mail">Email guests</a>` : ''}`
        : `<p class="upcoming-menu-note">Turn the room’s link on (in Rooms, choose New link) to send invites.</p>`;
    const start = new Date(m.startsAt);
    const dayLabel = withDay ? `<span class="upcoming-day">${escapeHtml(sameDay(start, addDays(now, 1)) ? 'Tomorrow' : start.toLocaleDateString([], { weekday: 'short', month: 'short', day: 'numeric' }))}</span>` : '';
    return `<li class="upcoming-row${cancelled ? ' is-cancelled' : ''}${phase === 'over' ? ' is-over' : ''}${phase === 'now' || phase === 'soon' ? ' is-soon' : ''}" data-meeting-id="${escapeHtml(m.id)}">
      <div class="upcoming-time">${dayLabel}<span class="upcoming-start">${escapeHtml(timeOf(m.startsAt))}</span><span class="upcoming-end">${escapeHtml(timeOf(m.endsAt))}</span></div>
      <div class="room-main">
        <div class="room-title"><strong>${name}</strong>${badge}</div>
        <div class="room-meta upcoming-meta"><span class="recent-room">${escapeHtml(m.roomName)}</span>${guests}${rel ? `<span class="upcoming-rel">${escapeHtml(rel)}</span>` : ''}${linkNote}</div>
      </div>
      <div class="room-actions">
        ${startable ? `<button class="btn ${prominent ? 'btn-primary' : 'btn-secondary'}" data-act="start" aria-label="${m.roomLive ? 'Rejoin' : 'Start'} ${name}">${m.roomLive ? 'Rejoin' : 'Start'}</button>` : ''}
        <details class="room-menu upcoming-invite"><summary class="btn btn-secondary" aria-label="Invite options for ${name}">${cancelled ? 'Notify' : 'Invite'}</summary>
          <div class="room-menu-list" role="menu" aria-label="Invite options for ${name}">${inviteItems}</div></details>
        <details class="room-menu"><summary class="icon-button" aria-label="More actions for ${name}">⋯</summary>
          <div class="room-menu-list" role="group" aria-label="${name} actions">
            ${cancelled ? '' : '<button data-act="edit">Edit…</button><button data-act="cancel" class="danger">Cancel meeting…</button>'}
            <button data-act="delete"${cancelled ? '' : ' class="danger"'}>${cancelled ? 'Remove from list' : 'Delete without notice…'}</button>
          </div></details>
      </div></li>`;
  }

  private replace(m: ScheduledMeeting): void {
    this.meetings = this.meetings.some(x => x.id === m.id) ? this.meetings.map(x => (x.id === m.id ? m : x)) : [...this.meetings, m];
    this.meetings.sort((a, b) => a.startsAt.localeCompare(b.startsAt));
    this.renderStrip();
    this.render();
  }

  private async act(action: string, m: ScheduledMeeting, el: HTMLElement): Promise<void> {
    if (el.tagName === 'A') {
      // Links do their own thing (download / new tab / mail app); just close the menu.
      setTimeout(() => el.closest('details')?.removeAttribute('open'), 0);
      if (action === 'ics') this.opts.toast(m.status === 'cancelled' ? 'Cancellation downloaded. Send it to your guests so their calendars drop the meeting.' : 'Invite downloaded. Attach it to an email or open it to add the meeting to your calendar.');
      return;
    }
    el.closest('details')?.removeAttribute('open');
    const { toast } = this.opts;
    try {
      switch (action) {
        case 'start':
          this.opts.onStart(m.roomId);
          return;
        case 'copy':
          if (await copyText(m.invite.text)) toast(m.status === 'cancelled' ? 'Cancellation text copied.' : 'Invite copied. Paste it into an email or message to your guests.');
          else void promptDialog('Copy the invite', 'Invite', m.invite.text, 'Done');
          return;
        case 'edit':
          await this.form(m);
          return;
        case 'cancel': {
          if (!await confirmDialog('Cancel this meeting?', `“${m.title}” is cancelled. Then send your guests the cancellation so it leaves their calendars.`, 'Cancel meeting', true)) return;
          const next = await this.api.cancel(m.id);
          this.replace(next);
          this.shareDialog(next, 'Meeting cancelled', 'Let your guests know: send the cancellation file or email them.');
          return;
        }
        case 'delete':
          if (m.status !== 'cancelled' && !await confirmDialog('Delete without telling guests?', `“${m.title}” disappears from p2p.video, but stays in your guests’ calendars. To remove it there too, use Cancel meeting instead.`, 'Delete', true)) return;
          await this.api.remove(m.id);
          this.meetings = this.meetings.filter(x => x.id !== m.id);
          this.renderStrip();
          this.render();
          toast('Removed.');
          return;
      }
    } catch (err) { toast(errorText(err)); }
  }

  /** Schedule (no meeting) or edit (meeting) dialog. */
  private form(edit?: ScheduledMeeting): Promise<void> {
    return new Promise(resolve => {
      const rooms = this.opts.rooms().filter(r => !r.archived);
      const tz = edit?.timeZone && edit.timeZone !== 'UTC' ? edit.timeZone : localTimeZone();
      const start = edit ? localParts(edit.startsAt) : localParts(defaultStart().toISOString());
      const duration = edit?.durationMin ?? 60;
      const durations = DURATIONS.includes(duration) ? DURATIONS : [...DURATIONS, duration].sort((a, b) => a - b);
      const roomOptions = rooms.map(r => `<option value="${escapeHtml(r.id)}"${r.id === edit?.roomId ? ' selected' : ''}>${escapeHtml(r.name)}</option>`).join('');
      const dialog = document.createElement('dialog');
      dialog.className = 'room-dialog schedule-dialog';
      dialog.setAttribute('aria-labelledby', 'schedule-dialog-title');
      dialog.innerHTML = `<form method="dialog" novalidate><h2 id="schedule-dialog-title">${edit ? 'Edit meeting' : 'Schedule a meeting'}</h2>
        <p>${edit ? 'Guests’ calendars update when you send them the new invite.' : 'Guests join with the room’s link. You get an invite to send; nothing is emailed for you.'}</p>
        <div class="record-form">
          <label class="field-label" for="sch-room">Room</label>
          <select id="sch-room">${roomOptions}<option value="__new"${rooms.length ? '' : ' selected'}>New room…</option></select>
          <input id="sch-room-name" maxlength="80" placeholder="Name the new room, e.g. The Podcast" ${rooms.length ? 'hidden' : ''} aria-label="New room name">
          <label class="field-label" for="sch-title">Title <span class="field-hint">(optional; the room name otherwise)</span></label>
          <input id="sch-title" maxlength="120" placeholder="e.g. Episode 12 with Ada" value="${escapeHtml(edit?.title ?? '')}">
          <div class="record-form-row schedule-when">
            <div><label class="field-label" for="sch-date">Date</label><input id="sch-date" type="date" required value="${start.date}"></div>
            <div><label class="field-label" for="sch-time">Start</label><input id="sch-time" type="time" required step="300" value="${start.time}"></div>
          </div>
          <label class="field-label" for="sch-duration">Length</label>
          <select id="sch-duration">${durations.map(d => `<option value="${d}"${d === duration ? ' selected' : ''}>${durationLabel(d)}</option>`).join('')}</select>
          <p class="schedule-tz">Times are ${escapeHtml(tz)}.</p>
          <label class="field-label" for="sch-desc">Agenda or notes <span class="field-hint">(optional; goes in the invite)</span></label>
          <textarea id="sch-desc" rows="4" maxlength="4000" placeholder="Topics, prep, anything guests should know">${escapeHtml(edit?.description ?? '')}</textarea>
          <label class="field-label" for="sch-guests">Guest emails <span class="field-hint">(optional; for the invite and your mail app)</span></label>
          <textarea id="sch-guests" rows="2" placeholder="ada@example.com, grace@example.org">${escapeHtml(edit?.guestEmails.join(', ') ?? '')}</textarea>
          <label class="field-label" for="sch-organizer">Your email <span class="field-hint">(optional; makes it an invitation guests can accept)</span></label>
          <input id="sch-organizer" type="email" maxlength="254" placeholder="you@example.com" value="${escapeHtml(edit?.organizerEmail ?? remembered())}">
          <p class="record-form-error" role="alert" hidden></p>
        </div>
        <div class="room-dialog-actions"><button value="cancel" class="btn btn-quiet" formnovalidate>Cancel</button><button value="ok" class="btn btn-primary" id="sch-save">${edit ? 'Save changes' : 'Schedule'}</button></div></form>`;
      const $ = <T extends HTMLElement>(sel: string) => dialog.querySelector<T>(sel)!;
      const error = $('.record-form-error');
      const fail = (msg: string, focus?: string) => { error.hidden = false; error.textContent = msg; if (focus) $(focus).focus(); };
      const roomSel = $<HTMLSelectElement>('#sch-room'), roomName = $<HTMLInputElement>('#sch-room-name');
      roomSel.addEventListener('change', () => { roomName.hidden = roomSel.value !== '__new'; if (!roomName.hidden) roomName.focus(); });
      $('#sch-save').addEventListener('click', e => {
        e.preventDefault();
        error.hidden = true;
        const startsAt = toStartsAt($<HTMLInputElement>('#sch-date').value, $<HTMLInputElement>('#sch-time').value);
        if (!startsAt) return fail('Pick a date and start time.', '#sch-date');
        if (!edit && new Date(startsAt).getTime() < Date.now() - 5 * 60_000) return fail('That time has passed. Pick a later start.', '#sch-time');
        const guests = parseEmails($<HTMLTextAreaElement>('#sch-guests').value);
        if (guests.invalid.length) return fail(`Check these addresses: ${guests.invalid.slice(0, 3).join(', ')}`, '#sch-guests');
        const organizer = $<HTMLInputElement>('#sch-organizer').value.trim();
        if (organizer && parseEmails(organizer).emails.length !== 1) return fail('Your email doesn’t look right.', '#sch-organizer');
        if (roomSel.value === '__new' && !roomName.value.trim()) return fail('Name the new room.', '#sch-room-name');
        const save = $<HTMLButtonElement>('#sch-save');
        save.disabled = true;
        void (async () => {
          try {
            let roomId = roomSel.value;
            if (roomId === '__new') {
              const room = await this.roomsApi.create(roomName.value.trim());
              roomId = room.id;
              await this.opts.onRoomsChanged?.();
            }
            const draft: MeetingDraft = {
              roomId, title: $<HTMLInputElement>('#sch-title').value.trim(), description: $<HTMLTextAreaElement>('#sch-desc').value,
              startsAt, durationMin: Number($<HTMLSelectElement>('#sch-duration').value), timeZone: tz, guestEmails: guests.emails, organizerEmail: organizer,
            };
            remember(organizer);
            const saved = edit ? await this.api.update(edit.id, draft) : await this.api.create(draft);
            this.replace(saved);
            dialog.close('ok');
            const changed = !edit || saved.sequence !== edit.sequence;
            if (changed) this.shareDialog(saved, edit ? 'Meeting updated' : 'Meeting scheduled', edit ? 'Send guests the updated invite so their calendars change.' : 'Now invite your guests.');
            else this.opts.toast('No changes.');
          } catch (err) { fail(errorText(err)); save.disabled = false; }
        })();
      });
      dialog.addEventListener('close', () => { dialog.remove(); resolve(); }, { once: true });
      document.body.appendChild(dialog);
      dialog.showModal();
      (rooms.length ? $('#sch-title') : roomName).focus();
    });
  }

  /** After scheduling, editing or cancelling: the ways to tell guests. */
  private shareDialog(m: ScheduledMeeting, title: string, lead: string): void {
    const cancelled = m.status === 'cancelled';
    const dialog = document.createElement('dialog');
    dialog.className = 'room-dialog share-dialog';
    dialog.setAttribute('aria-labelledby', 'share-dialog-title');
    const ready = m.linkReady || cancelled;
    dialog.innerHTML = `<h2 id="share-dialog-title">${escapeHtml(title)}</h2><p>${escapeHtml(lead)}</p>
      <div class="share-summary"><strong>${escapeHtml(m.title)}</strong><span>${escapeHtml(m.invite.when)}</span><span class="recent-room">${escapeHtml(m.roomName)}</span></div>
      ${ready ? `<div class="share-actions">
        <button class="btn btn-primary" data-share="copy">${cancelled ? 'Copy cancellation text' : 'Copy invite text'}</button>
        <a class="btn btn-secondary" href="${escapeHtml(m.invite.icsPath)}" download data-share="ics">${cancelled ? 'Download cancellation (.ics)' : 'Download .ics'}</a>
        ${cancelled ? '' : `<a class="btn btn-secondary" href="${escapeHtml(m.invite.googleUrl)}" target="_blank" rel="noopener noreferrer">Add to Google Calendar</a>
        <a class="btn btn-secondary" href="${escapeHtml(m.invite.outlookUrl)}" target="_blank" rel="noopener noreferrer">Add to Outlook</a>`}
        ${m.invite.mailtoUrl ? `<a class="btn btn-secondary" href="${escapeHtml(m.invite.mailtoUrl)}">Email guests</a>` : ''}
      </div>
      ${!cancelled && !m.invite.remote ? '<p class="home-note">The room link uses this Mac’s own address until a private or public Tailscale link is set up, so it only works on this Mac for now.</p>' : ''}`
    : '<p class="home-note">The room’s link is off. Turn it on (in Rooms, choose New link), then send the invite from Meetings.</p>'}
      <form method="dialog" class="room-dialog-actions"><button class="btn btn-quiet" value="ok">Done</button></form>`;
    dialog.querySelector('[data-share="copy"]')?.addEventListener('click', async () => {
      if (await copyText(m.invite.text)) this.opts.toast(cancelled ? 'Cancellation text copied.' : 'Invite copied. Paste it into an email or message to your guests.');
      else void promptDialog('Copy the invite', 'Invite', m.invite.text, 'Done');
    });
    dialog.addEventListener('close', () => dialog.remove(), { once: true });
    document.body.appendChild(dialog);
    dialog.showModal();
    dialog.querySelector<HTMLElement>('[data-share="copy"], button[value="ok"]')?.focus();
  }

  /** The host's calendar subscription (this Mac and tailnet devices). */
  private async feedDialog(): Promise<void> {
    let feed: CalendarFeed;
    try { feed = await this.api.feed(); } catch (err) { this.opts.toast(errorText(err)); return; }
    const dialog = document.createElement('dialog');
    dialog.className = 'room-dialog feed-dialog';
    dialog.setAttribute('aria-labelledby', 'feed-dialog-title');
    const draw = () => {
      const on = feed.enabled && !feed.lost && !!feed.url;
      dialog.innerHTML = `<h2 id="feed-dialog-title">Calendar feed</h2>
        <p>Subscribe to your scheduled meetings from Calendar on this Mac or on your other devices in your tailnet. It updates by itself, and is never served through a public link.</p>
        ${on ? `<label class="field-label" for="feed-url">Feed address <span class="field-hint">(private: anyone with it sees your meetings and their links)</span></label>
          <input id="feed-url" readonly value="${escapeHtml(feed.url!)}">
          <div class="share-actions">
            <button class="btn btn-primary" data-feed="copy">Copy address</button>
            ${feed.remote && feed.webcalUrl ? `<a class="btn btn-secondary" href="${escapeHtml(feed.webcalUrl)}">Subscribe in Calendar</a>` : ''}
            ${feed.localUrl && feed.localUrl !== feed.url ? '<button class="btn btn-secondary" data-feed="copy-local">Copy address for this Mac</button>' : ''}
          </div>
          <p class="schedule-tz">Google Calendar can’t subscribe to a private feed (it fetches from Google’s servers). Use “Add to Google Calendar” on each meeting, or let the p2p.video app add meetings to a calendar.</p>
          <div class="room-dialog-actions feed-manage"><button class="btn btn-quiet" data-feed="rotate">Replace address</button><button class="btn btn-quiet danger" data-feed="revoke">Turn off</button></div>`
        : `<p class="schedule-tz">${feed.lost ? 'The feed address stopped working when this Mac’s p2p.video key changed. Create a new one.' : 'The feed is off.'}</p>
          <div class="share-actions"><button class="btn btn-primary" data-feed="rotate">Turn on feed</button></div>`}
        <form method="dialog" class="room-dialog-actions"><button class="btn btn-quiet" value="ok">Done</button></form>`;
    };
    draw();
    dialog.addEventListener('click', async e => {
      const btn = (e.target as HTMLElement).closest<HTMLElement>('[data-feed]');
      if (!btn) return;
      try {
        switch (btn.dataset.feed) {
          case 'copy':
            if (feed.url && await copyText(feed.url)) this.opts.toast('Feed address copied. In Calendar choose File → New Calendar Subscription.');
            break;
          case 'copy-local':
            if (feed.localUrl && await copyText(feed.localUrl)) this.opts.toast('Address for this Mac copied.');
            break;
          case 'rotate':
            if (feed.enabled && !feed.lost && !await confirmDialog('Replace the feed address?', 'Calendars subscribed with the current address stop updating. Subscribe again with the new one.', 'Replace')) return;
            feed = await this.api.rotateFeed();
            draw();
            break;
          case 'revoke':
            if (!await confirmDialog('Turn off the calendar feed?', 'Subscribed calendars stop updating.', 'Turn off', true)) return;
            feed = await this.api.revokeFeed();
            draw();
            break;
        }
      } catch (err) { this.opts.toast(errorText(err)); }
    });
    dialog.addEventListener('close', () => dialog.remove(), { once: true });
    document.body.appendChild(dialog);
    dialog.showModal();
  }
}
