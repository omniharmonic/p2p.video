import { OPFSStorage } from '../lib/opfs';
import type { OPFSTrackMeta } from '../lib/opfs';
import { recoverTakeFromOPFS } from '../lib/upload';
import { escapeHtml as h } from '../lib/html';

export class OPFSRecoveryBanner {
  private container = document.createElement('div');
  private opfs = OPFSStorage.getInstance();
  private tracks: OPFSTrackMeta[] = [];
  private resuming: Promise<void> | null = null;
  constructor(private fallbackToken: string, private onRecoverComplete?: () => void) {
    this.container.id='opfs-recovery-banner'; this.container.className='recovery-banner glass-panel';
  }
  public setToken(token:string):void { this.fallbackToken=token; }
  public async checkAndMount(target:HTMLElement):Promise<boolean> {
    this.tracks=await this.opfs.listUnfinishedTracks();
    if (!this.tracks.length) { this.container.remove(); return false; }
    this.render(); target.prepend(this.container); return true;
  }
  /**
   * Uploads and finalizes every unfinished local track. Safe to call on page
   * load: a track still recording in another open tab is skipped (Web Lock),
   * and interrupted captures are finalized as partial, never as complete.
   */
  public resume():Promise<void> {
    this.resuming ??= this.doResume().finally(()=>{this.resuming=null;});
    return this.resuming;
  }
  private async doResume():Promise<void> {
    if (!this.tracks.length) return;
    this.setBusy(true);
    let successes=0;let partials=0;
    for (const meta of this.tracks) {
      try { if (await recoverTakeFromOPFS(meta,this.fallbackToken,progress=>{
        const status=this.container.querySelector('#recovery-status');
        if(progress.partialCapture)partials++;
        if (status) status.textContent=progress.error || `Track ${meta.trackId}: ${progress.uploadedBytes} bytes received by host`;
      })) successes++; } catch { /* Keep the track listed for retry. */ }
    }
    try { this.tracks=await this.opfs.listUnfinishedTracks(); }
    catch (error) { this.render(`Couldn’t check the uploads in this browser: ${String(error)}`); return; }
    this.render(`${successes} saved on the host. ${this.tracks.length} still to upload. ${partials?'Part of the audio was saved; check this recording before editing.':''}`);
    if (!this.tracks.length) { this.container.querySelector<HTMLButtonElement>('#resume-opfs-btn')!.disabled=true; }
    if (successes) this.onRecoverComplete?.();
  }
  private setBusy(busy:boolean):void {
    for (const id of ['#resume-opfs-btn','#discard-opfs-btn']) { const b=this.container.querySelector<HTMLButtonElement>(id); if (b) b.disabled=busy; }
  }
  private render(message=''):void {
    this.container.innerHTML=`<div class="recovery-content"><div class="rec-details">
      <strong>${this.tracks.length?`${this.tracks.length} ${this.tracks.length===1?'recording':'recordings'} didn’t finish uploading`:'Uploads finished'}</strong>
      <p>They’re safe in this browser. Finish uploading so the host has them, and keep this site’s data until then. A recording that was cut off is marked for checking.</p>
      ${this.tracks.length?`<details class="recovery-ids"><summary>Technical details</summary>${this.tracks.map(meta=>`<div>Recording <code>${h(meta.takeId)}</code>, file <code>${h(meta.trackId)}</code></div>`).join('')}</details>`:''}
      <p id="recovery-status" role="status">${h(message)}</p></div>
      <div class="recovery-buttons"><button id="resume-opfs-btn" class="btn btn-primary">Finish uploading</button>
      <button id="discard-opfs-btn" class="btn btn-quiet">${this.tracks.length?'Delete from this browser':'Close'}</button></div></div>`;
    const resume=this.container.querySelector<HTMLButtonElement>('#resume-opfs-btn')!;
    const discard=this.container.querySelector<HTMLButtonElement>('#discard-opfs-btn')!;
    resume.onclick=()=>void this.resume();
    discard.onclick=async()=>{
      if (!this.tracks.length) { this.container.remove(); return; }
      resume.disabled=true; discard.disabled=true;
      try {
        for (const meta of this.tracks) await this.opfs.deleteTrack(meta.takeId,meta.trackId);
        this.tracks=await this.opfs.listUnfinishedTracks();
        if (!this.tracks.length) this.container.remove(); else this.render('Some recordings couldn’t be deleted from this browser.');
      } catch (error) {this.render(error instanceof Error?error.message:String(error));}
    };
  }
}
