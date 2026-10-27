import { OPFSStorage } from '../lib/opfs';
import { recoverTakeFromOPFS } from '../lib/upload';
import { escapeHtml as h } from '../lib/html';
import { describeExport, originalsUrl, parseTakeExport, pollTakeExport, type TakeExportView } from '../lib/exportprogress';

export interface TrackTransferItem {
  trackId: string; sourceLabel: string; kind: 'video' | 'audio'; participantName: string;
  localBytes: number; serverBytes: number;
  status: 'verified' | 'assembled' | 'processed' | 'partial' | 'uploading' | 'paused' | 'missing'; lastSeq: number;
}
export interface TakeTransferDetails {
  sessionTitle: string; takeId: string; takeNumber: number; durationSec: number;
  participantsCount: number; tracks: TrackTransferItem[];
}

export class TransfersRecoveryModal {
  private modal: HTMLElement;
  private opfs = OPFSStorage.getInstance();
  private currentDetails: TakeTransferDetails | null = null;
  private isHost = false;
  private authoritative = false;
  private takeStopped = false;
  private missingParticipants = 0;
  private takeComplete = false;
  private message = '';
  private refreshing = 0;
  private exportView: TakeExportView = { originalsReady: false, postproc: null };
  private stopExportPoll: (() => void) | null = null;

  constructor(private fallbackToken: string, private onExportRequested?: (takeId: string) => void) {
    this.modal = document.createElement('div');
    this.modal.id = 'transfers-recovery-modal'; this.modal.setAttribute('role','dialog'); this.modal.setAttribute('aria-modal','true'); this.modal.setAttribute('aria-label','Uploads'); this.modal.className = 'modal-backdrop'; this.modal.style.display = 'none';
    document.body.appendChild(this.modal);
    document.addEventListener('keydown',event=>{if(event.key==='Escape'&&this.isVisible())this.hide();});
  }
  public setToken(token: string, isHost = true): void {
    this.refreshing++; this.fallbackToken = token; this.isHost = isHost; this.authoritative = false;
    if (this.isVisible()) void this.refreshFromHost();
  }
  public show(details: TakeTransferDetails): void {
    if (this.currentDetails?.takeId !== details.takeId) { this.stopWatchingExport(); this.exportView = { originalsReady: false, postproc: null }; }
    this.currentDetails = details; this.authoritative = false; this.message = '';
    this.modal.style.display = 'flex'; this.render(); void this.refreshFromHost();
  }
  public hide(): void { this.modal.style.display = 'none'; this.stopWatchingExport(); }
  /** Shows live export progress (stage/percent) until the modal closes. */
  public watchExport(takeId: string): void {
    this.stopWatchingExport();
    if (!this.fallbackToken || !this.isHost) return;
    this.stopExportPoll = pollTakeExport(takeId, this.fallbackToken, view => {
      if (this.currentDetails?.takeId !== takeId) return;
      this.exportView = view; this.renderExport();
    });
  }
  private stopWatchingExport(): void { this.stopExportPoll?.(); this.stopExportPoll = null; }
  private exportRunning(): boolean { return this.exportView.postproc?.state === 'running'; }
  private exportMarkup(): string {
    const d = this.currentDetails; if (!d) return '';
    const p = this.exportView.postproc;
    const bar = p ? `<div class="export-progress-row"><progress id="export-progress-bar" max="100" value="${p.state === 'failed' ? 0 : p.percent}" aria-label="Files for editing progress"></progress><span id="export-progress-text" class="export-progress-text ${p.state}" role="status">${h(describeExport(p))}</span></div>` : '';
    const originals = this.isHost && this.exportView.originalsReady
      ? `<a id="download-originals-btn" class="btn btn-secondary" href="${h(originalsUrl(d.takeId, this.fallbackToken))}" download referrerpolicy="no-referrer">Download raw files</a>`
      : `<button id="download-originals-btn" class="btn btn-secondary" disabled>Download raw files</button>`;
    return `${bar}<div class="export-quick"><p>Raw files: each person’s recording exactly as it was uploaded, with a file that says how to line them up. Ready as soon as everyone’s recording is saved.</p>${originals}</div>`;
  }
  private renderExport(): void {
    const el = this.modal.querySelector<HTMLElement>('#export-progress');
    const p = this.exportView.postproc;
    // Update the bar in place each poll; rebuild only when the structure
    // changes, so a click on the originals link is never swallowed.
    const key = `${this.exportView.originalsReady}|${p?.state ?? ''}`;
    const bar = el?.querySelector<HTMLProgressElement>('#export-progress-bar'), text = el?.querySelector<HTMLElement>('#export-progress-text');
    if (el && el.dataset.key === key && p && bar && text) { bar.value = p.state === 'failed' ? 0 : p.percent; text.textContent = describeExport(p); }
    else if (el) { el.innerHTML = this.exportMarkup(); el.dataset.key = key; }
    const prepare = this.modal.querySelector<HTMLButtonElement>('#prepare-export-btn');
    if (prepare) prepare.disabled = !this.canExport();
  }
  public isVisible(): boolean { return this.modal.style.display === 'flex'; }
  public updateTracks(tracks: TrackTransferItem[]): void {
    if (this.currentDetails) { this.currentDetails.tracks = tracks; this.authoritative = false; this.render(); void this.refreshFromHost(); }
  }
  public async refreshFromHost(): Promise<boolean> {
    const details = this.currentDetails;
    if (!details || !this.fallbackToken || !this.isHost) { this.authoritative = false; this.render(); return false; }
    const revision = ++this.refreshing;
    this.authoritative = false; this.render();
    try {
      const res = await fetch(`/api/v1/takes/${encodeURIComponent(details.takeId)}`, {
        headers: {Authorization:`Bearer ${this.fallbackToken}`}, signal: AbortSignal.timeout(10_000), cache:'no-store',
      });
      if (!res.ok) throw new Error(`Host status unavailable (HTTP ${res.status})`);
      const data = await res.json();
      if (!data.take || !Array.isArray(data.tracks) || data.take.id !== details.takeId) throw new Error('Invalid host take status');
      if (revision !== this.refreshing || this.currentDetails !== details) return false;
      const local = new Map(details.tracks.map(track => [track.trackId,track.localBytes]));
      details.tracks = data.tracks.map((track: {trackId:string;kind:'audio'|'video';status:string;totalBytes:number;lastSeq:number;participantName:string;sourceLabel:string}) => ({
        trackId:track.trackId,kind:track.kind,participantName:track.participantName,sourceLabel:track.sourceLabel,
        localBytes:local.get(track.trackId)||0, serverBytes:track.totalBytes||0,lastSeq:track.lastSeq,
        status:track.status === 'partial' ? 'partial' : track.status === 'processed' ? 'processed' : ['assembled','verified','ready'].includes(track.status) ? 'assembled' : track.status === 'uploading' ? 'uploading' : 'missing',
      }));
      this.missingParticipants = Array.isArray(data.missingParticipants) ? data.missingParticipants.length : 0;
      this.exportView = parseTakeExport(data);
      this.takeComplete = data.take.status === 'complete';
      this.takeStopped = ['stopped','uploading','processing','ready','failed','complete'].includes(data.take.status);
      this.authoritative = true; this.render(); return true;
    } catch (error) {
      if (revision === this.refreshing) { this.message = error instanceof Error ? error.message : String(error); this.authoritative = false; this.render(); }
      return false;
    }
  }
  private canExport(): boolean {
    return this.isHost && this.authoritative && !this.takeComplete && !this.exportRunning() && this.takeStopped && this.missingParticipants === 0 && !!this.currentDetails?.tracks.length &&
      this.currentDetails.tracks.every(track => track.status === 'assembled');
  }
  private render(): void {
    const d = this.currentDetails; if (!d) return;
    const received = (track:TrackTransferItem) => ['assembled','verified','processed'].includes(track.status);
    const rows = d.tracks.map(track => `<tr><td><strong class="transfer-source-name">${h(track.participantName)}</strong><span class="transfer-source-label">${h(track.sourceLabel)}</span></td>
      <td class="transfer-number">${(track.localBytes/1048576).toFixed(1)} MB</td><td class="transfer-number">${(track.serverBytes/1048576).toFixed(1)} MB</td>
      <td><span class="transfer-status ${received(track)?'':'pending'}">${track.status === 'partial' ? 'Only partly saved, check it' : track.status === 'processed' ? 'Ready for editing' : received(track) ? 'Saved on the host' : track.status === 'uploading' ? 'Uploading' : track.status === 'paused' ? 'Paused' : track.status === 'missing' ? 'Not on the host yet' : h(track.status)}</span><span class="transfer-source-label">${track.status==='processed'?'Check the timing in your editor':received(track)?'Checked again when files for editing are made':'Finishes from the device that recorded it'}</span></td></tr>`).join('');
    this.modal.classList.toggle('is-guest', !this.isHost);
    this.modal.innerHTML = `<div class="modal-dialog transfer-dialog">
      <div class="modal-header"><div><h2>Uploads</h2><p class="transfer-meeting">${h(d.sessionTitle)}</p></div><button id="close-transfers-btn" class="icon-button" aria-label="Close uploads">×</button></div>
      <div class="transfer-summary"><span>Recording ${h(d.takeNumber)}, ${d.tracks.length} ${d.tracks.length === 1 ? 'file' : 'files'}</span><span class="transfer-state" id="transfer-overall-badge">${this.authoritative ? (this.takeComplete ? 'Files for editing are ready' : this.canExport() ? 'All recordings saved on the host' : this.missingParticipants ? `Waiting for ${this.missingParticipants} ${this.missingParticipants === 1 ? 'person' : 'people'} to upload` : 'Still uploading') : 'Checking with the host…'}</span></div>
      <div class="table-scroll"><table class="transfer-table"><thead><tr><th>Person</th><th>In this browser</th><th>On the host</th><th>Status</th></tr></thead><tbody>${rows || '<tr><td colspan="4">Nothing recorded yet.</td></tr>'}</tbody></table></div>
      
      <div class="recovery-actions"><button id="sync-chunks-btn" class="btn btn-secondary">Finish uploads from this browser</button><button id="refresh-transfers-btn" class="btn btn-quiet">Check again</button></div>
      <p id="sync-status-msg" role="status">${h(this.message)}</p>
      <div id="export-progress" class="export-progress">${this.exportMarkup()}</div>
      <div class="modal-footer"><p>Keep this browser’s site data until every recording is saved on the host.</p><button id="prepare-export-btn" class="btn btn-primary" ${this.canExport()?'':'disabled'}>${this.takeComplete?'Files for editing are ready':'Get files for editing'}</button></div>
    </div>`;
    this.modal.querySelector<HTMLButtonElement>('#close-transfers-btn')!.onclick = () => this.hide();
    this.modal.querySelector<HTMLButtonElement>('#refresh-transfers-btn')!.onclick = () => { void this.refreshFromHost(); };
    this.modal.querySelector<HTMLButtonElement>('#prepare-export-btn')!.onclick = async () => {
      if (await this.refreshFromHost() && this.canExport()) { this.onExportRequested?.(d.takeId); this.watchExport(d.takeId); }
    };
    this.modal.querySelector<HTMLButtonElement>('#sync-chunks-btn')!.onclick = async event => {
      (event.currentTarget as HTMLButtonElement).disabled = true;
      try {
        const tracks = (await this.opfs.listUnfinishedTracks()).filter(meta => meta.takeId === d.takeId);
        let recovered = 0;
        for (const meta of tracks) {
          if (await recoverTakeFromOPFS(meta,this.fallbackToken)) recovered++;
        }
        this.message = tracks.length ? `${recovered} of ${tracks.length} local tracks received by host. ${tracks.length-recovered} still need recovery or review.` : 'No unfinished local tracks found in this browser. Other devices must upload their own tracks.';
        await this.refreshFromHost();
      } catch (error) { this.message = error instanceof Error ? error.message : String(error); }
      this.render();
    };
  }
}
