// Recordings panel on the host home [SL-P5-3]: async record links ("record
// solo"). Create a link (title, script, room, limits), copy it, replace or
// turn it off, and work with what guests sent: export each take for the
// editor, download the originals, transcript/Parachute handoffs, delete.
// Link URLs carry their secret: clipboard only, never rendered.
import '../recordlinks.css';
import { describeExport, originalsUrl, pollTakeExport } from '../lib/exportprogress';
import { escapeHtml } from '../lib/html';
import {
  RecordApiError, RecordLinksApi, agoLabel, draftPolicyText, expiryLabel, formatBytes, formatClock, formatLimit, linkStatus,
  type DraftPolicy, type RecordDraftView, type RecordLinkView, type RecordSubmissionView, type RecordTakeView,
} from '../lib/recordlink';
import { formatWhen } from '../lib/rooms';
import { EcosystemActions } from './EcosystemActions';
import { confirmDialog, copyText, promptDialog } from './Rooms';

export interface RecordLinksPanelOptions {
  api?: RecordLinksApi;
  toast: (message: string) => void;
  /** Rooms to attach a new link to. */
  rooms: () => Array<{ id: string; name: string }>;
  /** Poll interval while uploads or exports are in progress. */
  pollMs?: number;
}

const errorText = (err: unknown) => (err instanceof RecordApiError ? err.message : 'Something went wrong. Try again.');

export class RecordLinksPanel {
  readonly el: HTMLElement;
  private readonly api: RecordLinksApi;
  private links: RecordLinkView[] = [];
  private readonly list: HTMLElement;
  private timer: number | null = null;
  private exporting = new Map<string, string>(); // takeId → progress text
  private drafts: RecordDraftView[] = [];
  private policy: DraftPolicy | null = null;
  private readonly abandoned: HTMLElement;

  constructor(private readonly opts: RecordLinksPanelOptions) {
    this.api = opts.api ?? new RecordLinksApi();
    this.el = document.createElement('section');
    this.el.className = 'home-panel recordings-panel';
    this.el.id = 'recordings-panel';
    this.el.setAttribute('aria-labelledby', 'recordings-heading');
    this.el.innerHTML = `<div class="panel-heading"><div><h2 id="recordings-heading" class="list-heading">Record on their own</h2><p class="home-sub">Send someone a link. They record at full quality in their own time, no call needed, and you get the files here.</p></div>
      <button class="btn btn-secondary btn-compact" id="record-link-new">New record link</button></div>
      <section class="record-abandoned" id="record-abandoned" aria-labelledby="record-abandoned-heading" hidden></section>
      <ul class="record-link-list" role="list"><li class="room-empty"><span>Loading…</span></li></ul>`;
    this.list = this.el.querySelector('.record-link-list')!;
    this.abandoned = this.el.querySelector('#record-abandoned')!;
    this.abandoned.addEventListener('click', e => {
      const btn = (e.target as HTMLElement).closest<HTMLButtonElement>('[data-act]');
      if (btn) void this.draftAct(btn.dataset.act!, btn);
    });
    this.el.querySelector('#record-link-new')!.addEventListener('click', () => void this.createDialog());
    this.list.addEventListener('click', e => {
      const btn = (e.target as HTMLElement).closest<HTMLElement>('[data-act]');
      const card = btn?.closest<HTMLElement>('[data-link-id]');
      const link = this.links.find(l => l.id === card?.dataset.linkId);
      if (btn && link) void this.act(btn.dataset.act!, link, btn);
    });
    document.addEventListener('click', e => {
      for (const d of this.list.querySelectorAll<HTMLDetailsElement>('details[open]')) if (!d.contains(e.target as Node)) d.open = false;
    });
  }

  async refresh(): Promise<void> {
    const drafts = this.api.drafts().catch(() => null);
    try {
      this.links = (await this.api.list()).links;
    } catch (err) {
      this.list.innerHTML = `<li class="room-empty"><span>${escapeHtml(err instanceof RecordApiError && err.status === 403 ? 'Recording links are managed on the host Mac’s own dashboard.' : 'Recording links are unavailable right now.')}</span></li>`;
      this.abandoned.hidden = true;
      return;
    }
    const d = await drafts;
    this.drafts = d?.drafts ?? [];
    this.policy = d?.policy ?? null;
    this.render();
    this.renderAbandoned();
    this.schedule();
  }

  private schedule(): void {
    if (this.timer !== null) clearTimeout(this.timer);
    this.timer = null;
    // Abandoned drafts don't keep the panel polling.
    const busy = this.links.some(l => l.drafts - (l.abandonedDrafts ?? 0) > 0 || l.submissions.some(s => s.status === 'submitted' && s.takes.some(t => !t.originalsReady)));
    if (busy) this.timer = window.setTimeout(() => void this.refresh(), this.opts.pollMs ?? 4000);
  }

  /** Drafts guests started and left [SL-P5-4]: sizes, per-draft Delete, Clean up abandoned. */
  private renderAbandoned(): void {
    const list = this.drafts.filter(d => d.abandoned);
    const focused = (document.activeElement as HTMLElement | null)?.dataset?.focusKey;
    this.abandoned.hidden = !list.length;
    if (!list.length) { this.abandoned.replaceChildren(); return; }
    const total = list.reduce((n, d) => n + d.bytes, 0);
    this.abandoned.innerHTML = `<div class="record-abandoned-head"><div><h3 id="record-abandoned-heading">Abandoned drafts</h3>
        <span class="recent-meta">${list.length} draft${list.length === 1 ? '' : 's'} · ${escapeHtml(formatBytes(total))}</span></div>
        <button class="btn btn-secondary btn-compact" data-act="cleanup-drafts" data-focus-key="cleanup">Clean up abandoned</button></div>
      <p class="field-hint">Recordings guests started but never sent. ${escapeHtml(this.policy ? draftPolicyText(this.policy) : '')}</p>
      <ul class="record-draft-list" role="list">${list.map(d => `<li class="record-draft" data-draft-id="${escapeHtml(d.id)}">
        <div class="record-draft-main"><strong>${escapeHtml(d.name)}</strong><span class="recent-meta">${escapeHtml(d.linkTitle)}${d.reason === 'closed' ? ' · link closed' : ''} · ${d.takes} take${d.takes === 1 ? '' : 's'} · <span data-size>${escapeHtml(formatBytes(d.bytes))}</span> · last activity ${escapeHtml(agoLabel(d.lastActivity))}</span></div>
        <button class="btn btn-quiet btn-compact" data-act="delete-draft" data-draft="${escapeHtml(d.id)}" data-focus-key="draft-${escapeHtml(d.id)}" aria-label="Delete ${escapeHtml(d.name)}’s abandoned draft">Delete</button></li>`).join('')}</ul>`;
    if (focused) this.abandoned.querySelector<HTMLElement>(`[data-focus-key="${CSS.escape(focused)}"]`)?.focus();
  }

  private async draftAct(action: string, btn: HTMLButtonElement): Promise<void> {
    const { toast } = this.opts;
    try {
      if (action === 'delete-draft') {
        const d = this.drafts.find(x => x.id === btn.dataset.draft);
        if (!d || !await confirmDialog(`Delete ${d.name}’s draft?`, `Its ${d.takes} take${d.takes === 1 ? '' : 's'} (${formatBytes(d.bytes)}) are removed from this Mac. ${d.name} never sent this recording. This can’t be undone.`, 'Delete draft', true)) return;
        await this.api.deleteDraft(d.id);
        toast(`Draft deleted (${formatBytes(d.bytes)} freed).`);
      } else if (action === 'cleanup-drafts') {
        const list = this.drafts.filter(d => d.abandoned);
        const total = list.reduce((n, d) => n + d.bytes, 0);
        if (!list.length || !await confirmDialog('Clean up abandoned drafts?', `${list.length} draft${list.length === 1 ? '' : 's'} guests never sent (${formatBytes(total)}) are removed from this Mac. Sent recordings and drafts with recent activity stay. This can’t be undone.`, 'Delete drafts', true)) return;
        const res = await this.api.cleanupDrafts();
        toast(`${res.deleted.length} draft${res.deleted.length === 1 ? '' : 's'} deleted (${formatBytes(res.bytes)} freed)${res.skipped.length ? `; ${res.skipped.length} in use again, kept` : ''}.`);
      }
    } catch (err) { toast(errorText(err)); }
    await this.refresh();
  }

  private render(): void {
    if (!this.links.length) {
      this.list.innerHTML = `<li class="room-empty"><strong>No recording links yet.</strong><span>Ask a guest for an intro, a testimonial or a remote segment: they open the link, record at full quality and send it. You get editor-ready files.</span></li>`;
      return;
    }
    this.list.innerHTML = this.links.map(l => this.cardHtml(l)).join('');
  }

  private cardHtml(l: RecordLinkView): string {
    const st = linkStatus(l);
    const meta = [l.roomName ? `<span class="recent-room">${escapeHtml(l.roomName)}</span>` : '', escapeHtml(expiryLabel(l.expiresAt)), `up to ${escapeHtml(formatLimit(l.maxDurationSec))} a take`,
      `${l.submitted}/${l.maxSubmissions} received`, l.usedBytes ? escapeHtml(formatBytes(l.usedBytes)) : ''].filter(Boolean).join(' · ');
    const canCopy = l.linkEnabled && !l.linkLost && !!l.url && !l.expired;
    const subs = l.submissions.filter(s => s.status === 'submitted');
    const active = l.drafts - (l.abandonedDrafts ?? 0);
    const drafts = active > 0 ? `<p class="record-drafts">${active} recording${active === 1 ? '' : 's'} in progress</p>` : '';
    return `<li class="record-link" data-link-id="${escapeHtml(l.id)}">
      <div class="record-link-head"><div class="room-main"><div class="room-title"><strong>${escapeHtml(l.title)}</strong><span class="badge ${st.tone === 'ok' ? 'badge-success' : st.tone === 'warn' ? 'badge-waiting' : ''}">${escapeHtml(st.label)}</span></div>
        <div class="record-link-meta">${meta}</div></div>
        <div class="room-actions">
          ${canCopy ? `<button class="btn btn-secondary" data-act="copy" aria-label="Copy recording link for ${escapeHtml(l.title)}">Copy link</button>` : `<button class="btn btn-secondary" data-act="rotate" aria-label="Create a new recording link for ${escapeHtml(l.title)}">New link</button>`}
          <details class="room-menu"><summary class="icon-button" aria-label="More actions for ${escapeHtml(l.title)}">⋯</summary>
            <div class="room-menu-list" role="group" aria-label="${escapeHtml(l.title)} actions">
              <button data-act="rename">Rename</button>
              <button data-act="rotate">Replace link</button>
              <button data-act="extend">Extend by 7 days</button>
              ${l.linkEnabled ? '<button data-act="revoke">Turn off link</button>' : ''}
              ${!l.submissions.length ? '<button data-act="delete" class="danger">Delete…</button>' : ''}
            </div></details>
        </div></div>
      ${drafts}
      ${subs.length ? `<ul class="record-subs" role="list">${subs.map(s => this.subHtml(s)).join('')}</ul>` : ''}
    </li>`;
  }

  private subHtml(s: RecordSubmissionView): string {
    return `<li class="record-sub" data-sub-id="${escapeHtml(s.id)}">
      <div class="record-sub-head"><strong>${escapeHtml(s.name)}</strong><span class="recent-meta">${escapeHtml(s.submittedAt ? formatWhen(s.submittedAt) : '')} · ${s.takes.length} take${s.takes.length === 1 ? '' : 's'}</span>
        <button class="btn btn-quiet btn-compact record-sub-del" data-act="delete-sub" data-sub="${escapeHtml(s.id)}" aria-label="Delete ${escapeHtml(s.name)}’s recording">Delete</button></div>
      <ul class="record-takes" role="list">${s.takes.map(t => this.takeHtml(s, t)).join('')}</ul></li>`;
  }

  private takeHtml(s: RecordSubmissionView, t: RecordTakeView): string {
    const token = s.hostToken || '';
    const exporting = this.exporting.get(t.id);
    const exported = t.status === 'complete';
    const state = exporting ? escapeHtml(exporting) : !t.originalsReady ? `Uploading · ${t.assembled}/${t.tracks} tracks` : exported ? 'Exported' : t.status === 'failed' ? 'Export needs attention' : 'Ready';
    const files = [
      t.originalsReady ? `<a class="take-link" href="${escapeHtml(originalsUrl(t.id, token))}" download data-file="originals">Originals</a>` : '',
      exported ? `<a class="take-link" href="/api/v1/takes/${encodeURIComponent(t.id)}/download?token=${encodeURIComponent(token)}" download data-file="editor">Editor ZIP</a>` : '',
    ].join('');
    return `<li class="record-take" data-take-id="${escapeHtml(t.id)}" data-sub="${escapeHtml(s.id)}">
      <div class="record-take-main"><span>Take ${t.number} · ${escapeHtml(formatClock(t.durationSec))}</span><span class="record-take-state" data-state="${exported ? 'done' : t.originalsReady ? 'ready' : 'busy'}">${state}</span></div>
      <div class="recent-files">${files}
        ${t.originalsReady && !exported ? `<button class="btn btn-secondary btn-compact" data-act="export" ${exporting ? 'disabled' : ''}>Export for editor</button>` : ''}
        ${exported ? '<button class="btn btn-quiet btn-compact" data-act="handoffs">Transcript & handoffs</button>' : ''}
      </div></li>`;
  }

  private replace(view: RecordLinkView): void {
    this.links = this.links.map(l => (l.id === view.id ? view : l));
    this.render();
  }

  private async act(action: string, link: RecordLinkView, btn: HTMLElement): Promise<void> {
    btn.closest('details')?.removeAttribute('open');
    const { toast } = this.opts;
    const takeEl = btn.closest<HTMLElement>('[data-take-id]');
    const sub = link.submissions.find(s => s.id === (takeEl?.dataset.sub || btn.dataset.sub));
    try {
      switch (action) {
        case 'copy':
          if (link.url && await copyText(link.url)) toast(`Recording link for “${link.title}” copied. Anyone with it can record and send to you until it expires.`);
          else void promptDialog('Copy the recording link', 'Recording link', link.url || '', 'Done');
          return;
        case 'rename': {
          const title = await promptDialog('Rename recording link', 'Title guests see', link.title, 'Save');
          if (title && title !== link.title) this.replace(await this.api.update(link.id, { title }));
          return;
        }
        case 'rotate': {
          if (link.linkEnabled && !link.linkLost && !await confirmDialog('Replace the recording link?', `The current link for “${link.title}” stops working immediately. Recordings already sent stay.`, 'Replace link')) return;
          const next = await this.api.rotate(link.id);
          this.replace(next);
          if (next.url && await copyText(next.url)) toast('New recording link copied. The old link no longer works.');
          return;
        }
        case 'extend': {
          const base = Math.max(Date.now(), new Date(link.expiresAt).getTime());
          this.replace(await this.api.update(link.id, { expiresAt: new Date(base + 7 * 86_400_000).toISOString().replace(/\.\d+Z$/, 'Z') }));
          toast('Link extended by 7 days.');
          return;
        }
        case 'revoke':
          if (!await confirmDialog('Turn off the recording link?', `Nobody can record with the link for “${link.title}” until you create a new one. Uploads of takes already recorded still finish.`, 'Turn off link', true)) return;
          this.replace(await this.api.revoke(link.id));
          toast('Recording link turned off.');
          return;
        case 'delete':
          if (!await confirmDialog('Delete this recording link?', `“${link.title}” is removed for good.`, 'Delete link', true)) return;
          await this.api.remove(link.id);
          this.links = this.links.filter(l => l.id !== link.id);
          this.render();
          return;
        case 'delete-sub':
          if (!sub || !await confirmDialog(`Delete ${sub.name}’s recording?`, 'Every take and file of this recording is removed from this Mac. This can’t be undone.', 'Delete recording', true)) return;
          await this.api.removeSubmission(link.id, sub.id);
          toast('Recording deleted.');
          await this.refresh();
          return;
        case 'export':
          if (sub && takeEl?.dataset.takeId) await this.exportTake(sub, takeEl.dataset.takeId);
          return;
        case 'handoffs':
          if (sub && takeEl?.dataset.takeId) this.handoffs(sub, takeEl.dataset.takeId);
          return;
      }
    } catch (err) { toast(errorText(err)); }
  }

  private async exportTake(sub: RecordSubmissionView, takeId: string): Promise<void> {
    const token = sub.hostToken || '';
    this.exporting.set(takeId, 'Preparing editor files…');
    this.render();
    const stop = pollTakeExport(takeId, token, v => {
      if (v.postproc?.state === 'running') { this.exporting.set(takeId, describeExport(v.postproc)); this.updateTakeState(takeId); }
    });
    try {
      const res = await fetch(`/api/v1/takes/${encodeURIComponent(takeId)}/postprocess`, { method: 'POST', headers: { Authorization: `Bearer ${token}` } });
      if (!res.ok) throw new RecordApiError(res.status, (await res.text()).trim() || `Export failed (HTTP ${res.status})`);
      this.opts.toast(`${sub.name}’s take is ready for your editor.`);
    } catch (err) {
      this.opts.toast(`Export failed: ${errorText(err)}`);
    } finally {
      stop();
      this.exporting.delete(takeId);
      await this.refresh();
    }
  }

  private updateTakeState(takeId: string): void {
    const node = this.list.querySelector<HTMLElement>(`[data-take-id="${CSS.escape(takeId)}"] .record-take-state`);
    if (node) node.textContent = this.exporting.get(takeId) || '';
  }

  /** Transcript, Parachute and clip-editor handoffs for one take, as after a call. */
  private handoffs(sub: RecordSubmissionView, takeId: string): void {
    const dialog = document.createElement('dialog');
    dialog.className = 'room-dialog record-handoff-dialog';
    dialog.setAttribute('aria-labelledby', 'record-handoff-title');
    dialog.innerHTML = `<h2 id="record-handoff-title">${escapeHtml(sub.name)} · Take ${escapeHtml(sub.takes.find(t => t.id === takeId)?.number ?? '')}</h2>
      <section class="record-handoff-actions"></section><form method="dialog" class="room-dialog-actions"><button class="btn btn-primary" value="ok">Done</button></form>`;
    const actions = new EcosystemActions(dialog.querySelector('.record-handoff-actions') as HTMLElement);
    dialog.addEventListener('close', () => { actions.stop(); dialog.remove(); }, { once: true });
    document.body.appendChild(dialog);
    dialog.showModal();
    actions.show(takeId, sub.id, sub.hostToken || '');
  }

  /** New, then Ask someone to record on their own. */
  create(): Promise<void> { return this.createDialog(); }

  private createDialog(): Promise<void> {
    return new Promise(resolve => {
      const rooms = this.opts.rooms();
      const dialog = document.createElement('dialog');
      dialog.className = 'room-dialog record-link-dialog';
      dialog.setAttribute('aria-labelledby', 'record-link-dialog-title');
      dialog.innerHTML = `<form method="dialog" novalidate><h2 id="record-link-dialog-title">New recording link</h2>
        <p>Guests open it, check their camera and mic, read your script and record on their own. Nothing is live.</p>
        <div class="record-form">
          <label class="field-label" for="rl-title">Title guests see</label><input id="rl-title" maxlength="120" required placeholder="e.g. Intro for episode 12">
          <label class="field-label" for="rl-prompt">Script or prompt <span class="field-hint">(optional; shown while they record)</span></label><textarea id="rl-prompt" rows="5" maxlength="65536" placeholder="# Intro&#10;Tell us who you are and what you’re working on."></textarea>
          <div class="record-form-row">
            <div><label class="field-label" for="rl-limit">Each take up to</label><select id="rl-limit"><option value="300">5 min</option><option value="900">15 min</option><option value="1800" selected>30 min</option><option value="3600">1 hour</option><option value="7200">2 hours</option></select></div>
            <div><label class="field-label" for="rl-expiry">Link works for</label><select id="rl-expiry"><option value="24">1 day</option><option value="168" selected>7 days</option><option value="720">30 days</option><option value="2160">90 days</option></select></div>
          </div>
          <div class="record-form-row">
            <div><label class="field-label" for="rl-max">Recordings it accepts</label><input id="rl-max" type="number" min="1" max="500" value="10"></div>
            <div><label class="field-label" for="rl-gb">Storage limit (GB)</label><input id="rl-gb" type="number" min="1" max="2048" value="20"></div>
          </div>
          ${rooms.length ? `<label class="field-label" for="rl-room">Room <span class="field-hint">(optional)</span></label><select id="rl-room"><option value="">None</option>${rooms.map(r => `<option value="${escapeHtml(r.id)}">${escapeHtml(r.name)}</option>`).join('')}</select>` : ''}
          <p class="record-form-error" role="alert" hidden></p>
        </div>
        <div class="room-dialog-actions"><button value="cancel" class="btn btn-quiet" formnovalidate>Cancel</button><button value="ok" class="btn btn-primary" id="rl-create">Create and copy link</button></div></form>`;
      const $ = <T extends HTMLElement>(sel: string) => dialog.querySelector<T>(sel)!;
      const error = $('.record-form-error');
      $('#rl-create').addEventListener('click', e => {
        e.preventDefault();
        const title = $<HTMLInputElement>('#rl-title').value.trim();
        if (!title) { error.hidden = false; error.textContent = 'Give the link a title.'; $('#rl-title').focus(); return; }
        const body = {
          title,
          prompt: $<HTMLTextAreaElement>('#rl-prompt').value,
          maxDurationSec: Number($<HTMLSelectElement>('#rl-limit').value),
          expiresInHours: Number($<HTMLSelectElement>('#rl-expiry').value),
          maxSubmissions: Math.max(1, Math.min(500, Math.round(Number($<HTMLInputElement>('#rl-max').value) || 10))),
          maxTotalMB: Math.max(1, Math.min(2048, Number($<HTMLInputElement>('#rl-gb').value) || 20)) * 1024,
          ...(dialog.querySelector<HTMLSelectElement>('#rl-room')?.value ? { roomId: $<HTMLSelectElement>('#rl-room').value } : {}),
        };
        const btn = $<HTMLButtonElement>('#rl-create');
        btn.disabled = true;
        void this.api.create(body).then(async view => {
          this.links = [view, ...this.links];
          this.render();
          dialog.close('ok');
          if (view.url && await copyText(view.url)) this.opts.toast(`Recording link for “${view.title}” copied. Send it to your guest.`);
          else this.opts.toast('Recording link created. Copy it from the Recordings list.');
        }, err => { error.hidden = false; error.textContent = errorText(err); btn.disabled = false; });
      });
      dialog.addEventListener('close', () => { dialog.remove(); resolve(); }, { once: true });
      document.body.appendChild(dialog);
      dialog.showModal();
      $('#rl-title').focus();
    });
  }
}
