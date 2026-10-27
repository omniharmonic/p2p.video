// Rooms panel on the host home [SL-P1-5]: create, start, copy the guest
// link, rename, rotate or turn off the link, room defaults, archive, delete.
// Guest links contain their secret; they only ever go to the clipboard.
import { escapeHtml } from '../lib/html';
import { ApiError, RoomsApi, redactedLink, type RoomView } from '../lib/rooms';
import { roomTitlesDialog } from './RoomTitles';

export interface RoomsPanelOptions {
  api: RoomsApi;
  /** Start (or rejoin) a meeting in this room. */
  onStart: (room: RoomView) => void;
  /** Rooms changed (the home refreshes recents/desktop state). */
  onChange?: () => void;
  toast: (message: string) => void;
}

/** A modal confirm; resolves true on confirm. */
export function confirmDialog(title: string, body: string, confirmLabel: string, danger = false): Promise<boolean> {
  return new Promise(resolve => {
    const dialog = document.createElement('dialog');
    dialog.className = 'room-dialog';
    dialog.setAttribute('aria-labelledby', 'room-dialog-title');
    dialog.innerHTML = `<form method="dialog"><h2 id="room-dialog-title">${escapeHtml(title)}</h2><p>${escapeHtml(body)}</p>
      <div class="room-dialog-actions"><button value="cancel" class="btn btn-quiet">Cancel</button><button value="ok" class="btn ${danger ? 'btn-destructive' : 'btn-primary'}" data-confirm>${escapeHtml(confirmLabel)}</button></div></form>`;
    dialog.addEventListener('close', () => { resolve(dialog.returnValue === 'ok'); dialog.remove(); }, { once: true });
    document.body.appendChild(dialog);
    dialog.showModal();
    dialog.querySelector<HTMLButtonElement>('[data-confirm]')!.focus();
  });
}

/** A modal single-field prompt; resolves the trimmed value or null. */
export function promptDialog(title: string, label: string, value: string, confirmLabel: string): Promise<string | null> {
  return new Promise(resolve => {
    const dialog = document.createElement('dialog');
    dialog.className = 'room-dialog';
    dialog.setAttribute('aria-labelledby', 'room-dialog-title');
    dialog.innerHTML = `<form method="dialog"><h2 id="room-dialog-title">${escapeHtml(title)}</h2>
      <label class="field-label" for="room-dialog-input">${escapeHtml(label)}</label><input id="room-dialog-input" maxlength="80" autocomplete="off" required>
      <div class="room-dialog-actions"><button value="cancel" class="btn btn-quiet" formnovalidate>Cancel</button><button value="ok" class="btn btn-primary">${escapeHtml(confirmLabel)}</button></div></form>`;
    const input = dialog.querySelector('input')!;
    input.value = value;
    dialog.addEventListener('close', () => { resolve(dialog.returnValue === 'ok' && input.value.trim() ? input.value.trim() : null); dialog.remove(); }, { once: true });
    document.body.appendChild(dialog);
    dialog.showModal();
    input.select();
  });
}

export async function copyText(text: string): Promise<boolean> {
  try { await navigator.clipboard.writeText(text); return true; } catch { return false; }
}

function errorText(err: unknown): string {
  return err instanceof ApiError ? err.message : 'Something went wrong. Try again.';
}

export class RoomsPanel {
  readonly el: HTMLElement;
  private rooms: RoomView[] = [];
  private remote = true;
  private renderedHtml = '';
  private readonly list: HTMLElement;

  constructor(private readonly opts: RoomsPanelOptions) {
    this.el = document.createElement('section');
    this.el.className = 'home-panel rooms-panel';
    this.el.setAttribute('aria-labelledby', 'rooms-heading');
    this.el.innerHTML = `<div class="view-head"><div><h1 id="rooms-heading" class="view-title">Rooms</h1><p class="home-sub">A room keeps one link for a show or a team. Send it once; it works for every meeting.</p></div></div>
      <form class="room-create" autocomplete="off"><label class="visually-hidden" for="room-name-input">New room name</label>
        <input id="room-name-input" maxlength="80" placeholder="Name a room, e.g. The Podcast"><button class="btn btn-primary" id="room-create-btn">Create room</button></form>
      <p class="home-note" data-link-note hidden></p>
      <ul class="room-list" role="list"></ul>`;
    this.list = this.el.querySelector('.room-list')!;
    const form = this.el.querySelector<HTMLFormElement>('.room-create')!;
    form.addEventListener('submit', e => { e.preventDefault(); void this.create(); });
    this.list.addEventListener('click', e => {
      const btn = (e.target as HTMLElement).closest<HTMLButtonElement>('button[data-act]');
      const row = btn?.closest<HTMLElement>('[data-room-id]');
      const room = this.rooms.find(r => r.id === row?.dataset.roomId);
      if (btn && room) void this.act(btn.dataset.act!, room, btn);
    });
    // One open menu at a time; Escape or a click elsewhere closes it.
    document.addEventListener('click', e => {
      for (const d of this.list.querySelectorAll<HTMLDetailsElement>('details[open]')) if (!d.contains(e.target as Node)) d.open = false;
    });
    this.el.addEventListener('keydown', e => {
      if (e.key !== 'Escape') return;
      const open = this.list.querySelector<HTMLDetailsElement>('details[open]');
      if (open) { open.open = false; open.querySelector('summary')?.focus(); }
    });
  }

  render(rooms: RoomView[], guestLinksRemote: boolean): void {
    this.rooms = rooms;
    this.remote = guestLinksRemote;
    const note = this.el.querySelector<HTMLElement>('[data-link-note]')!;
    note.hidden = guestLinksRemote || !rooms.length;
    note.textContent = 'Room links use this Mac’s own address until a private or public Tailscale link is set up (see the Setup guide), so they only work on this Mac for now.';
    if (!rooms.length) {
      this.renderedHtml = '';
      this.list.innerHTML = `<li class="room-empty"><strong>No rooms yet.</strong><span>A room keeps one link that works for every episode or weekly call. Guests can bookmark it.</span></li>`;
      return;
    }
    // The home refreshes every few seconds: leave unchanged rows alone, and keep
    // an open ⋯ menu (and focus inside it) open when a row does change.
    const html = rooms.map(r => this.rowHtml(r)).join('');
    if (html === this.renderedHtml) return;
    this.renderedHtml = html;
    const open = this.list.querySelector<HTMLDetailsElement>('details[open]');
    const openId = open?.closest<HTMLElement>('[data-room-id]')?.dataset.roomId;
    const hadFocus = !!open && open.contains(document.activeElement);
    this.list.innerHTML = html;
    if (openId) {
      const again = this.list.querySelector<HTMLElement>(`[data-room-id="${CSS.escape(openId)}"]`)?.querySelector<HTMLDetailsElement>('details.room-menu');
      if (again) { again.open = true; if (hadFocus) again.querySelector('summary')?.focus(); }
    }
  }

  private rowHtml(r: RoomView): string {
    const state = r.live ? (r.hostInCall ? 'In a meeting' : 'Meeting open') : 'Ready';
    const policy = [r.settings.autoAdmit ? 'Guests join directly' : 'Guests wait to be let in', r.settings.lockOnStart ? 'Locks when it starts' : ''].filter(Boolean).map(p => `<span>${escapeHtml(p)}</span>`).join('');
    const link = r.linkEnabled && !r.linkLost ? `<code>${escapeHtml(redactedLink(r))}</code>` : `<span class="room-link-off">${r.linkLost ? 'Link needs a new address' : 'Room link off'}</span>`;
    const canCopy = r.linkEnabled && !r.linkLost && !!r.guestUrl;
    return `<li class="room-row${r.live ? ' is-live' : ''}" data-room-id="${escapeHtml(r.id)}">
      <div class="room-main"><div class="room-title"><span class="room-dot" aria-hidden="true"></span><strong>${escapeHtml(r.name)}</strong><span class="badge${r.live ? ' badge-success' : ''}">${state}</span></div>
        <div class="room-meta">${link}${policy}</div></div>
      <div class="room-actions">
        <button class="btn btn-primary" data-act="start" aria-label="${r.live ? 'Rejoin' : 'Start'} ${escapeHtml(r.name)}">${r.live ? 'Rejoin' : 'Start'}</button>
        ${canCopy ? `<button class="btn btn-secondary" data-act="copy" aria-label="Copy room link for ${escapeHtml(r.name)}">Copy link</button>` : `<button class="btn btn-secondary" data-act="rotate" aria-label="Create a room link for ${escapeHtml(r.name)}">New link</button>`}
        <details class="room-menu"><summary class="icon-button" aria-label="More actions for ${escapeHtml(r.name)}">⋯</summary>
          <div class="room-menu-list" role="group" aria-label="${escapeHtml(r.name)} actions">
            <button data-act="rename">Rename</button>
            <button data-act="rotate">Replace room link</button>
            <button data-act="producer">Copy producer link</button>
            <button data-act="titles">Saved titles…</button>
            ${r.linkEnabled ? '<button data-act="revoke">Turn off room link</button>' : ''}
            <button data-act="auto" aria-pressed="${r.settings.autoAdmit}">Let guests in without waiting</button>
            <button data-act="lock" aria-pressed="${r.settings.lockOnStart}">Lock each meeting on start</button>
            <button data-act="archive">Archive</button>
            <button data-act="delete" class="danger">Delete…</button>
          </div></details>
      </div></li>`;
  }

  /** New, then Create a room: focuses the name field. */
  focusCreate(): void { const i = this.el.querySelector<HTMLInputElement>("#room-name-input")!; i.focus(); }

  private async create(): Promise<void> {
    const input = this.el.querySelector<HTMLInputElement>('#room-name-input')!;
    const name = input.value.trim();
    if (!name) { input.focus(); this.opts.toast('Give the room a name first'); return; }
    const btn = this.el.querySelector<HTMLButtonElement>('#room-create-btn')!;
    btn.disabled = true;
    try {
      const room = await this.opts.api.create(name);
      input.value = '';
      this.rooms = [...this.rooms, room].sort((a, b) => a.name.localeCompare(b.name));
      this.render(this.rooms, room.guestUrlRemote);
      this.opts.toast(`Room “${room.name}” created. Copy its link any time.`);
      this.opts.onChange?.();
    } catch (err) { this.opts.toast(errorText(err)); }
    finally { btn.disabled = false; }
  }

  private replace(room: RoomView): void {
    this.rooms = this.rooms.map(r => r.id === room.id ? room : r);
    this.render(this.rooms, this.remote);
  }

  private async act(action: string, room: RoomView, btn: HTMLButtonElement): Promise<void> {
    btn.closest('details')?.removeAttribute('open');
    const { api, toast } = this.opts;
    try {
      switch (action) {
        case 'start': this.opts.onStart(room); return;
        case 'copy':
          if (room.guestUrl && await copyText(room.guestUrl)) toast(`Room link for “${room.name}” copied. It works for every meeting in this room.`);
          else promptDialog('Copy the room link', 'Room link', room.guestUrl || '', 'Done');
          return;
        case 'producer': {
          // Before the episode, like the guest link [SL-QA-1]: the room keeps one
          // producer link for every meeting; issue it on first use.
          const view = room.producer?.enabled && room.producer.url ? room : await api.producerRotate(room.id);
          if (view !== room) this.replace(view);
          const url = view.producer?.url || '';
          if (url && await copyText(url)) toast(`Producer link for “${room.name}” copied. It works for every meeting in this room; you admit the producer when they join.`);
          else promptDialog('Copy the producer link', 'Producer link', url, 'Done');
          return;
        }
        case 'titles':
          await roomTitlesDialog(api, room, toast);
          return;
        case 'rename': {
          const name = await promptDialog('Rename room', 'Room name', room.name, 'Save');
          if (name && name !== room.name) { this.replace(await api.update(room.id, { name })); toast('Room renamed. The room link is unchanged.'); }
          break;
        }
        case 'rotate': {
          if (room.linkEnabled && !room.linkLost && !await confirmDialog('Replace the room link?', `The current link for “${room.name}” stops working immediately, including for anyone who bookmarked it. Send the new link to your guests.`, 'Replace link')) return;
          const next = await api.rotate(room.id);
          this.replace(next);
          if (next.guestUrl && await copyText(next.guestUrl)) toast('New room link copied. The old link no longer works.');
          else toast('New room link ready. The old link no longer works.');
          break;
        }
        case 'revoke':
          if (!await confirmDialog('Turn off the room link?', `Nobody can use the link for “${room.name}” until you create a new one.`, 'Turn off link', true)) return;
          this.replace(await api.revoke(room.id));
          toast('Room link turned off.');
          break;
        case 'auto':
          this.replace(await api.update(room.id, { settings: { autoAdmit: !room.settings.autoAdmit } }));
          toast(!room.settings.autoAdmit ? 'Guests with the link will join without waiting.' : 'Guests will wait for you to admit them.');
          break;
        case 'lock':
          this.replace(await api.update(room.id, { settings: { lockOnStart: !room.settings.lockOnStart } }));
          break;
        case 'archive':
          if (!await confirmDialog('Archive this room?', `“${room.name}” is hidden and its link stops working. Past recordings stay.`, 'Archive')) return;
          await api.update(room.id, { archived: true });
          this.rooms = this.rooms.filter(r => r.id !== room.id);
          this.render(this.rooms, this.remote);
          toast('Room archived.');
          break;
        case 'delete':
          if (!await confirmDialog('Delete this room?', `“${room.name}” and its link are removed for good. Recordings from past meetings stay on this Mac.`, 'Delete room', true)) return;
          await api.remove(room.id);
          this.rooms = this.rooms.filter(r => r.id !== room.id);
          this.render(this.rooms, this.remote);
          toast('Room deleted.');
          break;
      }
      this.opts.onChange?.();
    } catch (err) { toast(errorText(err)); }
  }
}
