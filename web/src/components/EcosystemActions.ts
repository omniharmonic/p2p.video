import '../ecosystem.css';
import { escapeHtml as h } from '../lib/html';
import {
  ActionKind, EngineId, IntegrationsView, actionModel, canCancelTranscript, engineOptions, engineStatus, integrationUrl,
  isValidView, nextPollMs, trackRows, transcriptCapability, transcriptUrl,
} from '../lib/ecosystem';

/**
 * Post-take actions in the export modal (SL-P4-1, SL-P4-3): transcribe with
 * the built-in on-device engine or Minutes (with per-track progress, cancel
 * and retry), send a session note to Parachute, prepare the clip-editor
 * handoff, and the per-session "auto-transcribe after export" switch.
 * Host-only endpoints.
 */
export class EcosystemActions {
  private root: HTMLElement;
  private takeId = '';
  private sessionId = '';
  private token = '';
  private view: IntegrationsView | null = null;
  private message = '';
  private timer: number | undefined;
  private generation = 0;

  constructor(container: HTMLElement) {
    this.root = container;
    this.root.classList.add('ecosystem-actions');
    this.root.setAttribute('aria-label', 'Transcript and handoffs');
  }

  public show(takeId: string, sessionId: string, token: string): void {
    this.stop();
    this.generation++;
    this.takeId = takeId; this.sessionId = sessionId; this.token = token;
    this.view = null; this.message = '';
    this.render();
    void this.refresh();
  }

  public stop(): void {
    if (this.timer !== undefined) window.clearTimeout(this.timer);
    this.timer = undefined;
  }

  private headers(): HeadersInit { return { Authorization: `Bearer ${this.token}` }; }

  private async refresh(): Promise<void> {
    const generation = this.generation;
    try {
      const res = await fetch(integrationUrl(this.takeId), { headers: this.headers(), cache: 'no-store', signal: AbortSignal.timeout(20_000) });
      if (!res.ok) throw new Error(res.status === 503 ? 'Transcript and handoffs are unavailable on this host.' : `Status unavailable (HTTP ${res.status})`);
      const data: unknown = await res.json();
      if (!isValidView(data)) throw new Error('Unexpected status from the host');
      if (generation !== this.generation) return;
      this.view = data;
    } catch (error) {
      if (generation !== this.generation) return;
      this.message = error instanceof Error ? error.message : String(error);
    }
    this.render();
    this.schedule();
  }

  private schedule(): void {
    this.stop();
    const wait = nextPollMs(this.view);
    if (wait !== null) this.timer = window.setTimeout(() => { void this.refresh(); }, wait);
  }

  /** POST/PUT an action and adopt the returned view. */
  private async send(url: string, init: RequestInit, timeoutMs = 120_000): Promise<boolean> {
    this.message = '';
    try {
      const res = await fetch(url, { ...init, headers: { ...this.headers(), ...(init.headers || {}) }, signal: AbortSignal.timeout(timeoutMs) });
      const text = await res.text();
      if (!res.ok) throw new Error(text.trim() || `HTTP ${res.status}`);
      const data: unknown = JSON.parse(text);
      if (isValidView(data)) this.view = data;
      return true;
    } catch (error) {
      this.message = error instanceof Error ? error.message : String(error);
      return false;
    } finally {
      this.render();
      this.schedule();
    }
  }

  private run(kind: ActionKind): Promise<boolean> {
    return this.send(integrationUrl(this.takeId, kind), { method: 'POST' });
  }

  private async cancelTranscript(): Promise<void> {
    if (await this.send(transcriptUrl(this.takeId, 'cancel'), { method: 'POST' }, 20_000)) this.message = 'Cancelling… finished tracks are kept for a retry.';
    this.render();
  }

  private async setEngine(engine: EngineId): Promise<void> {
    await this.send(transcriptUrl(this.takeId, 'engine'), { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ engine }) }, 20_000);
  }

  private async setAutoTranscribe(enabled: boolean): Promise<void> {
    try {
      const res = await fetch(`/api/v1/sessions/${encodeURIComponent(this.sessionId)}/settings`, {
        method: 'PATCH', headers: { ...this.headers(), 'Content-Type': 'application/json' }, body: JSON.stringify({ auto_transcribe: enabled }),
      });
      if (!res.ok) throw new Error(`Setting not saved (HTTP ${res.status})`);
      if (this.view) this.view.autoTranscribe = enabled;
      this.message = enabled ? 'New exports in this session will be transcribed automatically.' : 'Auto-transcribe is off for this session.';
    } catch (error) {
      this.message = error instanceof Error ? error.message : String(error);
    }
    this.render();
  }

  private engineHtml(view: IntegrationsView): string {
    const options = engineOptions(view);
    if (!options.length) return '';
    const status = engineStatus(view);
    const running = view.transcript.state === 'running';
    const tone = status.tone === 'success' ? 'success' : status.tone === 'danger' ? 'danger' : 'unavailable';
    // When the engine is unavailable the row note above already says why.
    return `<div class="ecosystem-engine">
        <label class="ecosystem-engine-label" for="ecosystem-engine-select">Transcription engine</label>
        <select id="ecosystem-engine-select" data-engine ${running ? 'disabled' : ''}>${options.map(o =>
          `<option value="${h(o.id)}" ${o.selected ? 'selected' : ''} ${o.disabled ? 'disabled' : ''}>${h(o.label)}</option>`).join('')}</select>
        ${tone === 'success' || view.transcript.state === 'running' ? `<span class="ecosystem-engine-status" data-engine-status data-tone="${tone}">${h(status.text)}</span>` : ''}
      </div>`;
  }

  private tracksHtml(view: IntegrationsView): string {
    const rows = trackRows(view.transcript);
    if (!rows.length || (view.transcript.state === 'done' && rows.every(r => r.state === 'done' || r.state === 'empty'))) return '';
    return `<ul class="ecosystem-tracks" data-tracks aria-label="Transcript progress per participant">${rows.map(r => `
        <li class="ecosystem-track" data-track="${h(r.sourceId)}" data-track-state="${h(r.state)}" data-tone="${r.tone}">
          <span class="ecosystem-track-name">${h(r.speaker)}</span>
          <progress max="100" value="${r.percent}" aria-label="${h(r.speaker)} transcript progress"></progress>
          <span class="ecosystem-track-status">${h(r.status)}</span>
          ${r.error ? `<span class="ecosystem-track-error">${h(r.error)}</span>` : ''}
        </li>`).join('')}</ul>`;
  }

  private render(): void {
    const view = this.view;
    const rows = view ? (['transcript', 'parachute', 'clipEditor'] as ActionKind[]).map(kind => {
      const m = actionModel(view, kind);
      const badgeClass = m.tone === 'success' ? 'badge badge-success' : m.tone === 'danger' ? 'badge badge-danger' : 'badge';
      const cancel = kind === 'transcript' && canCancelTranscript(view) ? '<button class="btn btn-secondary" data-cancel-transcript>Cancel</button>' : '';
      const extra = kind === 'transcript' ? this.engineHtml(view) + this.tracksHtml(view) : '';
      return `<div class="export-item ecosystem-item" data-action="${kind}" data-state="${h(m.tone)}">
        <div><strong>${h(m.title)}</strong><span class="ecosystem-note" ${m.tone === 'running' ? 'aria-live="polite"' : ''}>${h(m.note)}</span></div>
        <span class="${badgeClass}" data-badge>${h(m.badge)}</span>
        <span class="ecosystem-buttons">${cancel}<button class="btn btn-secondary" data-run="${kind}" ${m.enabled ? '' : 'disabled'}>${h(m.label)}</button></span>
        ${extra ? `<div class="ecosystem-extra">${extra}</div>` : ''}
      </div>`;
    }).join('') : '<p class="ecosystem-note">Checking transcription, Parachute and the export package…</p>';
    const auto = view && transcriptCapability(view).available
      ? `<label class="ecosystem-toggle"><input type="checkbox" data-auto ${view.autoTranscribe ? 'checked' : ''}> Auto-transcribe after export in this session</label>`
      : '';
    this.root.innerHTML = `<div class="ecosystem-heading"><span class="field-label">Transcript and handoffs</span></div>
      <div class="artifact-list">${rows}</div>${auto}
      <p class="ecosystem-message" role="status">${h(this.message)}</p>`;
    this.root.querySelectorAll<HTMLButtonElement>('[data-run]').forEach(button => {
      button.onclick = () => { button.disabled = true; void this.run(button.dataset.run as ActionKind); };
    });
    const cancel = this.root.querySelector<HTMLButtonElement>('[data-cancel-transcript]');
    if (cancel) cancel.onclick = () => { cancel.disabled = true; void this.cancelTranscript(); };
    const engine = this.root.querySelector<HTMLSelectElement>('[data-engine]');
    if (engine) engine.onchange = () => { engine.disabled = true; void this.setEngine(engine.value as EngineId); };
    const toggle = this.root.querySelector<HTMLInputElement>('[data-auto]');
    if (toggle) toggle.onchange = () => { void this.setAutoTranscribe(toggle.checked); };
  }
}
