// "Add phone camera" (PRD F3) [SL-P1-9]: a participant asks the daemon for a
// single-use, 10-minute phone link, shown as a QR code drawn on this device.
// The phone joins as an extra source of this participant without another
// admission prompt, shows as a small "<Name> · Phone" tile, records locally
// and uploads under this participant (an extra angle in the export).
import { RoleSubsource, TypeRoster, type Envelope, type RosterPayload, type SubsourceLinkResponse } from '../lib/protocol';
import { expiryText, phoneLink, type PhoneLink } from '../lib/phonecam';
import { qrSvg } from '../lib/qr';
import { ICONS } from './icons';
import '../phonecam.css';

export interface PhoneCameraHost {
  sessionId(): string;
  /** This participant's signed grant (upload token); empty before admission. */
  grant(): string;
  selfId(): string;
  toast(message: string): void;
}

export class PhoneCamera {
  readonly menuButton: HTMLButtonElement;
  private dialog: HTMLDialogElement | null = null;
  private link: PhoneLink | null = null;
  private timer: number | null = null;
  private myPhones = new Set<string>();
  private phoneIds = new Set<string>();

  constructor(private readonly host: PhoneCameraHost, private readonly grid: HTMLElement) {
    this.menuButton = document.createElement('button');
    this.menuButton.type = 'button';
    this.menuButton.id = 'phone-camera-btn';
    this.menuButton.className = 'btn collab-btn';
    this.menuButton.innerHTML = `<span class="collab-icon" aria-hidden="true">${ICONS.phone}</span><span class="collab-label">Add phone camera</span>`;
    this.menuButton.addEventListener('click', () => void this.open());
    // Tiles can appear after the roster names them a phone.
    new MutationObserver(() => this.markTiles()).observe(grid, { childList: true });
  }

  /** Roster updates: which tiles are phones, and whether mine just connected. */
  handle(env: Envelope): void {
    if (env.t !== TypeRoster) return;
    const roster = env.payload as RosterPayload;
    const phones = (roster?.participants ?? []).filter(p => p.role === RoleSubsource);
    this.phoneIds = new Set(phones.map(p => p.participantId));
    const mine = new Set(phones.filter(p => p.parentParticipantId && p.parentParticipantId === this.host.selfId()).map(p => p.participantId));
    const joined = [...mine].some(id => !this.myPhones.has(id));
    this.myPhones = mine;
    if (joined) {
      this.host.toast('Your phone camera joined the call. It records on the phone and uploads under your name.');
      if (this.dialog?.open) this.connected();
    }
    this.markTiles();
  }

  private markTiles(): void {
    for (const tile of this.grid.querySelectorAll<HTMLElement>('.tile[data-participant-id]')) {
      tile.classList.toggle('tile-phone', this.phoneIds.has(tile.dataset.participantId || ''));
    }
  }

  private async open(): Promise<void> {
    if (!this.host.grant()) { this.host.toast('Join the call first, then add your phone.'); return; }
    if (!this.dialog) this.build();
    this.dialog!.showModal();
    await this.mint();
  }

  private build(): void {
    const d = document.createElement('dialog');
    d.id = 'phone-camera-dialog';
    d.className = 'room-dialog phone-dialog';
    d.setAttribute('aria-labelledby', 'phone-dialog-title');
    d.innerHTML = `<form method="dialog">
      <h2 id="phone-dialog-title">Add your phone as a camera</h2>
      <p>Scan the code with your phone’s camera. It joins as your extra angle, records on the phone and uploads under your name. No second approval is needed.</p>
      <div class="phone-qr" data-qr aria-live="polite"><span class="phone-qr-wait">Making a code…</span></div>
      <p class="phone-expiry" data-expiry></p>
      <p class="phone-warning" data-warning hidden>This link points at this computer (localhost), which your phone can’t open. Use your Tailscale address for p2p.video, then make a new code.</p>
      <label class="field-label" for="phone-link-input">Or open this link on your phone</label>
      <div class="phone-link-row"><input id="phone-link-input" readonly spellcheck="false"><button type="button" class="btn btn-secondary" data-copy>Copy</button></div>
      <p class="phone-status" data-status role="status">Waiting for your phone…</p>
      <div class="room-dialog-actions"><button type="button" class="btn btn-quiet" data-new>New code</button><button value="done" class="btn btn-primary">Done</button></div>
    </form>`;
    d.querySelector('[data-copy]')!.addEventListener('click', () => void this.copy());
    d.querySelector('[data-new]')!.addEventListener('click', () => void this.mint());
    d.addEventListener('close', () => { if (this.timer !== null) { clearInterval(this.timer); this.timer = null; } });
    document.body.append(d);
    this.dialog = d;
  }

  private async mint(): Promise<void> {
    const d = this.dialog!;
    const qr = d.querySelector<HTMLElement>('[data-qr]')!;
    const status = d.querySelector<HTMLElement>('[data-status]')!;
    status.textContent = 'Waiting for your phone…';
    d.classList.remove('is-connected');
    qr.innerHTML = '<span class="phone-qr-wait">Making a code…</span>';
    try {
      const res = await fetch(`/api/v1/sessions/${encodeURIComponent(this.host.sessionId())}/subsource`, { method: 'POST', headers: { Authorization: `Bearer ${this.host.grant()}` } });
      if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
      const link = phoneLink(await res.json() as SubsourceLinkResponse, location.origin);
      this.link = link;
      qr.innerHTML = qrSvg(link.url, 'QR code for your phone camera link');
      d.querySelector<HTMLInputElement>('#phone-link-input')!.value = link.url;
      d.querySelector<HTMLElement>('[data-warning]')!.hidden = link.reachable;
      const tick = () => {
        const text = expiryText(link.expiresAt, Date.now());
        d.querySelector<HTMLElement>('[data-expiry]')!.textContent = text;
        if (Date.now() >= link.expiresAt) qr.classList.add('is-expired');
      };
      qr.classList.remove('is-expired');
      tick();
      if (this.timer !== null) clearInterval(this.timer);
      this.timer = window.setInterval(tick, 1000);
    } catch (err) {
      qr.innerHTML = '<span class="phone-qr-wait">No code</span>';
      status.textContent = `Couldn’t make a phone link: ${err instanceof Error ? err.message : String(err)}`;
    }
  }

  private connected(): void {
    const d = this.dialog!;
    d.classList.add('is-connected');
    d.querySelector<HTMLElement>('[data-status]')!.textContent = 'Phone connected. You can close this.';
    d.querySelector<HTMLElement>('[data-expiry]')!.textContent = 'Code used.';
    if (this.timer !== null) { clearInterval(this.timer); this.timer = null; }
  }

  private async copy(): Promise<void> {
    if (!this.link) return;
    try { await navigator.clipboard.writeText(this.link.url); this.host.toast('Phone link copied. It works once, for 10 minutes.'); }
    catch { this.dialog?.querySelector<HTMLInputElement>('#phone-link-input')?.select(); }
  }
}
