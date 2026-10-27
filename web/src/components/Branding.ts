// Branding & lower thirds [SL-P5-2]. A drawer (More → Branding & titles) for
// the host and producers:
//   - host only: show name, accent colour, logo (PNG/JPEG ≤ 1 MB, uploaded to
//     our own origin), tile labels on/off, livestream lower-third mode. These
//     are host management HTTP routes; a producer never sees this section.
//   - host and producers: each participant's lower-third name and title, over
//     the WebSocket (the hub enforces the role).
// Every participant receives the brand (tile labels); main.ts forwards
// signaling messages to handle(). All user text is set with textContent or
// input values only.
import '../branding.css';
import { ICONS } from './icons';
import { brandStore, DEFAULT_ACCENT, validAccent, type BrandState } from '../lib/branding';
import { mountTileLowerThirds } from './LowerThirds';
import {
  BrandLogoMaxBytes, LowerThirdNameMaxRunes, LowerThirdTitleMaxRunes, BrandShowNameMaxRunes,
  TypeBranding, TypeLowerThird, TypeRoster,
  type BrandSettings, type BrandingPayload, type Envelope, type LowerThirdsMode, type RosterPayload,
} from '../lib/protocol';

export interface BrandingHost {
  send(env: Envelope): void;
  role(): 'host' | 'producer' | 'guest' | '';
  selfId(): string;
  toast(message: string): void;
  /** Session id and this page's credential (the host token for the host). */
  session(): { sessionId: string; token: string };
}

interface Person { participantId: string; name: string; role: string; }

export class Branding {
  readonly menuButton: HTMLButtonElement;
  private readonly panel: HTMLElement;
  private people: Person[] = [];
  private settings: BrandSettings | null = null;
  private logoUrl = '';

  constructor(private readonly host: BrandingHost) {
    this.menuButton = document.createElement('button');
    this.menuButton.type = 'button';
    this.menuButton.id = 'branding-btn';
    this.menuButton.className = 'btn collab-btn';
    this.menuButton.hidden = true;
    this.menuButton.innerHTML = `<span class="collab-icon" aria-hidden="true">${ICONS.brand}</span><span class="collab-label">Branding &amp; titles</span>`;
    this.menuButton.addEventListener('click', () => this.open());

    this.panel = document.createElement('div');
    this.panel.id = 'branding-panel';
    this.panel.className = 'settings-drawer-backdrop branding-backdrop';
    this.panel.innerHTML = `
      <div class="drawer-panel branding-drawer" role="dialog" aria-modal="true" aria-labelledby="branding-title">
        <div class="drawer-header"><div class="drawer-title-group"><h3 id="branding-title">Branding &amp; titles</h3></div><button type="button" class="close-icon-btn" data-close aria-label="Close branding">×</button></div>
        <div class="drawer-content">
          <section class="branding-host" id="brand-host-section" aria-labelledby="brand-look-title">
            <h4 class="branding-heading" id="brand-look-title">Show look</h4>
            <div class="drawer-section"><label class="section-label" for="brand-show-name">Show name</label><input id="brand-show-name" type="text" maxlength="${BrandShowNameMaxRunes}" autocomplete="off" placeholder="e.g. The Long Take"></div>
            <div class="drawer-section">
              <span class="section-label" id="brand-logo-label">Logo</span>
              <div class="branding-logo-row">
                <div class="branding-logo-preview" id="brand-logo-preview" aria-live="polite"><span>No logo</span></div>
                <div class="branding-logo-actions">
                  <label class="btn btn-secondary btn-compact branding-file"><input id="brand-logo-input" type="file" accept="image/png,image/jpeg" aria-labelledby="brand-logo-label"><span>Upload…</span></label>
                  <button type="button" class="btn btn-quiet btn-compact" id="brand-logo-remove" hidden>Remove</button>
                </div>
              </div>
              <p class="field-hint">PNG or JPEG, up to 1 MB. SVG isn't accepted. Shown in the branded livestream.</p>
            </div>
            <div class="drawer-section"><label class="section-label" for="brand-accent">Accent colour</label>
              <div class="branding-accent-row"><input id="brand-accent" type="color" value="${DEFAULT_ACCENT}"><code id="brand-accent-value">${DEFAULT_ACCENT}</code><div class="branding-swatch" aria-hidden="true"><span class="branding-swatch-strip"></span><span class="branding-swatch-text"><strong>Ana Ruiz</strong><small>Founder, Example</small></span></div></div></div>
            <label class="branding-switch-row"><span>Name and title labels on call tiles<small>Everyone in the call sees them.</small></span><button type="button" role="switch" class="switch" id="brand-tile-labels" aria-checked="false"><span></span></button></label>
            <div class="drawer-section"><label class="section-label" for="brand-lt-mode">Livestream lower thirds</label>
              <select id="brand-lt-mode"><option value="speaker">When someone starts speaking, and on demand</option><option value="manual">Only on demand</option><option value="off">Off</option></select></div>
            <div class="branding-save-row"><button type="button" class="btn btn-primary" id="brand-save">Save look</button><p class="branding-status" id="brand-status" role="status" aria-live="polite"></p></div>
          </section>
          <section class="branding-titles" aria-labelledby="brand-titles-title">
            <h4 class="branding-heading" id="brand-titles-title">Lower thirds</h4>
            <p class="field-hint">Name and title for each person. Used on tiles and in the livestream, and saved in each take's manifest.json for your editor. Recordings stay clean.</p>
            <p class="field-hint" id="brand-remember-hint" hidden>This room remembers each title by the person's name and fills it in next episode. Clear removes it here and for future episodes.</p>
            <div id="brand-people" class="branding-people"></div>
          </section>
        </div>
      </div>`;
    this.panel.addEventListener('click', e => { if (e.target === this.panel || (e.target as HTMLElement).closest('[data-close]')) this.close(); });
    this.panel.addEventListener('keydown', e => { if (e.key === 'Escape') { e.stopPropagation(); this.close(); } });
    const accent = this.q<HTMLInputElement>('#brand-accent');
    accent.addEventListener('input', () => this.previewAccent(accent.value));
    this.q('#brand-tile-labels').addEventListener('click', e => {
      const sw = e.currentTarget as HTMLElement;
      sw.setAttribute('aria-checked', String(sw.getAttribute('aria-checked') !== 'true'));
    });
    this.q('#brand-save').addEventListener('click', () => void this.save());
    this.q<HTMLInputElement>('#brand-logo-input').addEventListener('change', e => void this.uploadLogo(e.target as HTMLInputElement));
    this.q('#brand-logo-remove').addEventListener('click', () => void this.removeLogo());
    this.q('#brand-people').addEventListener('submit', e => {
      e.preventDefault();
      const form = e.target as HTMLFormElement;
      this.saveTitle(form);
    });
    this.q('#brand-people').addEventListener('click', e => {
      const btn = (e.target as HTMLElement).closest<HTMLButtonElement>('[data-clear]');
      const form = btn?.closest<HTMLFormElement>('form');
      if (form) this.clearTitle(form);
    });
  }

  mount(menu: HTMLElement | null = document.getElementById('more-list'), grid: HTMLElement | null = document.getElementById('video-grid')): void {
    menu?.append(this.menuButton);
    document.body.append(this.panel);
    mountTileLowerThirds(grid);
    brandStore.subscribe(() => this.renderPeople());
  }

  handle(env: Envelope): boolean {
    if (env.t === TypeBranding) {
      brandStore.selfId = this.host.selfId();
      brandStore.set(env.payload as BrandingPayload as BrandState);
      const role = this.host.role();
      this.menuButton.hidden = role !== 'host' && role !== 'producer';
      if (role === 'host' && this.settings && this.settings.logo?.version !== brandStore.get().logoVersion) void this.loadSettings(false);
      return true;
    }
    if (env.t === TypeRoster) {
      const roster = env.payload as RosterPayload;
      this.people = (roster?.participants ?? []).filter(p => p.role !== 'subsource').map(p => ({ participantId: p.participantId, name: p.name, role: p.role }));
      this.renderPeople();
    }
    return false; // the roster also feeds other components
  }

  private q<T extends HTMLElement = HTMLElement>(sel: string): T { return this.panel.querySelector(sel) as T; }

  private open(): void {
    const isHost = this.host.role() === 'host';
    this.q('#brand-host-section').hidden = !isHost;
    this.panel.classList.add('open');
    this.renderPeople(true);
    if (isHost) { this.setLoading(true); void this.loadSettings(true); }
    (this.panel.querySelector<HTMLElement>(isHost ? '[data-close]' : '#brand-people input') ?? this.q('[data-close]')).focus();
  }

  private close(): void { this.panel.classList.remove('open'); this.menuButton.focus({ preventScroll: true }); }

  private api(path = ''): string {
    return `/api/v1/sessions/${encodeURIComponent(this.host.session().sessionId)}/branding${path}`;
  }

  private auth(): Record<string, string> { return { Authorization: `Bearer ${this.host.session().token}` }; }

  private status(text: string, error = false): void {
    const el = this.q('#brand-status');
    el.textContent = text;
    el.classList.toggle('is-error', error);
  }

  /** Disables the look form while the saved settings load, so nothing typed is overwritten. */
  private setLoading(loading: boolean): void {
    for (const el of this.q('#brand-host-section').querySelectorAll<HTMLInputElement | HTMLButtonElement | HTMLSelectElement>('input, button, select')) el.disabled = loading;
    this.q('#brand-host-section').setAttribute('aria-busy', String(loading));
  }

  /** Reads the saved look; fillFields=false only refreshes the logo (another tab changed it). */
  private async loadSettings(fillFields: boolean): Promise<void> {
    try {
      const res = await fetch(this.api(), { headers: this.auth(), cache: 'no-store' });
      if (!res.ok) throw new Error(await res.text());
      this.settings = await res.json() as BrandSettings;
      if (fillFields) this.fill(this.settings);
      else this.q('#brand-logo-remove').hidden = !this.settings.logo;
      await this.loadLogoPreview();
    } catch {
      this.status('Brand settings are available on the host dashboard only.', true);
    } finally {
      if (fillFields) { this.setLoading(false); if (this.panel.classList.contains('open')) this.q('#brand-show-name').focus(); }
    }
  }

  private fill(s: BrandSettings): void {
    this.q<HTMLInputElement>('#brand-show-name').value = s.showName;
    const accent = validAccent(s.accent) ? s.accent : DEFAULT_ACCENT;
    this.q<HTMLInputElement>('#brand-accent').value = accent;
    this.previewAccent(accent);
    this.q('#brand-tile-labels').setAttribute('aria-checked', String(s.tileLabels));
    this.q<HTMLSelectElement>('#brand-lt-mode').value = s.lowerThirdsMode;
    this.q('#brand-logo-remove').hidden = !s.logo;
  }

  private previewAccent(accent: string): void {
    this.q('#brand-accent-value').textContent = accent;
    this.q('.branding-swatch').style.setProperty('--brand-accent', accent);
  }

  /** The logo preview, fetched with the host token and shown as a blob: URL (CSP img-src blob:). */
  private async loadLogoPreview(): Promise<void> {
    const holder = this.q('#brand-logo-preview');
    if (this.logoUrl) { URL.revokeObjectURL(this.logoUrl); this.logoUrl = ''; }
    if (!this.settings?.logo) { holder.replaceChildren(Object.assign(document.createElement('span'), { textContent: 'No logo' })); return; }
    const res = await fetch(this.api('/logo'), { headers: this.auth(), cache: 'no-store' });
    if (!res.ok) return;
    this.logoUrl = URL.createObjectURL(await res.blob());
    const img = document.createElement('img');
    img.alt = 'Current logo';
    img.src = this.logoUrl;
    holder.replaceChildren(img);
  }

  private async save(): Promise<void> {
    const body: BrandSettings = {
      showName: this.q<HTMLInputElement>('#brand-show-name').value.trim(),
      accent: this.q<HTMLInputElement>('#brand-accent').value.toLowerCase(),
      tileLabels: this.q('#brand-tile-labels').getAttribute('aria-checked') === 'true',
      lowerThirdsMode: this.q<HTMLSelectElement>('#brand-lt-mode').value as LowerThirdsMode,
    };
    this.status('Saving…');
    try {
      const res = await fetch(this.api(), { method: 'PUT', headers: { ...this.auth(), 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
      if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
      this.settings = await res.json() as BrandSettings;
      this.fill(this.settings);
      this.status('Saved. Everyone in the call has the new look.');
    } catch (e) {
      this.status(`Not saved: ${e instanceof Error ? e.message : String(e)}`, true);
    }
  }

  private async uploadLogo(input: HTMLInputElement): Promise<void> {
    const file = input.files?.[0];
    input.value = '';
    if (!file) return;
    if (file.type !== 'image/png' && file.type !== 'image/jpeg') { this.status('Choose a PNG or JPEG file (SVG isn\'t accepted).', true); return; }
    if (file.size > BrandLogoMaxBytes) { this.status('That file is larger than 1 MB.', true); return; }
    this.status('Uploading logo…');
    try {
      const res = await fetch(this.api('/logo'), { method: 'POST', headers: { ...this.auth(), 'Content-Type': file.type }, body: file });
      if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
      // Only the logo changed: keep any unsaved edits in the other fields.
      this.settings = await res.json() as BrandSettings;
      this.q('#brand-logo-remove').hidden = !this.settings.logo;
      await this.loadLogoPreview();
      this.status('Logo saved.');
    } catch (e) {
      this.status(`Logo not saved: ${e instanceof Error ? e.message : String(e)}`, true);
    }
  }

  private async removeLogo(): Promise<void> {
    try {
      const res = await fetch(this.api('/logo'), { method: 'DELETE', headers: this.auth() });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      // Only the logo changed: keep any unsaved edits in the other fields.
      this.settings = await res.json() as BrandSettings;
      this.q('#brand-logo-remove').hidden = !this.settings.logo;
      await this.loadLogoPreview();
      this.status('Logo removed.');
    } catch (e) {
      this.status(`Logo not removed: ${e instanceof Error ? e.message : String(e)}`, true);
    }
  }

  /** One small form per person; re-rendered only when the list or saved text changes. */
  private renderPeople(force = false): void {
    if (!this.panel.classList.contains('open') && !force) return;
    const holder = this.q('#brand-people');
    const brand = brandStore.get();
    this.q('#brand-remember-hint').hidden = !brand.rememberTitles;
    const key = JSON.stringify([this.people, brand.lowerThirds]);
    if (!force && holder.dataset.key === key) return;
    // Don't clobber a field someone is typing in.
    if (!force && holder.contains(document.activeElement)) return;
    holder.dataset.key = key;
    if (!this.people.length) { holder.replaceChildren(Object.assign(document.createElement('p'), { className: 'field-hint', textContent: 'People appear here when they join.' })); return; }
    holder.replaceChildren(...this.people.map(p => {
      const lt = brand.lowerThirds.find(l => l.participantId === p.participantId);
      const form = document.createElement('form');
      form.className = 'branding-person';
      form.dataset.ltRow = p.participantId;
      const who = document.createElement('div');
      who.className = 'branding-person-who';
      const nm = document.createElement('strong');
      nm.textContent = p.name || 'Guest';
      const role = document.createElement('span');
      role.textContent = p.role === 'host' ? 'Host' : p.role === 'producer' ? 'Producer' : 'Guest';
      who.append(nm, role);
      const name = this.input('name', `Name on screen (default: ${p.name || 'their name'})`, lt?.name ?? '', LowerThirdNameMaxRunes, p.name);
      const title = this.input('title', 'Title, e.g. Founder, Acme', lt?.title ?? '', LowerThirdTitleMaxRunes, '');
      const save = document.createElement('button');
      save.type = 'submit';
      save.className = 'btn btn-secondary btn-compact';
      save.textContent = 'Save';
      save.dataset.save = '';
      form.append(who, name, title, save);
      if (lt && (lt.name || lt.title)) {
        const clear = document.createElement('button');
        clear.type = 'button';
        clear.className = 'btn btn-quiet btn-compact';
        clear.textContent = 'Clear';
        clear.dataset.clear = '';
        clear.setAttribute('aria-label', `Clear the lower third for ${p.name || 'this guest'}`);
        who.append(clear); // top-right of the row (branding.css)
      }
      return form;
    }));
  }

  private input(field: string, label: string, value: string, max: number, placeholder: string): HTMLInputElement {
    const i = document.createElement('input');
    i.type = 'text';
    i.dataset.field = field;
    i.value = value;
    i.maxLength = max;
    i.autocomplete = 'off';
    i.placeholder = placeholder || label;
    i.setAttribute('aria-label', label);
    return i;
  }

  private saveTitle(form: HTMLFormElement): void {
    const participantId = form.dataset.ltRow || '';
    const name = form.querySelector<HTMLInputElement>('[data-field=name]')!.value.trim();
    const title = form.querySelector<HTMLInputElement>('[data-field=title]')!.value.trim();
    this.host.send({ t: TypeLowerThird, payload: { participantId, name, title } });
    (document.activeElement as HTMLElement | null)?.blur();
    this.host.toast(brandStore.get().rememberTitles ? 'Lower third saved. This room will remember it next episode.' : 'Lower third saved.');
  }

  /** Clears a person's lower third (and, in a room, forgets it for future episodes). */
  private clearTitle(form: HTMLFormElement): void {
    const participantId = form.dataset.ltRow || '';
    for (const i of form.querySelectorAll<HTMLInputElement>('input')) i.value = '';
    this.host.send({ t: TypeLowerThird, payload: { participantId, name: '', title: '' } });
    this.host.toast(brandStore.get().rememberTitles ? 'Lower third cleared and forgotten for this room.' : 'Lower third cleared.');
  }
}
