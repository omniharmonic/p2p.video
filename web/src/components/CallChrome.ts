// Call screen chrome [SL-UX-1, SL-UX-2]: presentation only. Arranges controls
// that other components create into the Meet-style call: a top bar (time,
// meeting title with details, REC pill, people), a centred dock (mic ▾,
// camera ▾, present, react, captions, hand, more, Record, Leave/End), a right
// cluster (details, people, chat, Studio) and the Studio panel that gathers the
// recording and production tools. It keeps icon/tooltip/state attributes in
// sync with the text their owners write, shows the host's "Your meeting's
// ready" card, drives the live sound ring and folds secondary controls into
// "More options" on phones. It never changes what a control does: elements
// keep their ids, listeners and owners; only their parent and attributes change.
// Layout reference: docs/design/call-screen.md.
import { ICONS, type IconName } from './icons';

const NARROW = '(max-width: 640px)';
/** Controls that fold into "More options" on phones, in dock order. */
const COLLAPSIBLE = ['.cc-wrap', '#hand-btn', '.reaction-wrap', '#screen-share-btn'];
/** Controls that live in the Studio panel, by section. Missing ones are skipped. */
const STUDIO_SHOW = ['livestream-btn', 'prompter-btn', 'branding-btn', 'phone-camera-btn'];
const STUDIO_MEETING = ['producer-link-btn'];
/** Studio rows without an icon of their own get one (CSS mask, survives text changes). */
const ROW_ICONS: Record<string, IconName> = {
  'dock-transfers-btn': 'transfers', 'chirp-btn': 'chirp', 'postproc-btn': 'export', 'settings-btn': 'settings',
  'livestream-btn': 'live', 'producer-link-btn': 'invite',
};

export interface CallChromeOptions {
  /** Copies the meeting link (host); resolves with a short status line. */
  copyMeetingLink?: () => Promise<string>;
  /** The capture stream, for the live sound ring on the mic button and self tile. */
  localStream?: () => MediaStream | null;
  /** The session id, so the ready card is shown once per meeting. */
  sessionId?: () => string;
}

const iconUrl = (name: IconName) => `url("data:image/svg+xml,${encodeURIComponent(ICONS[name].replace(/currentColor/g, '#000'))}")`;

function setIcon(el: HTMLElement, name: IconName): void {
  if (el.dataset.iconName === name) return;
  el.dataset.iconName = name;
  el.style.setProperty('--icon', iconUrl(name));
}

function setTip(el: HTMLElement, tip: string): void {
  if (tip && el.dataset.tip !== tip) el.dataset.tip = tip;
}

const shown = (el: HTMLElement | null): boolean => !!el && !el.hidden && el.style.display !== 'none';

export function mountCallChrome(opts: CallChromeOptions = {}): void {
  const studio = document.getElementById('studio-view');
  const dock = studio?.querySelector<HTMLElement>('.dock');
  if (!studio || !dock) return;
  const body = document.body;
  const right = dock.querySelector<HTMLElement>('.dock-right')!;
  const moreBtn = document.getElementById('more-btn') as HTMLButtonElement;
  const morePanel = document.getElementById('more-panel') as HTMLElement;
  const moreList = document.getElementById('more-list') as HTMLElement;
  const collapsed = morePanel.querySelector<HTMLElement>('.more-collapsed')!;
  const studioPanel = document.getElementById('studio-panel') as HTMLElement;
  const studioBtn = document.getElementById('studio-btn') as HTMLButtonElement;
  const stage = studio.querySelector<HTMLElement>('.stage-with-inspector')!;

  // Static icons declared in markup.
  for (const el of document.querySelectorAll<HTMLElement>('[data-icon]')) {
    const name = el.dataset.icon as IconName;
    if (ICONS[name]) el.innerHTML = ICONS[name];
  }

  // In-call mode: the page becomes a full-height call surface.
  const inCall = () => studio.style.display !== 'none' && studio.style.display !== '';
  const syncInCall = () => body.classList.toggle('in-call', inCall());
  new MutationObserver(syncInCall).observe(studio, { attributes: true, attributeFilter: ['style'] });
  syncInCall();

  // Recording consent and captions privacy notices join the admission
  // request in one notice row above the stage.
  const notices = studio.querySelector<HTMLElement>('.call-notices');
  for (const el of [document.getElementById('recording-banner'), document.getElementById('captions-indicator')]) {
    if (el && notices) notices.append(el);
  }

  // Right cluster (Meet: details, people, chat, activities): help and setup in More.
  const group = dock.querySelector<HTMLElement>('.transport-collab');
  const people = document.getElementById('people-btn');
  const chat = document.getElementById('chat-btn');
  const detailsBtn = document.getElementById('details-btn') as HTMLButtonElement;
  for (const el of [people, chat]) if (el) { el.dataset.collabItem = ''; right.insertBefore(el, studioBtn); }
  const help = document.getElementById('shortcuts-btn');
  if (help) { help.dataset.collabItem = ''; help.classList.add('more-item'); }
  if (group) {
    const syncGroup = () => dock.toggleAttribute('data-collab-off', group.hidden);
    new MutationObserver(syncGroup).observe(group, { attributes: true, attributeFilter: ['hidden'] });
    syncGroup();
  }

  // More options: devices, background, shortcuts, setup guide.
  for (const id of ['devices-btn', 'effects-btn']) { const el = document.getElementById(id); if (el) moreList.append(el); }
  if (help) moreList.append(help);
  const wizard = document.getElementById('wizard-btn');
  if (wizard) {
    const guide = document.createElement('button');
    guide.type = 'button';
    guide.className = 'btn more-item';
    guide.dataset.proxy = 'wizard-btn';
    guide.innerHTML = `<span class="collab-icon" aria-hidden="true">${ICONS.guide}</span><span class="collab-label">Setup guide</span>`;
    guide.onclick = () => wizard.click();
    moreList.append(guide);
  }
  for (const el of moreList.querySelectorAll<HTMLElement>('.collab-btn')) el.classList.add('more-item');
  // Anything else components put in More (now or later) that belongs in Studio moves there.
  const studioList = (name: string) => studioPanel.querySelector<HTMLElement>(`.studio-list[data-group="${name}"]`)!;
  const toStudio = () => {
    for (const [ids, list] of [[STUDIO_SHOW, studioList('show')], [STUDIO_MEETING, studioList('meeting')]] as const) {
      for (const id of ids) {
        const el = document.getElementById(id);
        if (!el || el.parentElement === list) continue;
        el.classList.remove('more-item', 'btn-quiet', 'btn-secondary', 'livestream-toolbar-btn');
        el.classList.add('studio-item');
        list.append(el);
      }
    }
    for (const [id, icon] of Object.entries(ROW_ICONS)) {
      const el = document.getElementById(id);
      if (el && !el.querySelector('.collab-icon')) { el.classList.add('studio-item', 'has-mask-icon'); setIcon(el, icon); }
    }
  };
  toStudio();
  new MutationObserver(toStudio).observe(moreList, { childList: true });
  // Live caption transcript: the host's caption menu download, offered in Studio too.
  const ccMenuBtn = document.getElementById('cc-menu-btn');
  const ccDownloads = document.querySelectorAll<HTMLButtonElement>('#cc-menu [data-format]');
  if (ccMenuBtn && ccDownloads.length) {
    const row = document.createElement('button');
    row.type = 'button';
    row.id = 'studio-captions-btn';
    row.className = 'btn studio-item has-mask-icon';
    row.textContent = 'Download captions';
    setIcon(row, 'cc');
    row.onclick = () => ccDownloads[0].click();
    studioList('show').append(row);
    const syncCc = () => { row.hidden = ccMenuBtn.hidden; };
    new MutationObserver(syncCc).observe(ccMenuBtn, { attributes: true, attributeFilter: ['hidden'] });
    syncCc();
  }

  // "Live" chip mirrors the livestream control, which lives in Studio.
  const live = document.getElementById('livestream-btn');
  const liveChip = document.getElementById('live-chip');
  if (live && liveChip) {
    const syncLive = () => { liveChip.hidden = !live.classList.contains('is-live'); };
    new MutationObserver(syncLive).observe(live, { attributes: true, attributeFilter: ['class'] });
    syncLive();
  }

  // Mic ▾ and camera ▾: split buttons whose caret opens the device settings.
  const mic = document.getElementById('toggle-mic-btn');
  const cam = document.getElementById('toggle-cam-btn');
  const devices = document.getElementById('devices-btn');
  const split = (ctl: HTMLElement | null, id: string, label: string, focus: string) => {
    if (!ctl || !devices) return;
    const wrap = document.createElement('span');
    wrap.className = 'dock-split';
    wrap.dataset.for = ctl.id;
    ctl.before(wrap);
    const caret = document.createElement('button');
    caret.type = 'button';
    caret.id = id;
    caret.className = 'btn dock-caret';
    caret.setAttribute('aria-label', label);
    caret.dataset.tip = label;
    caret.innerHTML = ICONS.caret;
    caret.onclick = () => {
      const pop = document.getElementById('device-popover');
      if (pop?.hidden) devices.click();
      window.setTimeout(() => document.getElementById(focus)?.focus(), 60);
    };
    wrap.append(ctl, caret);
  };
  split(mic, 'mic-options-btn', 'Audio settings', 'call-mic-select');
  split(cam, 'cam-options-btn', 'Video settings', 'call-camera-select');
  // Camera ▾ also reaches background effects from the device settings.
  const devicePop = document.getElementById('device-popover');
  const effectsBtn = document.getElementById('effects-btn');
  if (devicePop && effectsBtn) {
    const bg = document.createElement('button');
    bg.type = 'button';
    bg.className = 'btn btn-secondary btn-compact device-bg-btn';
    bg.dataset.proxy = 'effects-btn';
    bg.textContent = 'Background effects';
    bg.onclick = () => { (devicePop.querySelector('[data-close]') as HTMLButtonElement | null)?.click(); effectsBtn.click(); };
    devicePop.append(bg);
  }

  // Icon, state and tooltip for controls whose owners write plain text.
  const rec = document.getElementById('rec-btn');
  const recTimer = document.getElementById('rec-timer');
  const sync = () => {
    if (mic) {
      const text = (mic.textContent || '').trim();
      const off = /^Unmute/.test(text);
      mic.dataset.state = off ? 'off' : 'on';
      mic.classList.add('dock-ctl', 'has-mask-icon');
      setIcon(mic, off ? 'micOff' : 'mic');
      setTip(mic, /host muted/.test(text) ? 'The host muted you. Turn on microphone' : off ? 'Turn on microphone' : 'Turn off microphone');
      mic.closest('.dock-split')?.setAttribute('data-state', mic.dataset.state);
    }
    if (cam) {
      const off = /^Enable/.test(cam.textContent || '');
      cam.dataset.state = off ? 'off' : 'on';
      cam.classList.add('dock-ctl', 'has-mask-icon');
      setIcon(cam, off ? 'camOff' : 'cam');
      setTip(cam, off ? 'Turn on camera' : 'Turn off camera');
      cam.closest('.dock-split')?.setAttribute('data-state', cam.dataset.state);
    }
    if (rec) {
      const stop = /^Stop/.test(rec.textContent || '');
      rec.dataset.state = stop ? 'recording' : 'idle';
      rec.classList.add('has-mask-icon');
      setIcon(rec, stop ? 'stop' : 'record');
      const t = recTimer?.textContent?.trim() || '';
      const label = stop ? `Stop recording${t && t !== '0:00' ? `  ${t}` : ''}` : 'Record';
      if (rec.dataset.label !== label) rec.dataset.label = label;
    }
    const end = document.getElementById('end-meeting-btn');
    if (end) { end.classList.add('has-mask-icon', 'dock-leave'); setIcon(end, 'end'); setTip(end, 'End meeting for everyone'); end.setAttribute('aria-label', 'End meeting for everyone'); }
    const leave = document.getElementById('leave-btn');
    if (leave) { leave.classList.add('has-mask-icon', 'dock-leave'); setIcon(leave, 'end'); setTip(leave, 'Leave call'); }
    for (const b of dock.querySelectorAll<HTMLElement>('.collab-btn')) {
      const label = b.getAttribute('aria-label') || b.querySelector('.collab-label')?.textContent || '';
      setTip(b, label.trim());
    }
  };
  new MutationObserver(sync).observe(dock, { childList: true, characterData: true, subtree: true });
  if (recTimer) new MutationObserver(sync).observe(recTimer, { childList: true, characterData: true, subtree: true });
  sync();

  // ------------------------------------------------------------ popovers
  // More options: a disclosure popover. Escape or a click outside closes it;
  // activating an item closes it, except controls that open their own picker.
  const setOpen = (open: boolean, focus = false) => {
    morePanel.hidden = !open;
    moreBtn.setAttribute('aria-expanded', String(open));
    moreBtn.classList.toggle('active', open);
    if (open) {
      const first = [...morePanel.querySelectorAll<HTMLElement>('button')].find(b => b.offsetParent !== null);
      first?.focus();
    } else if (focus) moreBtn.focus();
  };
  moreBtn.addEventListener('click', () => setOpen(morePanel.hidden));
  morePanel.addEventListener('click', e => {
    const t = e.target as HTMLElement;
    if (!t.closest('button') || t.closest('#react-btn, #cc-menu-btn, .cc-menu, .reaction-picker')) return;
    setOpen(false);
  });
  morePanel.addEventListener('keydown', e => { if (e.key === 'Escape' && !morePanel.hidden) { e.stopPropagation(); setOpen(false, true); } });
  moreBtn.addEventListener('keydown', e => { if (e.key === 'Escape' && !morePanel.hidden) setOpen(false); });
  document.addEventListener('pointerdown', e => {
    if (!morePanel.hidden && !(e.target as HTMLElement).closest('.more-wrap')) setOpen(false);
  });

  // Meeting details (ⓘ): link, recording and connection.
  const details = document.getElementById('call-details') as HTMLElement;
  const titleBtn = document.getElementById('call-details-btn') as HTMLButtonElement;
  let detailsReturn: HTMLElement | null = null;
  const setDetails = (open: boolean, from?: HTMLElement) => {
    details.hidden = !open;
    for (const b of [titleBtn, detailsBtn]) b.setAttribute('aria-expanded', String(open && b === (from ?? titleBtn)));
    detailsBtn.classList.toggle('active', open && from === detailsBtn);
    details.classList.toggle('from-dock', open && from === detailsBtn);
    if (open) { detailsReturn = from ?? titleBtn; details.querySelector<HTMLElement>('button:not([hidden])')?.focus(); }
    else if (from) from.focus();
  };
  titleBtn.onclick = () => setDetails(details.hidden, titleBtn);
  detailsBtn.onclick = () => setDetails(details.hidden, detailsBtn);
  details.querySelector<HTMLButtonElement>('[data-close]')!.onclick = () => setDetails(false, detailsReturn ?? titleBtn);
  details.addEventListener('keydown', e => { if (e.key === 'Escape') { e.stopPropagation(); setDetails(false, detailsReturn ?? titleBtn); } });
  document.addEventListener('pointerdown', e => {
    const t = e.target as HTMLElement;
    if (!details.hidden && !details.contains(t) && !titleBtn.contains(t) && !detailsBtn.contains(t)) setDetails(false);
  });
  const invite = document.getElementById('copy-invite-btn') as HTMLButtonElement | null;
  const copyLink = async (status: HTMLElement | null) => {
    if (!opts.copyMeetingLink || !status) return;
    status.textContent = 'Copying…';
    status.textContent = await opts.copyMeetingLink().catch(err => String(err instanceof Error ? err.message : err));
  };
  details.querySelector<HTMLButtonElement>('[data-copy-link]')!.onclick = () => void copyLink(details.querySelector('[data-link-status]'));
  details.querySelector<HTMLButtonElement>('[data-add-others]')!.onclick = () => { setDetails(false); invite?.click(); };
  const connText = document.getElementById('conn-text');
  const connMirror = details.querySelector<HTMLElement>('[data-conn-mirror]')!;
  if (connText) {
    const mirror = () => { connMirror.textContent = friendlyConnection(connText.textContent || ''); };
    new MutationObserver(mirror).observe(connText, { childList: true, characterData: true, subtree: true });
    mirror();
  }

  // Host-only bits follow the Add others control, which the app shows only to the host.
  const isHost = () => !!invite && invite.style.display !== 'none' && invite.style.display !== '';
  const syncHost = () => {
    body.classList.toggle('call-host', isHost());
    for (const el of details.querySelectorAll<HTMLElement>('[data-host-only]')) el.hidden = !isHost();
  };

  // "Your meeting's ready": the host's first view of a new meeting.
  const ready = document.createElement('section');
  ready.id = 'meeting-ready';
  ready.className = 'meeting-ready';
  ready.setAttribute('aria-labelledby', 'meeting-ready-title');
  ready.hidden = true;
  ready.innerHTML = `<div class="meeting-ready-head"><h2 id="meeting-ready-title">Your meeting’s ready</h2><button type="button" class="icon-button" data-close aria-label="Close">×</button></div>
    <button type="button" class="btn btn-primary meeting-ready-add" data-add-others><span class="ui-icon" aria-hidden="true">${ICONS.invite}</span>Add others</button>
    <p class="meeting-ready-or">Or share this meeting link with others you want in the meeting</p>
    <button type="button" class="meeting-ready-link" data-copy-link><span>Copy meeting link</span><span class="ui-icon" aria-hidden="true">${ICONS.copy}</span></button>
    <p class="meeting-ready-status" data-link-status role="status"></p>
    <p class="meeting-ready-note"><span class="ui-icon" aria-hidden="true">${ICONS.people}</span>People who use this meeting link must get your permission before they can join.</p>`;
  stage.append(ready);
  const readyKey = () => `sl.ready-shown.${opts.sessionId?.() || ''}`;
  const setReady = (open: boolean) => {
    ready.hidden = !open;
    if (open) ready.querySelector<HTMLElement>('[data-add-others]')?.focus();
  };
  ready.querySelector<HTMLButtonElement>('[data-close]')!.onclick = () => setReady(false);
  ready.querySelector<HTMLButtonElement>('[data-add-others]')!.onclick = () => invite?.click();
  ready.querySelector<HTMLButtonElement>('[data-copy-link]')!.onclick = () => void copyLink(ready.querySelector('[data-link-status]'));
  ready.addEventListener('keydown', e => { if (e.key === 'Escape') { e.stopPropagation(); setReady(false); } });
  details.querySelector<HTMLButtonElement>('[data-show-ready]')!.onclick = () => { setDetails(false); setReady(true); };
  const maybeShowReady = () => {
    syncHost();
    if (!inCall() || !isHost()) { if (!isHost()) ready.hidden = true; return; }
    let seen = false;
    try { seen = sessionStorage.getItem(readyKey()) === '1'; sessionStorage.setItem(readyKey(), '1'); } catch { /* shown each time */ }
    if (!seen && ready.dataset.shown !== readyKey()) { ready.dataset.shown = readyKey(); ready.hidden = false; }
  };
  if (invite) new MutationObserver(maybeShowReady).observe(invite, { attributes: true, attributeFilter: ['style'] });
  new MutationObserver(maybeShowReady).observe(studio, { attributes: true, attributeFilter: ['style'] });
  maybeShowReady();

  // ------------------------------------------------------------ Studio panel
  const callPanel = document.getElementById('call-panel');
  const setStudio = (open: boolean, focus = false) => {
    if (open && callPanel?.classList.contains('open')) right.querySelector<HTMLElement>('[data-collab-item][aria-expanded="true"]')?.click();
    studioPanel.hidden = !open;
    stage.classList.toggle('studio-open', open);
    studioBtn.setAttribute('aria-expanded', String(open));
    studioBtn.classList.toggle('active', open);
    if (open) { setReady(false); studioPanel.querySelector<HTMLElement>('[data-close]')?.focus(); }
    else if (focus) studioBtn.focus();
  };
  studioBtn.onclick = () => setStudio(studioPanel.hidden);
  // On phones Studio is a full-screen sheet: a tool that opens its own view closes it.
  const sheet = window.matchMedia('(max-width: 820px)');
  studioPanel.addEventListener('click', e => {
    if (sheet.matches && (e.target as HTMLElement).closest('.studio-item')) window.setTimeout(() => setStudio(false), 0);
  });
  studioPanel.querySelector<HTMLButtonElement>('[data-close]')!.onclick = () => setStudio(false, true);
  studioPanel.addEventListener('keydown', e => {
    if (e.key === 'Escape' && !(e.target as HTMLElement).closest('.preflight-popover')) { e.stopPropagation(); setStudio(false, true); }
  });
  if (callPanel) new MutationObserver(() => { if (callPanel.classList.contains('open') && !studioPanel.hidden) setStudio(false); }).observe(callPanel, { attributes: true, attributeFilter: ['class'] });

  // People: per-person "Connection & recording" (host and producer).
  const healthSection = document.getElementById('health-section');
  const peoplePanel = callPanel?.querySelector('.people-panel');
  if (healthSection && peoplePanel) peoplePanel.append(healthSection);
  const healthAside = document.getElementById('session-health-aside');
  const healthToggle = document.getElementById('toggle-health-btn');
  if (healthAside && healthToggle) {
    const syncHealth = () => healthToggle.setAttribute('aria-expanded', String(shown(healthAside)));
    new MutationObserver(syncHealth).observe(healthAside, { attributes: true, attributeFilter: ['style'] });
    syncHealth();
  }

  // Top bar: participant avatars mirror the people list; a click opens People.
  const avatars = document.getElementById('call-avatars') as HTMLButtonElement;
  const stack = avatars.querySelector<HTMLElement>('.call-avatar-stack')!;
  const count = avatars.querySelector<HTMLElement>('.call-avatar-count')!;
  avatars.onclick = () => { if (people?.getAttribute('aria-expanded') !== 'true') people?.click(); };
  const peopleList = callPanel?.querySelector('.people-list');
  const syncAvatars = () => {
    const rows = [...(peopleList?.querySelectorAll<HTMLElement>('.people-row') ?? [])];
    const initials = rows.slice(0, 3).map(r => r.querySelector('.people-avatar')?.textContent || '?');
    if (stack.dataset.key !== initials.join('')) {
      stack.dataset.key = initials.join('');
      stack.replaceChildren(...initials.map((ch, i) => { const s = document.createElement('span'); s.className = `call-avatar tone-${i}`; s.textContent = ch; return s; }));
    }
    count.textContent = rows.length ? String(rows.length) : '';
    avatars.hidden = rows.length === 0;
    avatars.setAttribute('aria-label', rows.length ? `People, ${rows.length} in the call` : 'People');
  };
  if (peopleList) new MutationObserver(syncAvatars).observe(peopleList, { childList: true, subtree: true });
  syncAvatars();

  // Recording state for the Studio panel headline and the Studio button.
  const recIndicator = document.getElementById('rec-indicator');
  const headline = document.getElementById('studio-rec-headline');
  const syncRec = () => {
    const on = recIndicator?.style.display === 'inline-flex';
    body.classList.toggle('call-recording', on);
    if (headline) headline.textContent = on ? `Recording ${recTimer?.textContent || ''}`.trim() : 'Not recording';
  };
  if (recIndicator) new MutationObserver(syncRec).observe(recIndicator, { attributes: true, attributeFilter: ['style'] });
  if (recTimer) new MutationObserver(syncRec).observe(recTimer, { childList: true, characterData: true, subtree: true });
  syncRec();

  // Phones: fold secondary controls into the menu; restore them on widen.
  const homes = new Map<Element, ParentNode>();
  const fold = (narrow: boolean) => {
    for (const sel of COLLAPSIBLE) {
      const el = document.querySelector(sel);
      if (!el) continue;
      if (narrow && el.parentElement !== collapsed) { homes.set(el, el.parentNode!); collapsed.append(el); }
      else if (!narrow && el.parentElement === collapsed) homes.get(el)?.appendChild(el);
    }
    morePanel.classList.toggle('is-narrow', narrow);
  };
  const mq = window.matchMedia(NARROW);
  mq.addEventListener('change', e => fold(e.matches));
  fold(mq.matches);

  // Local time at the start of the top bar (Meet shows it there).
  const clock = document.getElementById('call-clock');
  const tick = () => { if (clock) clock.textContent = new Date().toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' }); };
  tick();
  window.setInterval(tick, 15000);

  if (opts.localStream) mountSoundRing(opts.localStream, inCall, mic, document.getElementById('local-tile'));
}

/** The connection line for people, not engineers. The raw state stays in #conn-text. */
export function friendlyConnection(raw: string): string {
  if (raw === 'Remote video decoded') return 'Connected. Everyone’s video is coming through.';
  if (/^Peer connected/.test(raw)) return 'Connected. Waiting for video from others.';
  if (/disconnected/i.test(raw)) return 'Reconnecting to the host…';
  if (/^Not connected/.test(raw)) return 'Not connected yet.';
  if (/media (failed|disconnected)/.test(raw)) return 'Call connection interrupted. Reconnecting…';
  if (/media (new|connecting)/.test(raw)) return 'Connecting the call…';
  return raw;
}

/**
 * The live sound ring [SL-UX-2]: your own level drives a soft ring on the dock
 * mic button and your tile (remote tiles get theirs from LayoutSwitcher). It
 * reads the capture stream with an analyser only; nothing is recorded or sent.
 * Under prefers-reduced-motion the CSS shows a steady ring instead.
 */
function mountSoundRing(stream: () => MediaStream | null, active: () => boolean, mic: HTMLElement | null, tile: HTMLElement | null): void {
  if (typeof AudioContext === 'undefined') return;
  let ctx: AudioContext | null = null;
  let analyser: AnalyserNode | null = null;
  let source: MediaStreamAudioSourceNode | null = null;
  let trackId = '';
  let level = 0;
  let last = 0;
  const data = new Float32Array(512);
  const targets = [mic?.closest<HTMLElement>('.dock-split') ?? mic, tile].filter(Boolean) as HTMLElement[];
  const paint = (v: number) => {
    for (const el of targets) {
      el.style.setProperty('--level', v.toFixed(2));
      if (v > 0.12) el.dataset.speaking = ''; else delete el.dataset.speaking;
    }
  };
  const frame = (now: number) => {
    requestAnimationFrame(frame);
    if (now - last < 60) return;
    last = now;
    const track = active() ? stream()?.getAudioTracks().find(t => t.readyState === 'live') : undefined;
    if (!track || !track.enabled || mic?.dataset.state === 'off') { if (level) { level = 0; paint(0); } return; }
    try {
      if (!ctx) ctx = new AudioContext();
      if (ctx.state === 'suspended') void ctx.resume();
      if (track.id !== trackId) {
        source?.disconnect();
        source = ctx.createMediaStreamSource(new MediaStream([track]));
        analyser = ctx.createAnalyser();
        analyser.fftSize = 1024;
        source.connect(analyser);
        trackId = track.id;
      }
      analyser!.getFloatTimeDomainData(data);
      let sum = 0;
      for (const s of data) sum += s * s;
      const rms = Math.sqrt(sum / data.length);
      const target = Math.min(1, Math.max(0, (20 * Math.log10(rms + 1e-6) + 52) / 36));
      level = target > level ? level * 0.4 + target * 0.6 : level * 0.8 + target * 0.2;
      paint(level < 0.03 ? 0 : level);
    } catch { /* the ring is decoration; the call is unaffected */ }
  };
  requestAnimationFrame(frame);
}
