// Scheduled meetings and calendar invites [SL-P6-1]: typed client for
// /api/v1/schedule… and /api/v1/calendar/feed… (host/internal/httpserver/
// schedule.go), plus the pure helpers the Upcoming panel uses. Invite links,
// texts and the feed URL carry secrets (the room's guest link, the feed key):
// clipboard, downloads and the host's own calendar only; never logged.
import { ApiError } from './rooms';

export interface MeetingInvite {
  when: string;
  /** PUBLISH, REQUEST (organizer + guests) or CANCEL. */
  method: 'PUBLISH' | 'REQUEST' | 'CANCEL';
  guestUrl?: string;
  remote: boolean;
  text: string;
  googleUrl: string;
  outlookUrl: string;
  mailtoUrl?: string;
  icsPath: string;
}

export interface ScheduledMeeting {
  id: string;
  roomId: string;
  title: string;
  description: string;
  startsAt: string;
  endsAt: string;
  durationMin: number;
  timeZone: string;
  guestEmails: string[];
  organizerEmail?: string;
  uid: string;
  sequence: number;
  status: 'scheduled' | 'cancelled';
  createdAt: string;
  updatedAt: string;
  roomName: string;
  roomArchived: boolean;
  roomLive: boolean;
  linkReady: boolean;
  invite: MeetingInvite;
}

export interface MeetingDraft {
  roomId: string;
  title?: string;
  description?: string;
  startsAt: string;
  durationMin: number;
  timeZone: string;
  guestEmails: string[];
  organizerEmail?: string;
}

export interface CalendarFeed {
  enabled: boolean;
  lost?: boolean;
  url?: string;
  webcalUrl?: string;
  localUrl?: string;
  remote: boolean;
}

type Fetch = typeof fetch;

async function call<T>(fetchImpl: Fetch, path: string, init: RequestInit = {}): Promise<T> {
  const res = await fetchImpl(path, init);
  if (!res.ok) {
    const text = (await res.text().catch(() => '')).trim();
    throw new ApiError(res.status, text || `Request failed (${res.status})`);
  }
  if (res.status === 204) return undefined as T;
  return res.json() as Promise<T>;
}

const json = (method: string, body?: unknown): RequestInit => ({
  method,
  headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
  body: body === undefined ? undefined : JSON.stringify(body),
});

export class ScheduleApi {
  constructor(private readonly fetchImpl: Fetch = (...a) => fetch(...a)) {}
  list(): Promise<{ meetings: ScheduledMeeting[] }> { return call(this.fetchImpl, '/api/v1/schedule'); }
  create(draft: MeetingDraft): Promise<ScheduledMeeting> { return call(this.fetchImpl, '/api/v1/schedule', json('POST', draft)); }
  update(id: string, patch: Partial<MeetingDraft>): Promise<ScheduledMeeting> {
    return call(this.fetchImpl, `/api/v1/schedule/${encodeURIComponent(id)}`, json('PATCH', patch));
  }
  cancel(id: string): Promise<ScheduledMeeting> { return call(this.fetchImpl, `/api/v1/schedule/${encodeURIComponent(id)}/cancel`, { method: 'POST' }); }
  remove(id: string): Promise<void> { return call(this.fetchImpl, `/api/v1/schedule/${encodeURIComponent(id)}`, { method: 'DELETE' }); }
  feed(): Promise<CalendarFeed> { return call(this.fetchImpl, '/api/v1/calendar/feed'); }
  rotateFeed(): Promise<CalendarFeed> { return call(this.fetchImpl, '/api/v1/calendar/feed/rotate', { method: 'POST' }); }
  revokeFeed(): Promise<CalendarFeed> { return call(this.fetchImpl, '/api/v1/calendar/feed/revoke', { method: 'POST' }); }
}

/** The browser's IANA time zone ("UTC" when unknown). */
export function localTimeZone(): string {
  try { return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'; } catch { return 'UTC'; }
}

const pad = (n: number) => String(n).padStart(2, '0');

/** A local date ("2026-10-30") and time ("10:00") as an RFC 3339 UTC instant, or null. */
export function toStartsAt(date: string, time: string): string | null {
  const d = /^(\d{4})-(\d{2})-(\d{2})$/.exec(date.trim());
  const t = /^(\d{1,2}):(\d{2})$/.exec(time.trim());
  if (!d || !t) return null;
  const [y, mo, da, h, mi] = [+d[1], +d[2], +d[3], +t[1], +t[2]];
  if (mo < 1 || mo > 12 || da < 1 || da > 31 || h > 23 || mi > 59) return null;
  const at = new Date(y, mo - 1, da, h, mi);
  if (at.getFullYear() !== y || at.getMonth() !== mo - 1 || at.getDate() !== da) return null; // Feb 30 etc.
  return at.toISOString().replace(/\.\d{3}Z$/, 'Z');
}

/** The local date and time inputs for an instant (inverse of toStartsAt). */
export function localParts(iso: string): { date: string; time: string } {
  const d = new Date(iso);
  return { date: `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`, time: `${pad(d.getHours())}:${pad(d.getMinutes())}` };
}

/** The next half hour at least 10 minutes away: the form's default start. */
export function defaultStart(now = new Date()): Date {
  const d = new Date(now.getTime() + 10 * 60_000);
  d.setSeconds(0, 0);
  d.setMinutes(d.getMinutes() <= 30 ? (d.getMinutes() === 0 ? 0 : 30) : 60);
  return d;
}

// Mirrors the daemon's conservative address check (schedule.go emailPattern).
const EMAIL = /^[A-Za-z0-9._+-]{1,64}@[A-Za-z0-9-]{1,63}(\.[A-Za-z0-9-]{1,63})+$/;

/** Splits a guest list on commas, semicolons, spaces or new lines. */
export function parseEmails(text: string): { emails: string[]; invalid: string[] } {
  const emails: string[] = [], invalid: string[] = [];
  const seen = new Set<string>();
  for (const raw of text.split(/[\s,;]+/)) {
    const addr = raw.trim().replace(/^<|>$/g, '');
    if (!addr) continue;
    if (!EMAIL.test(addr) || addr.length > 254) { invalid.push(addr); continue; }
    if (!seen.has(addr.toLowerCase())) { seen.add(addr.toLowerCase()); emails.push(addr); }
  }
  return { emails, invalid };
}

export type MeetingPhase = 'cancelled' | 'upcoming' | 'soon' | 'now' | 'over';

/** Where a meeting is relative to now: soon = starts within 15 minutes. */
export function meetingPhase(m: Pick<ScheduledMeeting, 'status' | 'startsAt' | 'endsAt'>, now = new Date()): MeetingPhase {
  if (m.status === 'cancelled') return 'cancelled';
  const start = new Date(m.startsAt).getTime(), end = new Date(m.endsAt).getTime(), t = now.getTime();
  if (t >= end) return 'over';
  if (t >= start) return 'now';
  if (start - t <= 15 * 60_000) return 'soon';
  return 'upcoming';
}

const sameDay = (a: Date, b: Date) => a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
const hm = (d: Date) => d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });

/** "Today · 10:00 AM – 11:30 AM", "Tomorrow · …", "Fri, Oct 30 · …" in this browser's zone. */
export function meetingWhen(m: Pick<ScheduledMeeting, 'startsAt' | 'endsAt'>, now = new Date()): string {
  const s = new Date(m.startsAt), e = new Date(m.endsAt);
  if (Number.isNaN(s.getTime()) || Number.isNaN(e.getTime())) return '';
  const tomorrow = new Date(now); tomorrow.setDate(now.getDate() + 1);
  const day = sameDay(s, now) ? 'Today' : sameDay(s, tomorrow) ? 'Tomorrow'
    : s.toLocaleDateString([], { weekday: 'short', month: 'short', day: 'numeric', ...(s.getFullYear() !== now.getFullYear() ? { year: 'numeric' } : {}) });
  return `${day} · ${hm(s)} – ${hm(e)}`;
}

/** "in 5 min", "in 2 h", "started 10 min ago"; empty when far away or over. */
export function relativeStart(m: Pick<ScheduledMeeting, 'startsAt' | 'endsAt'>, now = new Date()): string {
  const diff = new Date(m.startsAt).getTime() - now.getTime();
  const min = Math.round(Math.abs(diff) / 60_000);
  if (diff > 0) {
    if (min < 1) return 'starting now';
    if (min < 60) return `in ${min} min`;
    if (min < 24 * 60) return `in ${Math.round(min / 60)} h`;
    return '';
  }
  if (now.getTime() >= new Date(m.endsAt).getTime()) return '';
  return min < 1 ? 'starting now' : `started ${min} min ago`;
}

/** Duration options for the form (minutes). */
export const DURATIONS = [15, 30, 45, 60, 90, 120, 180];

export function durationLabel(min: number): string {
  if (min < 60) return `${min} min`;
  const h = Math.floor(min / 60), r = min % 60;
  return r ? `${h} h ${r} min` : `${h} h`;
}
