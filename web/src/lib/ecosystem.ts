// Post-take ecosystem handoffs (ROADMAP P4, SL-P4-1, SL-P4-3): types and pure
// helpers for GET /api/v1/takes/{id}/integrations. Mirrors
// host/internal/integrations.View.

export type JobState = 'idle' | 'running' | 'done' | 'error' | 'cancelled';

export interface JobStatus {
  state: JobState;
  message?: string;
  updatedAt?: string;
  detail?: Record<string, unknown>;
}

export interface Capability { available: boolean; reason?: string; code?: string; target?: string }

export type EngineId = 'builtin' | 'minutes';

export interface EngineInfo { id: EngineId; label: string; available: boolean; code?: string; reason?: string; detail?: string }

export interface TranscriptionView { engine: EngineId; engines: EngineInfo[]; language?: string }

export interface IntegrationsView {
  takeId: string;
  packageReady: boolean;
  hasTranscript: boolean;
  hasHandoff: boolean;
  autoTranscribe: boolean;
  canResume?: boolean;
  capabilities: { transcript?: Capability; minutes: Capability; parachute: Capability; programMix: Capability };
  transcription?: TranscriptionView;
  transcript: JobStatus;
  parachute: JobStatus;
  clipEditor: JobStatus;
}

export type ActionKind = 'transcript' | 'parachute' | 'clipEditor';

export interface ActionModel {
  kind: ActionKind;
  /** Stable row title. */
  title: string;
  /** Button label (changes to "again" or "retry" wording once a result exists). */
  label: string;
  enabled: boolean;
  /** Short state word for the badge. */
  badge: string;
  tone: 'idle' | 'running' | 'success' | 'danger' | 'unavailable';
  /** One-line explanation under the action. */
  note: string;
}

const LABELS: Record<ActionKind, { title: string; run: string; again: string; idleNote: string }> = {
  transcript: { title: 'Transcript', run: 'Transcribe', again: 'Transcribe again', idleNote: 'One isolated track per participant, so every line is named exactly.' },
  parachute: { title: 'Parachute session note', run: 'Send to Parachute', again: 'Update Parachute note', idleNote: 'A session note with participants, files and the transcript when ready.' },
  clipEditor: { title: 'Clip-editor handoff', run: 'Prepare clip-editor handoff', again: 'Refresh clip-editor handoff', idleNote: 'handoff/clip-editor.json for the agentic video editor.' },
};

export function isValidView(value: unknown): value is IntegrationsView {
  const v = value as IntegrationsView;
  return !!v && typeof v === 'object' && typeof v.takeId === 'string' && !!v.capabilities && !!v.transcript && !!v.parachute && !!v.clipEditor;
}

/** The selected engine's capability (older hosts only report Minutes). */
export function transcriptCapability(view: IntegrationsView): Capability {
  return view.capabilities.transcript ?? view.capabilities.minutes;
}

function capabilityFor(view: IntegrationsView, kind: ActionKind): Capability {
  if (kind === 'transcript') return transcriptCapability(view);
  if (kind === 'parachute') return view.capabilities.parachute;
  return { available: true };
}

function jobFor(view: IntegrationsView, kind: ActionKind): JobStatus {
  return kind === 'transcript' ? view.transcript : kind === 'parachute' ? view.parachute : view.clipEditor;
}

function hasResult(view: IntegrationsView, kind: ActionKind): boolean {
  if (kind === 'transcript') return view.hasTranscript;
  if (kind === 'clipEditor') return view.hasHandoff;
  return view.parachute.state === 'done';
}

/** Describes one action row: title, button label, availability, badge and note. */
export function actionModel(view: IntegrationsView, kind: ActionKind): ActionModel {
  return { ...stateModel(view, kind), title: LABELS[kind].title };
}

function stateModel(view: IntegrationsView, kind: ActionKind): Omit<ActionModel, 'title'> {
  const cap = capabilityFor(view, kind);
  const job = jobFor(view, kind);
  const text = LABELS[kind];
  const resume = kind === 'transcript' && !!view.canResume;
  const label = resume ? 'Retry remaining tracks' : hasResult(view, kind) ? text.again : text.run;
  if (!view.packageReady) {
    return { kind, label: text.run, enabled: false, badge: 'Waiting', tone: 'unavailable', note: 'Prepare the editor files first.' };
  }
  // A running job keeps reporting even if the engine went away meanwhile.
  if (job.state === 'running') {
    return { kind, label, enabled: false, badge: 'Running', tone: 'running', note: job.message || 'Working…' };
  }
  if (!cap.available) {
    const note = cap.reason || 'Not configured on this host.';
    // A failure the engine explains (e.g. permission) stays visible as an error.
    if (job.state === 'error') return { kind, label, enabled: false, badge: 'Error', tone: 'danger', note: job.message ? `${job.message}` : note };
    return { kind, label: text.run, enabled: false, badge: 'Unavailable', tone: 'unavailable', note };
  }
  switch (job.state) {
    case 'done':
      return { kind, label, enabled: true, badge: 'Done', tone: 'success', note: job.message || 'Done.' };
    case 'error':
      return { kind, label, enabled: true, badge: 'Error', tone: 'danger', note: job.message || 'Failed. Try again.' };
    case 'cancelled':
      return { kind, label, enabled: true, badge: 'Cancelled', tone: 'idle', note: job.message || 'Cancelled. Nothing was written to the package.' };
    default:
      if (hasResult(view, kind)) return { kind, label, enabled: true, badge: 'Ready', tone: 'success', note: kind === 'transcript' ? 'transcript/ is in the package (JSON, Markdown, SRT, VTT).' : text.idleNote };
      if (kind === 'parachute' && cap.target) return { kind, label, enabled: true, badge: 'Ready', tone: 'idle', note: `Vault: ${cap.target}` };
      return { kind, label, enabled: true, badge: 'Ready', tone: 'idle', note: text.idleNote };
  }
}

export type TrackState = 'queued' | 'running' | 'done' | 'empty' | 'error' | 'cancelled';

export interface TrackProgress {
  sourceId: string;
  speaker: string;
  state: TrackState;
  progress: number;
  durationSec?: number;
  segments?: number;
  reused?: boolean;
  error?: string;
}

export interface TrackRow {
  sourceId: string;
  speaker: string;
  state: TrackState;
  /** 0–100 for the progress bar. */
  percent: number;
  /** Short status text, e.g. "42%", "12 lines", "No speech". */
  status: string;
  tone: 'idle' | 'running' | 'success' | 'danger';
  error?: string;
}

const TRACK_STATES: TrackState[] = ['queued', 'running', 'done', 'empty', 'error', 'cancelled'];

/** Per-track rows from the transcript job's detail (validated; unknown data is dropped). */
export function trackRows(job: JobStatus): TrackRow[] {
  const raw = job.detail?.tracks;
  if (!Array.isArray(raw)) return [];
  const rows: TrackRow[] = [];
  for (const t of raw as Partial<TrackProgress>[]) {
    if (!t || typeof t.speaker !== 'string' || typeof t.sourceId !== 'string' || !TRACK_STATES.includes(t.state as TrackState)) continue;
    const state = t.state as TrackState;
    const progress = typeof t.progress === 'number' && Number.isFinite(t.progress) ? Math.min(1, Math.max(0, t.progress)) : 0;
    const segments = typeof t.segments === 'number' ? t.segments : 0;
    let status: string; let tone: TrackRow['tone'];
    switch (state) {
      case 'queued': status = 'Waiting'; tone = 'idle'; break;
      case 'running': status = `${Math.floor(progress * 100)}%`; tone = 'running'; break;
      case 'done': status = `${segments} ${segments === 1 ? 'line' : 'lines'}${t.reused ? ' (kept)' : ''}`; tone = 'success'; break;
      case 'empty': status = 'No speech'; tone = 'success'; break;
      case 'error': status = 'Failed'; tone = 'danger'; break;
      default: status = 'Cancelled'; tone = 'idle';
    }
    const percent = state === 'done' || state === 'empty' ? 100 : state === 'running' ? Math.floor(progress * 100) : 0;
    rows.push({ sourceId: t.sourceId, speaker: t.speaker, state, percent, status, tone, error: typeof t.error === 'string' && t.error ? t.error : undefined });
  }
  return rows;
}

export interface EngineOption { id: EngineId; label: string; selected: boolean; disabled: boolean }

/** Options for the engine picker: the built-in engine is always selectable, Minutes only when detected. */
export function engineOptions(view: IntegrationsView): EngineOption[] {
  const t = view.transcription;
  if (!t) return [];
  return t.engines.map(e => ({
    id: e.id,
    label: e.id === 'minutes' && !e.available ? `${e.label} (not detected)` : e.label,
    selected: e.id === t.engine,
    disabled: e.id !== 'builtin' && !e.available && e.id !== t.engine,
  }));
}

/** One line under the picker describing the selected engine's state. */
export function engineStatus(view: IntegrationsView): { text: string; tone: 'success' | 'danger' | 'unavailable' } {
  const t = view.transcription;
  if (!t) return { text: '', tone: 'unavailable' };
  const e = t.engines.find(x => x.id === t.engine);
  if (!e) return { text: '', tone: 'unavailable' };
  if (e.available) {
    const where = e.id === 'builtin' ? 'On-device speech in the p2p.video app; audio never leaves this Mac.' : 'minutes-cli transcribe-file on this Mac.';
    return { text: `Ready. ${where}`, tone: 'success' };
  }
  return { text: e.reason || 'Unavailable.', tone: e.code === 'not_authorized' ? 'danger' : 'unavailable' };
}

/** True while any job is running, so the UI keeps polling. */
export function anyRunning(view: IntegrationsView): boolean {
  return [view.transcript, view.parachute, view.clipEditor].some(job => job.state === 'running');
}

/** The transcript can be cancelled while it runs. */
export function canCancelTranscript(view: IntegrationsView | null): boolean {
  return !!view && view.transcript.state === 'running';
}

export const ACTION_PATHS: Record<ActionKind, string> = {
  transcript: 'transcript',
  parachute: 'parachute',
  clipEditor: 'handoff/clip-editor',
};

/** Host endpoint for an action or the status view. */
export function integrationUrl(takeId: string, kind?: ActionKind): string {
  const base = `/api/v1/takes/${encodeURIComponent(takeId)}`;
  return kind ? `${base}/${ACTION_PATHS[kind]}` : `${base}/integrations`;
}

/** Host endpoints for transcript cancel and engine choice. */
export function transcriptUrl(takeId: string, sub: 'cancel' | 'engine'): string {
  return `/api/v1/takes/${encodeURIComponent(takeId)}/transcript/${sub}`;
}

/** Poll interval: quick while work is running; slow while the selected
 * transcription engine is unavailable (so opening the app is noticed);
 * otherwise stop. */
export function nextPollMs(view: IntegrationsView | null): number | null {
  if (!view) return null;
  if (anyRunning(view)) return 1500;
  if (view.packageReady && view.transcription && !transcriptCapability(view).available) return 5000;
  return null;
}
