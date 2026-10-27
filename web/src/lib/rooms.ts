// Persistent rooms and the host home [SL-P1-5]: typed client for the room,
// home, lobby and meeting-end endpoints (host/internal/httpserver/rooms.go),
// plus the pure helpers the home and lobby UIs use. Guest links carry their
// secret: never log them or put them anywhere but the clipboard.

export interface RecordingDefaults {
  video_bitrate_bps?: number;
  video_codec?: string;
  target_fps?: number;
}

export interface RoomSettings {
  autoAdmit: boolean;
  lockOnStart: boolean;
  recording?: RecordingDefaults;
  hostName?: string;
}

export interface RoomView {
  id: string;
  name: string;
  slug: string;
  createdAt: string;
  updatedAt: string;
  settings: RoomSettings;
  archived: boolean;
  linkEnabled: boolean;
  linkLost?: boolean;
  guestUrl?: string;
  guestPath?: string;
  guestUrlRemote: boolean;
  live: boolean;
  liveSessionId?: string;
  hostInCall: boolean;
  /** The room's producer link [SL-P1-11] (secret in url/path; clipboard only). */
  producer?: { enabled: boolean; url?: string; path?: string; remote: boolean };
}

export interface TakeSummary {
  id: string;
  number: number;
  status: string;
  wallClockStart: string;
}

export interface HomeSession {
  id: string;
  title: string;
  createdAt: string;
  endedAt?: string;
  roomId?: string;
  roomName?: string;
  takes: TakeSummary[];
  live: boolean;
  hostToken: string;
}

export interface HomeData {
  rooms: RoomView[];
  recent: HomeSession[];
  guestLinksRemote: boolean;
}

export interface RoomLobby {
  name: string;
  hostName?: string;
  live: boolean;
  sessionId?: string;
  /** "producer" for the room's producer link [SL-P1-11]. */
  role?: 'producer';
}

export interface OpenedSession {
  sessionId: string;
  token: string;
  host: boolean;
  title?: string;
}

export interface RoomStart {
  sessionId: string;
  title: string;
  roomId: string;
  hostToken: string;
  hostUrl: string;
  resumed: boolean;
}

/** HTTP failure with the server's (credential-free) message. */
export class ApiError extends Error {
  constructor(readonly status: number, message: string) { super(message); this.name = 'ApiError'; }
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

/** A lower third a room remembers [SL-P5-4]; key is the normalized display name. */
export interface RoomTitle { key: string; displayName: string; name?: string; title?: string; updatedAt: string }

export class RoomsApi {
  constructor(private readonly fetchImpl: Fetch = (...a) => fetch(...a)) {}
  home(): Promise<HomeData> { return call(this.fetchImpl, '/api/v1/home'); }
  create(name: string): Promise<RoomView> { return call(this.fetchImpl, '/api/v1/rooms', json('POST', { name })); }
  update(id: string, patch: { name?: string; archived?: boolean; settings?: Partial<Pick<RoomSettings, 'autoAdmit' | 'lockOnStart'>> }): Promise<RoomView> {
    return call(this.fetchImpl, `/api/v1/rooms/${encodeURIComponent(id)}`, json('PATCH', patch));
  }
  remove(id: string): Promise<void> { return call(this.fetchImpl, `/api/v1/rooms/${encodeURIComponent(id)}`, { method: 'DELETE' }); }
  rotate(id: string): Promise<RoomView> { return call(this.fetchImpl, `/api/v1/rooms/${encodeURIComponent(id)}/rotate`, { method: 'POST' }); }
  revoke(id: string): Promise<RoomView> { return call(this.fetchImpl, `/api/v1/rooms/${encodeURIComponent(id)}/revoke`, { method: 'POST' }); }
  /** Issues (or replaces) the room's producer link; the old one stops working. */
  producerRotate(id: string): Promise<RoomView> { return call(this.fetchImpl, `/api/v1/rooms/${encodeURIComponent(id)}/producer/rotate`, { method: 'POST' }); }
  /** Lower thirds the room remembers by display name for its next episode [SL-P5-4]. */
  titles(id: string): Promise<{ titles: RoomTitle[] }> { return call(this.fetchImpl, `/api/v1/rooms/${encodeURIComponent(id)}/titles`); }
  /** Saves a remembered lower third; both name and title empty forgets it. */
  saveTitle(id: string, t: { displayName: string; name: string; title: string }): Promise<{ titles: RoomTitle[] }> {
    return call(this.fetchImpl, `/api/v1/rooms/${encodeURIComponent(id)}/titles`, json('PUT', t));
  }
  forgetTitle(id: string, key: string): Promise<{ titles: RoomTitle[] }> {
    return call(this.fetchImpl, `/api/v1/rooms/${encodeURIComponent(id)}/titles?key=${encodeURIComponent(key)}`, { method: 'DELETE' });
  }
  start(id: string, hostName: string): Promise<RoomStart> {
    return call(this.fetchImpl, `/api/v1/rooms/${encodeURIComponent(id)}/start`, json('POST', { hostName }));
  }
  instant(title: string): Promise<{ id: string; title: string; hostToken: string }> {
    return call(this.fetchImpl, '/api/v1/sessions', json('POST', { title, autoAdmit: false }));
  }
  lobby(slug: string, token: string): Promise<RoomLobby> {
    return call(this.fetchImpl, `/api/v1/rooms/lobby?room=${encodeURIComponent(slug)}`, { headers: { Authorization: `Bearer ${token}` } });
  }
  end(sessionId: string, hostToken: string): Promise<{ ended: boolean; roomId?: string }> {
    return call(this.fetchImpl, `/api/v1/sessions/${encodeURIComponent(sessionId)}/end`, { method: 'POST', headers: { Authorization: `Bearer ${hostToken}` } });
  }
}

/** Where this page load is headed, from its URL. */
export type EntryMode =
  | { kind: 'host-home'; roomId?: string }
  | { kind: 'guest-room'; slug: string; token: string }
  | { kind: 'session' };

export function entryMode(pathname: string, search: string): EntryMode {
  const q = new URLSearchParams(search);
  if (q.get('session')) return { kind: 'session' };
  if (pathname === '/join') {
    const slug = q.get('room'), token = q.get('token');
    return slug && token ? { kind: 'guest-room', slug, token } : { kind: 'session' };
  }
  return { kind: 'host-home', roomId: q.get('room') || undefined };
}

/** "Ben" or a neutral fallback for the waiting-for-host copy. */
export function hostLabel(name: string | undefined): string {
  const n = (name || '').trim();
  return n || 'the host';
}

const sameDay = (a: Date, b: Date) => a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();

/** Compact meeting date: "Today 14:05", "Yesterday 09:30", "Oct 3, 14:05". */
export function formatWhen(iso: string, now = new Date()): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  const time = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false });
  if (sameDay(d, now)) return `Today ${time}`;
  const y = new Date(now); y.setDate(now.getDate() - 1);
  if (sameDay(d, y)) return `Yesterday ${time}`;
  const date = d.toLocaleDateString([], { month: 'short', day: 'numeric', ...(d.getFullYear() !== now.getFullYear() ? { year: 'numeric' } : {}) });
  return `${date}, ${time}`;
}

/** "42 min", "1 h 05 min"; empty for an open or unparseable meeting. */
export function formatDuration(startIso: string, endIso?: string): string {
  if (!endIso) return '';
  const ms = new Date(endIso).getTime() - new Date(startIso).getTime();
  if (!Number.isFinite(ms) || ms < 0) return '';
  const min = Math.max(1, Math.round(ms / 60000));
  if (min < 60) return `${min} min`;
  return `${Math.floor(min / 60)} h ${String(min % 60).padStart(2, '0')} min`;
}

/** Human take status for the recent list. */
export function takeStatusLabel(status: string): string {
  switch (status) {
    case 'recording': return 'Recording';
    case 'stopped': case 'uploading': return 'Uploading';
    case 'ready': return 'Ready to export';
    case 'processing': return 'Exporting';
    case 'complete': return 'Exported';
    case 'failed': return 'Needs attention';
    default: return status;
  }
}

/** One-line summary of a meeting's recordings ("No recording", "2 takes · 1 exported"). */
export function takesSummary(takes: TakeSummary[]): string {
  if (!takes.length) return 'No recording';
  const exported = takes.filter(t => t.status === 'complete').length;
  const base = `${takes.length} take${takes.length === 1 ? '' : 's'}`;
  return exported ? `${base} · ${exported} exported` : `${base} · ${takeStatusLabel(takes[takes.length - 1].status).toLowerCase()}`;
}

/** Display-safe guest link: the room path without its secret. */
export function redactedLink(view: Pick<RoomView, 'slug'>): string {
  return `/join?room=${view.slug}`;
}
