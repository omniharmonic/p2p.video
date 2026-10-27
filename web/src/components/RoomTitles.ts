// "Saved titles" for a room [SL-P5-4]: the lower thirds the room remembers by
// each person's display name and fills in when they join the next episode.
// Edit, forget, or add one before someone's first episode. User text is set
// with input values and textContent only.
import '../roomtitles.css';
import { LowerThirdNameMaxRunes, LowerThirdTitleMaxRunes } from '../lib/protocol';
import { ApiError, type RoomsApi, type RoomTitle, type RoomView } from '../lib/rooms';

function field(name: string, label: string, value: string, max: number, placeholder: string): HTMLInputElement {
  const i = document.createElement('input');
  i.type = 'text';
  i.dataset.field = name;
  i.value = value;
  i.maxLength = max;
  i.autocomplete = 'off';
  i.placeholder = placeholder;
  i.setAttribute('aria-label', label);
  return i;
}

function button(text: string, cls: string, attr: string, label?: string, type: 'button' | 'submit' = 'button'): HTMLButtonElement {
  const b = document.createElement('button');
  b.type = type;
  b.className = `btn ${cls} btn-compact`;
  b.textContent = text;
  b.dataset[attr] = '';
  if (label) b.setAttribute('aria-label', label);
  return b;
}

/** Opens the dialog; resolves when it closes. */
export function roomTitlesDialog(api: RoomsApi, room: RoomView, toast: (m: string) => void): Promise<void> {
  return new Promise(resolve => {
    const dialog = document.createElement('dialog');
    dialog.className = 'room-dialog room-titles-dialog';
    dialog.setAttribute('aria-labelledby', 'room-titles-title');
    dialog.innerHTML = `<h2 id="room-titles-title"></h2>
      <p>When someone joins this room with the same name, their lower third (name on screen and title) is filled in for them. Edits during a meeting update this list.</p>
      <div class="room-titles-list" role="list" aria-live="polite"></div>
      <form class="room-titles-add room-title-row"><strong>Add someone</strong></form>
      <p class="room-titles-error" role="alert" hidden></p>
      <form method="dialog" class="room-dialog-actions"><button class="btn btn-primary" value="ok">Done</button></form>`;
    dialog.querySelector('h2')!.textContent = `Saved titles · ${room.name}`;
    const list = dialog.querySelector<HTMLElement>('.room-titles-list')!;
    const error = dialog.querySelector<HTMLElement>('.room-titles-error')!;
    const add = dialog.querySelector<HTMLFormElement>('.room-titles-add')!;
    add.append(
      field('displayName', 'Their name as they join', '', LowerThirdNameMaxRunes, 'Name they join with'),
      field('name', 'Name on screen (optional)', '', LowerThirdNameMaxRunes, 'Name on screen (optional)'),
      field('title', 'Title', '', LowerThirdTitleMaxRunes, 'Title, e.g. Founder, Acme'),
      button('Add', 'btn-secondary', 'save', undefined, 'submit'),
    );
    const fail = (err: unknown) => {
      error.hidden = false;
      error.textContent = err instanceof ApiError ? err.message : 'Something went wrong. Try again.';
    };
    const render = (titles: RoomTitle[]) => {
      error.hidden = true;
      if (!titles.length) {
        const empty = document.createElement('p');
        empty.className = 'field-hint';
        empty.textContent = 'No saved titles yet. Set a lower third in Branding & titles during a meeting, or add one here.';
        list.replaceChildren(empty);
        return;
      }
      list.replaceChildren(...titles.map(t => {
        const row = document.createElement('form');
        row.className = 'room-title-row';
        row.setAttribute('role', 'listitem');
        row.dataset.key = t.key;
        row.dataset.displayName = t.displayName;
        const who = document.createElement('strong');
        who.textContent = t.displayName;
        row.append(who,
          field('name', `Name on screen for ${t.displayName}`, t.name ?? '', LowerThirdNameMaxRunes, t.displayName),
          field('title', `Title for ${t.displayName}`, t.title ?? '', LowerThirdTitleMaxRunes, 'Title'),
          button('Save', 'btn-secondary', 'save', `Save title for ${t.displayName}`, 'submit'),
          button('Forget', 'btn-quiet', 'forget', `Forget title for ${t.displayName}`));
        return row;
      }));
    };
    const value = (form: HTMLFormElement, name: string) => form.querySelector<HTMLInputElement>(`[data-field=${name}]`)?.value.trim() ?? '';
    dialog.addEventListener('submit', e => {
      const form = e.target as HTMLFormElement;
      if (form.method === 'dialog') return;
      e.preventDefault();
      const displayName = form === add ? value(form, 'displayName') : form.dataset.displayName || '';
      if (!displayName) { fail(new ApiError(400, 'Enter the name they join with.')); return; }
      const body = { displayName, name: value(form, 'name'), title: value(form, 'title') };
      void api.saveTitle(room.id, body).then(r => {
        render(r.titles);
        if (form === add) form.reset();
        toast(body.name || body.title ? `Saved for ${displayName}'s next episode.` : `Forgot ${displayName}'s title.`);
      }, fail);
    });
    list.addEventListener('click', e => {
      const row = (e.target as HTMLElement).closest('[data-forget]')?.closest<HTMLFormElement>('form');
      if (!row?.dataset.key) return;
      void api.forgetTitle(room.id, row.dataset.key).then(r => { render(r.titles); toast(`Forgot ${row.dataset.displayName}'s title.`); }, fail);
    });
    dialog.addEventListener('close', () => { dialog.remove(); resolve(); }, { once: true });
    document.body.appendChild(dialog);
    dialog.showModal();
    void api.titles(room.id).then(r => render(r.titles), fail);
  });
}
