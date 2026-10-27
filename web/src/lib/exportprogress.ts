// Export progress and quick delivery [SL-P0-9]: the host's take status
// carries the running export's stage/percent (`postproc`) and whether the
// per-participant originals can already be downloaded (`originalsReady`).

export interface ExportStatus {
  takeId: string;
  state: 'running' | 'complete' | 'failed';
  stage: string;
  percent: number;
  elapsedSec: number;
  error?: string;
  videoCodec?: string;
  videoWorkers?: number;
}

export interface TakeExportView {
  originalsReady: boolean;
  postproc: ExportStatus | null;
}

const STAGE_LABELS: Record<string, string> = {
  validate: 'Checking uploads',
  probe: 'Inspecting media',
  audio: 'Preparing microphones',
  video: 'Conforming video',
  verify: 'Verifying files',
  timelines: 'Writing editor timelines',
  publish: 'Publishing package',
  complete: 'Editor files ready',
};

/** Reads the export fields from a GET /api/v1/takes/{id} body. */
export function parseTakeExport(body: unknown): TakeExportView {
  const data = (body ?? {}) as { originalsReady?: unknown; postproc?: unknown };
  const p = data.postproc as Partial<ExportStatus> | undefined;
  const valid = !!p && typeof p === 'object' && typeof p.state === 'string' && ['running', 'complete', 'failed'].includes(p.state)
    && typeof p.percent === 'number' && Number.isFinite(p.percent);
  return {
    originalsReady: data.originalsReady === true,
    postproc: valid ? {
      takeId: String(p!.takeId ?? ''), state: p!.state as ExportStatus['state'], stage: String(p!.stage ?? ''),
      percent: Math.max(0, Math.min(100, p!.percent as number)), elapsedSec: Number(p!.elapsedSec) || 0,
      ...(p!.error ? { error: String(p!.error) } : {}),
      ...(p!.videoCodec ? { videoCodec: String(p!.videoCodec) } : {}),
      ...(p!.videoWorkers ? { videoWorkers: Number(p!.videoWorkers) } : {}),
    } : null,
  };
}

/** Human summary such as "Conforming video · 42% · 1:05 elapsed". */
export function describeExport(status: ExportStatus): string {
  const elapsed = formatElapsed(status.elapsedSec);
  if (status.state === 'failed') return `Export failed${status.error ? `: ${status.error}` : ''}`;
  if (status.state === 'complete') return `Editor files ready · ${elapsed}`;
  const stage = STAGE_LABELS[status.stage] ?? 'Preparing editor files';
  return `${stage} · ${Math.floor(status.percent)}% · ${elapsed} elapsed`;
}

export function formatElapsed(seconds: number): string {
  const s = Math.max(0, Math.round(seconds));
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), r = s % 60;
  return h ? `${h}:${String(m).padStart(2, '0')}:${String(r).padStart(2, '0')}` : `${m}:${String(r).padStart(2, '0')}`;
}

export function originalsUrl(takeId: string, token: string): string {
  return `/api/v1/takes/${encodeURIComponent(takeId)}/originals?token=${encodeURIComponent(token)}`;
}

/**
 * Polls the take status until stopped. The export POST blocks until the
 * package is published, so callers poll alongside it and stop when it returns.
 */
export function pollTakeExport(takeId: string, token: string, onUpdate: (view: TakeExportView) => void,
  { intervalMs = 1000, fetchImpl = fetch }: { intervalMs?: number; fetchImpl?: typeof fetch } = {}): () => void {
  let stopped = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const tick = async () => {
    if (stopped) return;
    try {
      const res = await fetchImpl(`/api/v1/takes/${encodeURIComponent(takeId)}`, { headers: { Authorization: `Bearer ${token}` }, cache: 'no-store' });
      if (res.ok && !stopped) onUpdate(parseTakeExport(await res.json()));
    } catch { /* transient: the next tick retries */ }
    if (!stopped) timer = setTimeout(() => { void tick(); }, intervalMs);
  };
  void tick();
  return () => { stopped = true; if (timer) clearTimeout(timer); };
}
