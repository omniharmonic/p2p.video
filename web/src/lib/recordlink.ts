// Async record links [SL-P5-3]: typed clients for the guest side
// (/api/v1/record/…) and the host side (/api/v1/recordlinks…), mirroring
// host/internal/httpserver/recordlinks.go, plus the solo recorder's state
// machine and pure helpers (unit-tested in node). No DOM, no imports with
// side effects. The link secret and the submission key are credentials:
// never log them; they live in the address bar only until the page reads
// them, then in sessionStorage (secret) and localStorage (key, per link).

export interface RecordingPreset {
  video_bitrate_bps?: number;
  video_codec?: string;
  target_fps?: number;
  recording_mic_mode?: 'auto' | 'raw' | 'clean' | '';
}

/** GET /api/v1/record/{id}. */
export interface RecordLinkPublic {
  id: string;
  title: string;
  prompt: string;
  maxDurationSec: number;
  expiresAt: string;
  accepting: boolean;
  reason?: 'full';
  recording: RecordingPreset;
}

export interface RecordGuestTrack { id: string; kind: 'video' | 'audio'; label: string; status: string; bytes: number }
export interface RecordGuestTake { id: string; number: number; status: string; durationSec: number; tracks: RecordGuestTrack[] }

/** The guest's own submission. uploadToken is fresh on every read. */
export interface RecordGuestSubmission {
  id: string;
  name: string;
  status: 'draft' | 'submitted';
  participantId: string;
  uploadToken: string;
  maxDurationSec: number;
  takes: RecordGuestTake[];
}

export interface RecordTakeStart { takeId: string; number: number; startAtHostUs: number; maxDurationSec: number }

/** HTTP failure with the server's (credential-free) message. */
export class RecordApiError extends Error {
  constructor(readonly status: number, message: string) { super(message); this.name = 'RecordApiError'; }
}

type Fetch = typeof fetch;

async function request<T>(fetchImpl: Fetch, path: string, init: RequestInit): Promise<T> {
  const res = await fetchImpl(path, { cache: 'no-store', ...init });
  if (!res.ok) {
    const text = (await res.text().catch(() => '')).trim();
    throw new RecordApiError(res.status, text || `Request failed (${res.status})`);
  }
  if (res.status === 204) return undefined as T;
  return res.json() as Promise<T>;
}

/** The guest side: the link secret on every call, plus the submission key. */
export class RecordGuestApi {
  constructor(readonly linkId: string, private readonly secret: string, private readonly fetchImpl: Fetch = (...a) => fetch(...a)) {}

  private headers(key?: string, json = false): HeadersInit {
    return { Authorization: `Bearer ${this.secret}`, ...(key ? { 'X-Submission-Key': key } : {}), ...(json ? { 'Content-Type': 'application/json' } : {}) };
  }
  private base(): string { return `/api/v1/record/${encodeURIComponent(this.linkId)}`; }
  private sub(id: string, rest = ''): string { return `${this.base()}/submissions/${encodeURIComponent(id)}${rest}`; }

  info(): Promise<RecordLinkPublic> { return request(this.fetchImpl, this.base(), { headers: this.headers() }); }
  start(name: string): Promise<{ key: string; submission: RecordGuestSubmission }> {
    return request(this.fetchImpl, `${this.base()}/submissions`, { method: 'POST', headers: this.headers(undefined, true), body: JSON.stringify({ name }) });
  }
  get(id: string, key: string): Promise<RecordGuestSubmission> { return request(this.fetchImpl, this.sub(id), { headers: this.headers(key) }); }
  clock(id: string, key: string, t0: number): Promise<{ t0: number; t1: number; t2: number }> {
    return request(this.fetchImpl, this.sub(id, '/clock'), { method: 'POST', headers: this.headers(key, true), body: JSON.stringify({ t0 }) });
  }
  startTake(id: string, key: string): Promise<RecordTakeStart> { return request(this.fetchImpl, this.sub(id, '/takes'), { method: 'POST', headers: this.headers(key) }); }
  stopTake(id: string, key: string, takeId: string): Promise<{ takeId: string; status: string; durationSec: number }> {
    return request(this.fetchImpl, this.sub(id, `/takes/${encodeURIComponent(takeId)}/stop`), { method: 'POST', headers: this.headers(key) });
  }
  discardTake(id: string, key: string, takeId: string): Promise<void> {
    return request(this.fetchImpl, this.sub(id, `/takes/${encodeURIComponent(takeId)}`), { method: 'DELETE', headers: this.headers(key) });
  }
  submit(id: string, key: string): Promise<RecordGuestSubmission> { return request(this.fetchImpl, this.sub(id, '/submit'), { method: 'POST', headers: this.headers(key) }); }
}

// ----------------------------------------------------------------- host side

export interface RecordTakeView {
  id: string;
  number: number;
  status: string;
  wallClockStart: string;
  durationSec: number;
  tracks: number;
  assembled: number;
  originalsReady: boolean;
}

export interface RecordSubmissionView {
  id: string;
  linkId: string;
  name: string;
  status: 'draft' | 'submitted';
  createdAt: string;
  submittedAt?: string;
  hostToken?: string;
  takes: RecordTakeView[];
}

export interface RecordLinkView {
  id: string;
  title: string;
  prompt: string;
  roomId?: string;
  roomName?: string;
  createdAt: string;
  expiresAt: string;
  maxDurationSec: number;
  maxSubmissions: number;
  maxTotalBytes: number;
  linkEnabled: boolean;
  linkLost?: boolean;
  url?: string;
  path?: string;
  urlRemote: boolean;
  expired: boolean;
  submitted: number;
  drafts: number;
  /** Drafts idle long enough to clean up (included in drafts) [SL-P5-4]. */
  abandonedDrafts?: number;
  usedBytes: number;
  submissions: RecordSubmissionView[];
}

export interface RecordLinkCreate {
  title: string;
  prompt?: string;
  roomId?: string;
  maxDurationSec?: number;
  expiresInHours?: number;
  maxSubmissions?: number;
  maxTotalMB?: number;
}

/** Host management (admin cookie or desktop token; never through Funnel). */
export class RecordLinksApi {
  constructor(private readonly fetchImpl: Fetch = (...a) => fetch(...a)) {}
  list(): Promise<{ links: RecordLinkView[] }> { return request(this.fetchImpl, '/api/v1/recordlinks', {}); }
  create(body: RecordLinkCreate): Promise<RecordLinkView> {
    return request(this.fetchImpl, '/api/v1/recordlinks', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  }
  update(id: string, patch: Partial<RecordLinkCreate> & { expiresAt?: string }): Promise<RecordLinkView> {
    return request(this.fetchImpl, `/api/v1/recordlinks/${encodeURIComponent(id)}`, { method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(patch) });
  }
  rotate(id: string): Promise<RecordLinkView> { return request(this.fetchImpl, `/api/v1/recordlinks/${encodeURIComponent(id)}/rotate`, { method: 'POST' }); }
  revoke(id: string): Promise<RecordLinkView> { return request(this.fetchImpl, `/api/v1/recordlinks/${encodeURIComponent(id)}/revoke`, { method: 'POST' }); }
  remove(id: string): Promise<void> { return request(this.fetchImpl, `/api/v1/recordlinks/${encodeURIComponent(id)}`, { method: 'DELETE' }); }
  removeSubmission(id: string, sessionId: string): Promise<void> {
    return request(this.fetchImpl, `/api/v1/recordlinks/${encodeURIComponent(id)}/submissions/${encodeURIComponent(sessionId)}`, { method: 'DELETE' });
  }
  /** Draft submissions with activity and the cleanup policy [SL-P5-4]. */
  drafts(): Promise<{ drafts: RecordDraftView[]; policy: DraftPolicy }> { return request(this.fetchImpl, '/api/v1/recordlinks/drafts', {}); }
  /** Deletes one abandoned draft (409 if it is in use again). */
  deleteDraft(sessionId: string): Promise<void> {
    return request(this.fetchImpl, `/api/v1/recordlinks/drafts/${encodeURIComponent(sessionId)}`, { method: 'DELETE' });
  }
  /** Deletes every abandoned draft, or just ids. */
  cleanupDrafts(ids?: string[]): Promise<{ deleted: string[]; skipped: string[]; bytes: number }> {
    return request(this.fetchImpl, '/api/v1/recordlinks/drafts/cleanup', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(ids ? { ids } : {}) });
  }
}

/** A draft submission on the host dashboard [SL-P5-4]. */
export interface RecordDraftView {
  id: string;
  linkId: string;
  linkTitle: string;
  linkClosed: boolean;
  name: string;
  createdAt: string;
  lastActivity: string;
  lastUpload?: string;
  recording: boolean;
  takes: number;
  bytes: number;
  abandoned: boolean;
  reason?: 'idle' | 'closed';
  autoDeleteAt?: string;
}

export interface DraftPolicy {
  autoCleanup: boolean;
  expiryDays: number;
  closedLinkHours: number;
  uploadGuardMinutes: number;
  abandonedAfterHours: number;
}

/** How long ago, coarsely ("3 days ago", "5 hours ago", "just now"). */
export function agoLabel(iso: string, now = Date.now()): string {
  const sec = Math.max(0, (now - new Date(iso).getTime()) / 1000);
  if (sec < 90) return 'just now';
  const units: Array<[number, string]> = [[86_400, 'day'], [3_600, 'hour'], [60, 'minute']];
  for (const [n, name] of units) {
    if (sec >= n) { const v = Math.floor(sec / n); return `${v} ${name}${v === 1 ? '' : 's'} ago`; }
  }
  return 'just now';
}

/** The cleanup policy in one sentence for the panel. */
export function draftPolicyText(p: DraftPolicy): string {
  if (!p.autoCleanup) return 'Automatic cleanup is off; delete abandoned drafts here.';
  return `Deleted automatically after ${p.expiryDays} day${p.expiryDays === 1 ? '' : 's'} without activity, or ${p.closedLinkHours} hour${p.closedLinkHours === 1 ? '' : 's'} after their link expires or is turned off. Sent recordings are never touched.`;
}

// ---------------------------------------------------------------- page state

/** Where the guest page is. */
export type SoloPhase =
  | 'loading'     // reading the link
  | 'invalid'     // wrong, rotated or revoked link (401)
  | 'expired'     // 410
  | 'full'        // the link has all its submissions and this browser has none
  | 'setup'       // green room: devices, pre-flight, name; no take yet
  | 'countdown'   // 3-2-1 before a take
  | 'recording'
  | 'stopping'    // recorders flushing, take being stopped on the host
  | 'review'      // at least one take kept; record again, delete or submit
  | 'submitting'
  | 'submitted';

export interface SoloTake {
  id: string;
  number: number;
  durationSec: number;
  /** Host-side status: recording | stopped. */
  status: string;
  uploadedBytes: number;
  pendingBytes: number;
  /** Every track of the take has a host assembly receipt. */
  uploaded: boolean;
  error?: string;
}

export interface SoloState {
  phase: SoloPhase;
  takes: SoloTake[];
  activeTakeId?: string;
  message?: string;
}

export type SoloEvent =
  | { type: 'link-ok'; accepting: boolean }
  | { type: 'link-error'; status: number; message?: string }
  | { type: 'resumed'; submission: Pick<RecordGuestSubmission, 'status' | 'takes'> }
  | { type: 'record' }
  | { type: 'started'; take: Pick<RecordTakeStart, 'takeId' | 'number'> }
  | { type: 'start-failed'; message: string }
  | { type: 'stop' }
  | { type: 'stopped'; takeId: string; durationSec: number }
  | { type: 'discarded'; takeId: string }
  | { type: 'progress'; takeId: string; uploadedBytes: number; pendingBytes: number; uploaded: boolean; error?: string }
  | { type: 'submit' }
  | { type: 'submitted' }
  | { type: 'submit-failed'; message: string };

export const INITIAL_SOLO_STATE: SoloState = { phase: 'loading', takes: [] };

/** What a failed link read means for the page. */
export function linkErrorPhase(status: number): Extract<SoloPhase, 'invalid' | 'expired' | 'full'> | null {
  if (status === 401 || status === 404) return 'invalid';
  if (status === 410) return 'expired';
  if (status === 409) return 'full';
  return null;
}

const idle = (s: SoloState): SoloPhase => (s.takes.length ? 'review' : 'setup');

/** The guest page's state machine. Events that don't fit the phase are ignored. */
export function soloReduce(s: SoloState, e: SoloEvent): SoloState {
  switch (e.type) {
    case 'link-ok':
      if (s.phase !== 'loading') return s;
      return { ...s, phase: e.accepting ? 'setup' : 'full', message: undefined };
    case 'link-error': {
      const phase = linkErrorPhase(e.status);
      // A submitted recording keeps showing its uploads even if the link
      // was rotated or expired since.
      if (!phase || s.phase === 'submitted') return { ...s, message: e.message };
      return { ...s, phase, message: e.message, activeTakeId: undefined };
    }
    case 'resumed': {
      const takes: SoloTake[] = e.submission.takes.map(t => ({ id: t.id, number: t.number, durationSec: t.durationSec, status: t.status,
        uploadedBytes: t.tracks.reduce((n, tr) => n + tr.bytes, 0), pendingBytes: 0,
        uploaded: t.tracks.length > 0 && t.tracks.every(tr => tr.status === 'assembled' || tr.status === 'processed' || tr.status === 'partial') }));
      if (e.submission.status === 'submitted') return { phase: 'submitted', takes };
      return { phase: takes.length ? 'review' : 'setup', takes };
    }
    case 'record':
      if (!canRecord(s)) return s;
      return { ...s, phase: 'countdown', message: undefined };
    case 'started':
      if (s.phase !== 'countdown') return s;
      return { ...s, phase: 'recording', activeTakeId: e.take.takeId,
        takes: [...s.takes, { id: e.take.takeId, number: e.take.number, durationSec: 0, status: 'recording', uploadedBytes: 0, pendingBytes: 0, uploaded: false }] };
    case 'start-failed':
      if (s.phase !== 'countdown' && s.phase !== 'recording') return s;
      return { ...s, phase: idle(s), activeTakeId: undefined, message: e.message };
    case 'stop':
      if (s.phase === 'countdown') return { ...s, phase: idle(s) };
      if (s.phase !== 'recording') return s;
      return { ...s, phase: 'stopping' };
    case 'stopped':
      return { ...s, phase: s.phase === 'stopping' || s.phase === 'recording' ? 'review' : s.phase, activeTakeId: s.activeTakeId === e.takeId ? undefined : s.activeTakeId,
        takes: s.takes.map(t => (t.id === e.takeId ? { ...t, status: 'stopped', durationSec: e.durationSec } : t)) };
    case 'discarded': {
      if (s.phase === 'submitted' || s.phase === 'submitting') return s;
      const takes = s.takes.filter(t => t.id !== e.takeId);
      const phase = s.phase === 'review' || s.phase === 'setup' ? (takes.length ? 'review' : 'setup') : s.phase;
      return { ...s, takes, phase };
    }
    case 'progress':
      return { ...s, takes: s.takes.map(t => (t.id === e.takeId ? { ...t, uploadedBytes: e.uploadedBytes, pendingBytes: e.pendingBytes, uploaded: e.uploaded, error: e.error } : t)) };
    case 'submit':
      if (!canSubmit(s)) return s;
      return { ...s, phase: 'submitting', message: undefined };
    case 'submitted':
      if (s.phase !== 'submitting') return s;
      return { ...s, phase: 'submitted' };
    case 'submit-failed':
      if (s.phase !== 'submitting') return s;
      return { ...s, phase: 'review', message: e.message };
  }
}

/** A new take may start: devices ready (setup/review), nothing recording. */
export function canRecord(s: SoloState, maxTakes = 10): boolean {
  return (s.phase === 'setup' || s.phase === 'review') && s.takes.length < maxTakes;
}

/** Submit needs a kept take and nothing recording. Uploads may continue after. */
export function canSubmit(s: SoloState): boolean {
  return s.phase === 'review' && s.takes.length > 0 && s.takes.every(t => t.status !== 'recording');
}

/** A take can be deleted (re-record) until the submission is sent. */
export function canDiscard(s: SoloState, takeId: string): boolean {
  return (s.phase === 'review' || s.phase === 'setup') && s.takes.some(t => t.id === takeId && t.status !== 'recording');
}

/** Bytes still to upload across takes, and whether every take is in. */
export function uploadSummary(takes: SoloTake[]): { pendingBytes: number; uploadedBytes: number; allUploaded: boolean } {
  return {
    pendingBytes: takes.reduce((n, t) => n + t.pendingBytes, 0),
    uploadedBytes: takes.reduce((n, t) => n + t.uploadedBytes, 0),
    allUploaded: takes.length > 0 && takes.every(t => t.uploaded),
  };
}

// ------------------------------------------------------------------ helpers

/** The link from the page URL (/record?link=<id>&token=<secret>). */
export function linkFromUrl(search: string): { linkId: string; secret: string } | null {
  const q = new URLSearchParams(search);
  const linkId = q.get('link') || '', secret = q.get('token') || '';
  return /^rl_[0-9a-f]{6,32}$/.test(linkId) && /^[0-9a-f]{64}$/.test(secret) ? { linkId, secret } : null;
}

/** localStorage key for this browser's submission under a link. */
export function draftKey(linkId: string): string { return `studiolink.record.${linkId}`; }
/** sessionStorage key for the link secret (survives a reload of this tab only). */
export function secretKey(linkId: string): string { return `studiolink.recordSecret.${linkId}`; }

export interface SavedDraft { submissionId: string; key: string; name: string }

export function parseDraft(raw: string | null | undefined): SavedDraft | null {
  if (!raw) return null;
  try {
    const v = JSON.parse(raw) as Partial<SavedDraft>;
    if (typeof v.submissionId === 'string' && /^[\w-]{1,40}$/.test(v.submissionId) && typeof v.key === 'string' && /^[0-9a-f]{64}$/.test(v.key)) {
      return { submissionId: v.submissionId, key: v.key, name: typeof v.name === 'string' ? v.name.slice(0, 80) : '' };
    }
  } catch { /* fall through */ }
  return null;
}

/** "0:07", "12:34", "1:02:03". */
export function formatClock(totalSec: number): string {
  const s = Math.max(0, Math.floor(totalSec));
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60;
  return h ? `${h}:${String(m).padStart(2, '0')}:${String(sec).padStart(2, '0')}` : `${m}:${String(sec).padStart(2, '0')}`;
}

/** "30 min", "1 h", "1 h 30 min", "45 s". */
export function formatLimit(sec: number): string {
  if (sec < 60) return `${sec} s`;
  const m = Math.round(sec / 60), h = Math.floor(m / 60), rest = m % 60;
  return h ? `${h} h${rest ? ` ${rest} min` : ''}` : `${m} min`;
}

export function formatBytes(bytes: number): string {
  if (bytes >= 1024 ** 3) return `${(bytes / 1024 ** 3).toFixed(1)} GB`;
  if (bytes >= 1024 ** 2) return `${(bytes / 1024 ** 2).toFixed(1)} MB`;
  if (bytes >= 1024) return `${Math.round(bytes / 1024)} KB`;
  return `${bytes} B`;
}

/** "Expires in 6 days" / "Expires in 3 h" / "Expired". */
export function expiryLabel(iso: string, now = Date.now()): string {
  const ms = new Date(iso).getTime() - now;
  if (!Number.isFinite(ms)) return '';
  if (ms <= 0) return 'Expired';
  const h = ms / 3_600_000;
  if (h < 1) return `Expires in ${Math.max(1, Math.round(ms / 60_000))} min`;
  if (h < 48) return `Expires in ${Math.round(h)} h`;
  return `Expires in ${Math.round(h / 24)} days`;
}

/** Seconds left in a take, given the limit and elapsed time. */
export function remainingSec(maxDurationSec: number, elapsedSec: number): number {
  return Math.max(0, Math.ceil(maxDurationSec - elapsedSec));
}

/** Host-side status of a link for its badge. */
export function linkStatus(v: Pick<RecordLinkView, 'linkEnabled' | 'linkLost' | 'expired' | 'submitted' | 'maxSubmissions'>): { label: string; tone: 'ok' | 'warn' | 'off' } {
  if (!v.linkEnabled) return { label: 'Turned off', tone: 'off' };
  if (v.linkLost) return { label: 'Needs a new link', tone: 'warn' };
  if (v.expired) return { label: 'Expired', tone: 'off' };
  if (v.submitted >= v.maxSubmissions) return { label: 'Full', tone: 'warn' };
  return { label: 'Accepting', tone: 'ok' };
}
