// Teleprompter [SL-P5-1]: a script editor for the host (and producers when
// the host allows) and a large-type reader near the top of the screen, where
// the camera is, so eyes stay close to the lens. Auto-scroll with speed and
// size controls, Space / arrow keys when focused, a mirrored mode for beam
// splitter glass, and "follow host" scroll sync. The hub enforces who may
// see and edit; the script is untrusted text and is rendered with
// textContent only. main.ts forwards signaling messages to handle().
import '../teleprompter.css';
import { ICONS } from './icons';
import {
  FONT_DEFAULT, FONT_STEP, SHARE_INTERVAL_MS, SPEED_DEFAULT, VISIBILITY_LABELS,
  advance, atEnd, canEdit, canSee, clampFont, clampSpeed, fromFraction, maxScroll, nudge, parseScript, shouldShare, toFraction, utf8Bytes,
  type PrompterRole, type Visibility,
} from '../lib/prompter';
import {
  PrompterMaxBytes, TypeActionError, TypePrompter, TypePrompterEdit, TypePrompterScroll,
  type ActionErrorPayload, type Envelope, type PrompterPayload, type PrompterScrollPayload,
} from '../lib/protocol';

export interface TeleprompterHost {
  send(env: Envelope): void;
  /** This participant's admitted role ('' before admission). */
  role(): PrompterRole;
  toast(message: string): void;
}

const PREFS_KEY = 'studiolink.prompter';
const SAVE_DELAY_MS = 600;

interface Prefs { font: number; speed: number; mirrored: boolean; }

function storage(): Storage | null {
  try { return window.localStorage; } catch { return null; }
}

function loadPrefs(): Prefs {
  try {
    const raw = JSON.parse(storage()?.getItem(PREFS_KEY) || '{}');
    return { font: clampFont(raw.font ?? FONT_DEFAULT), speed: clampSpeed(raw.speed ?? SPEED_DEFAULT), mirrored: raw.mirrored === true };
  } catch {
    return { font: FONT_DEFAULT, speed: SPEED_DEFAULT, mirrored: false };
  }
}

export class Teleprompter {
  readonly menuButton: HTMLButtonElement;
  private readonly editor: HTMLElement;
  private readonly reader: HTMLElement;
  private readonly scroller: HTMLElement;
  private readonly textView: HTMLElement;
  private readonly textarea: HTMLTextAreaElement;
  private readonly visibility: HTMLSelectElement;
  private state: PrompterPayload | null = null;
  private prefs = loadPrefs();
  private playing = false;
  private pos = 0;
  private raf: number | null = null;
  private lastFrame = 0;
  private sharing = false;
  private follow = true;
  private hostScroll: PrompterScrollPayload | null = null;
  private lastShared: { at: number; position: number; playing: boolean; speed: number } | null = null;
  private dirty = false;
  private saveTimer: number | null = null;
  private renderedText: string | null = null;
  private trailing: number | null = null;

  constructor(private readonly host: TeleprompterHost) {
    this.menuButton = document.createElement('button');
    this.menuButton.type = 'button';
    this.menuButton.id = 'prompter-btn';
    this.menuButton.className = 'btn collab-btn';
    this.menuButton.hidden = true;
    this.menuButton.innerHTML = `<span class="collab-icon" aria-hidden="true">${ICONS.prompter}</span><span class="collab-label">Teleprompter</span>`;
    this.menuButton.addEventListener('click', () => this.openFromMenu());

    this.editor = document.createElement('div');
    this.editor.id = 'prompter-editor';
    this.editor.className = 'settings-drawer-backdrop prompter-editor-backdrop';
    this.editor.innerHTML = `
      <div class="drawer-panel prompter-editor" role="dialog" aria-modal="true" aria-labelledby="prompter-editor-title">
        <div class="drawer-header"><div class="drawer-title-group"><h3 id="prompter-editor-title">Teleprompter</h3></div><button type="button" class="close-icon-btn" data-close aria-label="Close teleprompter editor">×</button></div>
        <div class="drawer-content">
          <div class="drawer-section">
            <label class="section-label" for="prompter-visibility">Who can see the script</label>
            <select id="prompter-visibility"></select>
            <p class="field-hint" id="prompter-visibility-hint"></p>
          </div>
          <div class="drawer-section prompter-script-section">
            <label class="section-label" for="prompter-text">Script</label>
            <textarea id="prompter-text" rows="14" spellcheck="true" placeholder="# Cold open&#10;Welcome back to the show…&#10;&#10;## Guest intro&#10;Today we're joined by…"></textarea>
            <div class="prompter-meta"><span class="field-hint"># Section, ## Sub-section. Line breaks are kept.</span><span class="field-hint" id="prompter-count"></span></div>
            <p class="prompter-save-status" id="prompter-status" role="status" aria-live="polite"></p>
          </div>
        </div>
        <div class="drawer-footer"><button type="button" class="btn btn-primary btn-block" id="prompter-open-reader">Open reader</button></div>
      </div>`;
    this.textarea = this.editor.querySelector('#prompter-text')!;
    this.visibility = this.editor.querySelector('#prompter-visibility')!;
    for (const v of Object.keys(VISIBILITY_LABELS) as Visibility[]) this.visibility.append(new Option(VISIBILITY_LABELS[v], v));
    this.editor.addEventListener('click', e => { if (e.target === this.editor || (e.target as HTMLElement).closest('[data-close]')) this.closeEditor(); });
    this.editor.addEventListener('keydown', e => { if (e.key === 'Escape') { e.stopPropagation(); this.closeEditor(); } });
    this.textarea.addEventListener('input', () => this.onType());
    this.visibility.addEventListener('change', () => this.host.send({ t: TypePrompterEdit, payload: { visibility: this.visibility.value as Visibility } }));
    this.editor.querySelector('#prompter-open-reader')!.addEventListener('click', () => { this.flushSave(); this.closeEditor(); this.openReader(); });

    this.reader = document.createElement('section');
    this.reader.id = 'prompter-reader';
    this.reader.className = 'prompter-reader';
    this.reader.setAttribute('aria-label', 'Teleprompter');
    this.reader.hidden = true;
    this.reader.innerHTML = `
      <div class="prompter-toolbar" role="toolbar" aria-label="Teleprompter controls">
        <button type="button" class="prompter-tool prompter-play" id="prompter-play" aria-pressed="false"><span class="prompter-play-icon" aria-hidden="true"></span><span class="prompter-play-label">Play</span></button>
        <div class="prompter-group" role="group" aria-label="Scroll speed"><button type="button" class="prompter-tool" data-act="slower" aria-label="Slower">−</button><span class="prompter-value" id="prompter-speed" aria-live="polite"></span><button type="button" class="prompter-tool" data-act="faster" aria-label="Faster">+</button></div>
        <div class="prompter-group" role="group" aria-label="Text size"><button type="button" class="prompter-tool" data-act="smaller" aria-label="Smaller text">A−</button><button type="button" class="prompter-tool" data-act="larger" aria-label="Larger text">A+</button></div>
        <button type="button" class="prompter-tool prompter-toggle" id="prompter-mirror" role="switch" aria-checked="false">Mirror</button>
        <button type="button" class="prompter-tool prompter-toggle" id="prompter-share" role="switch" aria-checked="false" hidden>Share scroll</button>
        <button type="button" class="prompter-tool prompter-toggle" id="prompter-follow" role="switch" aria-checked="true" hidden>Follow host</button>
        <span class="prompter-spacer"></span>
        <button type="button" class="prompter-tool" id="prompter-edit" hidden>Edit</button>
        <button type="button" class="prompter-tool prompter-close" id="prompter-close" aria-label="Close teleprompter">×</button>
      </div>
      <div class="prompter-viewport">
        <div class="prompter-scroll" id="prompter-scroll" tabindex="0" aria-label="Script. Space plays or pauses, arrow keys move a line."><div class="prompter-text" id="prompter-view"></div></div>
        <div class="prompter-marker" aria-hidden="true"></div>
      </div>`;
    this.scroller = this.reader.querySelector('#prompter-scroll')!;
    this.textView = this.reader.querySelector('#prompter-view')!;
    this.reader.querySelector('#prompter-play')!.addEventListener('click', () => this.setPlaying(!this.playing));
    this.reader.querySelector('#prompter-close')!.addEventListener('click', () => this.closeReader());
    this.reader.querySelector('#prompter-edit')!.addEventListener('click', () => this.openEditor());
    this.reader.querySelector('#prompter-mirror')!.addEventListener('click', () => { this.prefs.mirrored = !this.prefs.mirrored; this.savePrefs(); this.applyPrefs(); });
    this.reader.querySelector('#prompter-share')!.addEventListener('click', () => this.setSharing(!this.sharing));
    this.reader.querySelector('#prompter-follow')!.addEventListener('click', () => { this.follow = !this.follow; this.renderControls(); if (this.follow && this.hostScroll) this.applyHostScroll(this.hostScroll); });
    for (const b of this.reader.querySelectorAll<HTMLButtonElement>('[data-act]')) b.addEventListener('click', () => this.act(b.dataset.act!));
    this.reader.addEventListener('keydown', e => this.onKey(e));
    // Manual scrolling (wheel, trackpad) moves the reading position.
    this.scroller.addEventListener('scroll', () => { if (!this.playing) this.pos = this.scroller.scrollTop; this.share(); });
    window.addEventListener('resize', () => { if (!this.reader.hidden) this.clampPos(); });
  }

  mount(menu: HTMLElement | null = document.getElementById('more-list')): void {
    menu?.append(this.menuButton);
    document.body.append(this.editor, this.reader);
    this.applyPrefs();
  }

  /** Forwarded signaling messages; returns true when handled. */
  handle(env: Envelope): boolean {
    switch (env.t) {
      case TypePrompter: this.onState(env.payload as PrompterPayload); return true;
      case TypePrompterScroll: this.onHostScroll(env.payload as PrompterScrollPayload); return true;
      case TypeActionError: {
        const err = env.payload as ActionErrorPayload;
        // CallFeatures toasts every action error; the editor also shows it.
        if (err?.action === TypePrompterEdit) this.status(err.message, true);
        return false;
        return false;
      }
    }
    return false;
  }

  private role(): PrompterRole { return this.host.role(); }
  private vis(): Visibility { return (this.state?.visibility ?? 'host') as Visibility; }
  private editable(): boolean { return canEdit(this.role(), this.vis()); }
  private visible(): boolean { return !!this.state && !this.state.hidden && canSee(this.role(), this.vis()); }

  private onState(p: PrompterPayload): void {
    const wasVisible = this.visible() && !!this.state?.text;
    this.state = p;
    const role = this.role();
    this.menuButton.hidden = !(role === 'host' || this.editable() || (this.visible() && !!p.text));
    if (!this.visible()) {
      if (!this.reader.hidden) { this.closeReader(); this.host.toast('The host stopped sharing the script.'); }
      // Drop a script we may no longer see from the page.
      this.textView.replaceChildren();
      this.renderedText = null;
      this.hostScroll = null;
      if (!this.editable()) this.closeEditor();
    } else if (!wasVisible && p.text && role === 'guest') {
      this.host.toast('The host shared a script: More options → Teleprompter.');
    }
    // The editor: keep what the user is typing; otherwise show the latest.
    if (!this.dirty || this.textarea.value === p.text) {
      this.dirty = false;
      if (this.textarea.value !== p.text) this.textarea.value = p.text;
      this.status(p.revision ? `Saved${p.updatedBy ? ` · last edit by ${p.updatedBy}` : ''}` : 'Nothing saved yet.');
    } else {
      this.status(`${p.updatedBy || 'Someone'} changed the script while you were typing. Your next save replaces it.`);
    }
    this.visibility.value = p.visibility;
    this.renderCount();
    this.renderEditorRole();
    if (p.scroll && role !== 'host') this.hostScroll = p.scroll;
    if (!this.reader.hidden) { this.renderText(); this.renderControls(); }
  }

  private onHostScroll(p: PrompterScrollPayload): void {
    if (this.role() === 'host') return;
    this.hostScroll = p.shared ? p : null;
    this.renderControls();
    if (p.shared && this.follow && !this.reader.hidden) this.applyHostScroll(p);
  }

  private applyHostScroll(p: PrompterScrollPayload): void {
    this.prefs.speed = clampSpeed(p.speed || this.prefs.speed);
    this.pos = fromFraction(p.position, this.max());
    this.scroller.scrollTop = this.pos;
    this.setPlaying(p.playing, false);
    this.renderControls();
  }

  private openFromMenu(): void {
    if (this.editable()) this.openEditor();
    else if (this.visible()) this.openReader();
  }

  private openEditor(): void {
    if (!this.editable()) return;
    this.renderEditorRole();
    this.editor.classList.add('open');
    this.textarea.focus();
  }

  private closeEditor(): void {
    if (!this.editor.classList.contains('open')) return;
    this.flushSave();
    this.editor.classList.remove('open');
  }

  private renderEditorRole(): void {
    const isHost = this.role() === 'host';
    this.visibility.disabled = !isHost;
    const hint = this.editor.querySelector('#prompter-visibility-hint')!;
    hint.textContent = isHost
      ? 'Only you decide who sees it. Producers can edit unless it is “Only me”. Guests never edit.'
      : 'The host decides who sees the script.';
  }

  private onType(): void {
    this.dirty = true;
    this.renderCount();
    if (this.saveTimer !== null) clearTimeout(this.saveTimer);
    this.saveTimer = window.setTimeout(() => this.flushSave(), SAVE_DELAY_MS);
    this.status('Saving…');
  }

  private flushSave(): void {
    if (this.saveTimer !== null) { clearTimeout(this.saveTimer); this.saveTimer = null; }
    if (!this.dirty || !this.editable()) return;
    if (utf8Bytes(this.textarea.value) > PrompterMaxBytes) { this.status('The script is too long (64 KB at most). It is not saved.', true); return; }
    this.host.send({ t: TypePrompterEdit, payload: { text: this.textarea.value } });
  }

  private renderCount(): void {
    const kb = utf8Bytes(this.textarea.value) / 1024;
    const el = this.editor.querySelector('#prompter-count')!;
    el.textContent = `${kb.toFixed(1)} / 64 KB`;
    el.classList.toggle('is-over', kb > 64);
  }

  private status(text: string, error = false): void {
    const el = this.editor.querySelector('#prompter-status')!;
    el.textContent = text;
    el.classList.toggle('is-error', error);
  }

  private openReader(): void {
    if (!this.visible()) return;
    this.reader.hidden = false;
    document.body.classList.add('prompter-open');
    this.renderText();
    this.renderControls();
    if (this.role() !== 'host' && this.follow && this.hostScroll) this.applyHostScroll(this.hostScroll);
    this.scroller.focus({ preventScroll: true });
  }

  private closeReader(): void {
    if (this.reader.hidden) return;
    this.setPlaying(false);
    if (this.sharing) this.setSharing(false);
    this.reader.hidden = true;
    document.body.classList.remove('prompter-open');
    this.menuButton.focus({ preventScroll: true });
  }

  /** Renders the script as text nodes only (it is untrusted). */
  private renderText(): void {
    const text = this.state?.text ?? '';
    if (text === this.renderedText) return;
    const fraction = toFraction(this.pos, this.max());
    this.renderedText = text;
    const blocks = parseScript(text);
    if (!blocks.length) {
      const empty = document.createElement('p');
      empty.className = 'prompter-empty';
      empty.textContent = this.editable() ? 'No script yet. Choose Edit to write one.' : 'The script is empty.';
      this.textView.replaceChildren(empty);
    } else {
      this.textView.replaceChildren(...blocks.map(b => {
        const el = document.createElement(b.kind === 'h1' ? 'h2' : b.kind === 'h2' ? 'h3' : b.kind === 'gap' ? 'div' : 'p');
        el.className = `prompter-${b.kind}`;
        el.textContent = b.text;
        return el;
      }));
    }
    // Keep the reading position (as a fraction) across edits.
    this.pos = fromFraction(fraction, this.max());
    this.scroller.scrollTop = this.pos;
  }

  private renderControls(): void {
    const isHost = this.role() === 'host';
    const play = this.reader.querySelector<HTMLButtonElement>('#prompter-play')!;
    play.setAttribute('aria-pressed', String(this.playing));
    play.querySelector('.prompter-play-label')!.textContent = this.playing ? 'Pause' : 'Play';
    play.classList.toggle('is-playing', this.playing);
    this.reader.querySelector('#prompter-speed')!.textContent = `Speed ${this.prefs.speed}`;
    const share = this.reader.querySelector<HTMLButtonElement>('#prompter-share')!;
    share.hidden = !isHost;
    share.setAttribute('aria-checked', String(this.sharing));
    const follow = this.reader.querySelector<HTMLButtonElement>('#prompter-follow')!;
    follow.hidden = isHost || !this.hostScroll;
    follow.setAttribute('aria-checked', String(this.follow));
    this.reader.querySelector<HTMLButtonElement>('#prompter-edit')!.hidden = !this.editable();
    this.reader.querySelector<HTMLButtonElement>('#prompter-mirror')!.setAttribute('aria-checked', String(this.prefs.mirrored));
    this.reader.dataset.following = String(!isHost && this.follow && !!this.hostScroll);
  }

  private applyPrefs(): void {
    this.reader.style.setProperty('--prompter-font', `${this.prefs.font}px`);
    this.reader.classList.toggle('is-mirrored', this.prefs.mirrored);
    this.renderControls();
    if (!this.reader.hidden) this.clampPos();
  }

  private savePrefs(): void {
    try { storage()?.setItem(PREFS_KEY, JSON.stringify(this.prefs)); } catch { /* not remembered */ }
  }

  private act(action: string): void {
    // Changing speed while following the host takes you off "follow".
    if ((action === 'faster' || action === 'slower') && this.role() !== 'host' && this.follow && this.hostScroll) { this.follow = false; }
    if (action === 'faster') this.prefs.speed = clampSpeed(this.prefs.speed + 1);
    if (action === 'slower') this.prefs.speed = clampSpeed(this.prefs.speed - 1);
    if (action === 'larger' || action === 'smaller') {
      const fraction = toFraction(this.pos, this.max());
      this.prefs.font = clampFont(this.prefs.font + (action === 'larger' ? FONT_STEP : -FONT_STEP));
      this.applyPrefs();
      this.pos = fromFraction(fraction, this.max());
      this.scroller.scrollTop = this.pos;
    }
    this.savePrefs();
    this.renderControls();
    this.share();
  }

  private onKey(e: KeyboardEvent): void {
    if ((e.target as HTMLElement).closest('button') && (e.key === ' ' || e.key === 'Enter')) return; // the button's own action
    if (e.key === ' ' || e.code === 'Space') { e.preventDefault(); e.stopPropagation(); this.setPlaying(!this.playing); }
    else if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault(); e.stopPropagation();
      this.pos = nudge(this.pos, e.key === 'ArrowDown' ? 1 : -1, this.prefs.font, this.max());
      this.scroller.scrollTop = this.pos;
      this.share();
    } else if (e.key === 'Escape') { e.stopPropagation(); this.closeReader(); }
  }

  private max(): number { return maxScroll(this.scroller.scrollHeight, this.scroller.clientHeight); }

  private clampPos(): void { this.pos = Math.min(this.pos, this.max()); this.scroller.scrollTop = this.pos; }

  private setPlaying(on: boolean, shareIt = true): void {
    if (on && atEnd(this.pos, this.max()) && this.role() === 'host') { this.pos = 0; this.scroller.scrollTop = 0; }
    this.playing = on;
    if (on && this.raf === null) { this.lastFrame = performance.now(); this.raf = requestAnimationFrame(t => this.frame(t)); }
    if (!on && this.raf !== null) { cancelAnimationFrame(this.raf); this.raf = null; }
    this.renderControls();
    if (shareIt) this.share();
  }

  private frame(now: number): void {
    this.raf = null;
    if (!this.playing || this.reader.hidden) return;
    const max = this.max();
    this.pos = advance(this.pos, now - this.lastFrame, this.prefs.speed, this.prefs.font, max);
    this.lastFrame = now;
    this.scroller.scrollTop = this.pos;
    if (atEnd(this.pos, max)) { this.setPlaying(false); return; }
    this.share();
    this.raf = requestAnimationFrame(t => this.frame(t));
  }

  private setSharing(on: boolean): void {
    if (this.role() !== 'host') return;
    this.sharing = on;
    this.lastShared = null;
    if (!on) this.host.send({ t: TypePrompterScroll, payload: { position: toFraction(this.pos, this.max()), playing: false, speed: this.prefs.speed, shared: false } });
    this.renderControls();
    this.share();
  }

  /** A sharing host sends its position (throttled; state changes at once). */
  private share(): void {
    if (!this.sharing || this.role() !== 'host') return;
    const now = performance.now();
    const position = toFraction(this.pos, this.max());
    if (!shouldShare(this.lastShared, now, position, this.playing, this.prefs.speed)) {
      // Throttled: make sure the final position still goes out.
      if (this.trailing === null) this.trailing = window.setTimeout(() => { this.trailing = null; this.share(); }, SHARE_INTERVAL_MS);
      return;
    }
    this.lastShared = { at: now, position, playing: this.playing, speed: this.prefs.speed };
    this.host.send({ t: TypePrompterScroll, payload: { position, playing: this.playing, speed: this.prefs.speed, shared: true } });
  }
}
