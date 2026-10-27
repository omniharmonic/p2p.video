// Host: invite a producer [SL-P1-11]. The producer link is separate from the
// guest link and rotatable; for a meeting opened from a room it is the room's
// link (reused every episode). A producer still waits for the host to admit
// them, is heard in the call (camera optional) and is never recorded.
import type { ProducerLinkResponse } from '../lib/protocol';

async function call(sessionId: string, token: string, action: '' | 'rotate' | 'revoke'): Promise<ProducerLinkResponse> {
  const path = `/api/v1/sessions/${encodeURIComponent(sessionId)}/producer${action ? `/${action}` : ''}`;
  const res = await fetch(path, { method: action ? 'POST' : 'GET', headers: { Authorization: `Bearer ${token}` } });
  if (!res.ok) throw new Error((await res.text()) || `producer link ${res.status}`);
  return res.json();
}

export async function showProducerLinkDialog(sessionId: string, token: string, toast: (m: string) => void): Promise<void> {
  let link: ProducerLinkResponse;
  try {
    link = await call(sessionId, token, '');
    if (!link.enabled) link = await call(sessionId, token, 'rotate');
  } catch (err) {
    toast(`Couldn’t get a producer link: ${err instanceof Error ? err.message : String(err)}`);
    return;
  }
  const dialog = document.createElement('dialog');
  dialog.className = 'invite-dialog producer-dialog';
  dialog.id = 'producer-dialog';
  dialog.setAttribute('aria-labelledby', 'producer-heading');
  dialog.innerHTML = `<div class="modal-header"><h2 id="producer-heading">Invite a producer</h2><button class="icon-button" aria-label="Close producer invitation">×</button></div>
    <p>A producer joins the call to help run it: they are heard (camera optional) but <strong>never recorded</strong> and not in exports or captions. They can admit or decline waiting guests, start and stop takes, mute and remove guests and see source health. They can’t end the meeting, change links or settings, livestream, or act on you.</p>
    <div class="setup-note"><label class="field-label" for="producer-link-value">Producer link</label><textarea id="producer-link-value" readonly rows="3" spellcheck="false"></textarea>
      <div class="setup-links"><button class="btn btn-primary" data-copy>Copy producer link</button><button class="btn btn-secondary" data-rotate>New link</button><button class="btn btn-quiet" data-revoke>Turn off</button></div>
      <p data-result role="status"></p>
    </div>
    <p class="connection-note" data-scope></p>
    <p class="connection-note">You still admit the producer when they ask to join. Keep this link private; a new link stops the old one at once.</p>`;
  const field = dialog.querySelector('textarea')!;
  const status = dialog.querySelector<HTMLElement>('[data-result]')!;
  const render = () => {
    field.value = link.enabled ? link.url ?? '' : '';
    field.placeholder = link.enabled ? '' : 'Producer link is off';
    dialog.querySelector<HTMLButtonElement>('[data-copy]')!.disabled = !link.enabled;
    dialog.querySelector<HTMLButtonElement>('[data-revoke]')!.disabled = !link.enabled;
    dialog.querySelector('[data-scope]')!.textContent = (link.roomId ? 'This is the room’s producer link: it works for every meeting in this room. ' : 'This link works for this meeting only. ')
      + (link.remote ? '' : 'This address only works on this machine: configure Tailscale Serve/Funnel for a remote producer.');
  };
  const act = async (action: 'rotate' | 'revoke') => {
    try { link = await call(sessionId, token, action); status.textContent = action === 'rotate' ? 'New producer link issued; the old one no longer works.' : 'Producer link turned off.'; }
    catch (err) { status.textContent = `Failed: ${err instanceof Error ? err.message : String(err)}`; }
    render();
  };
  dialog.querySelector('[aria-label="Close producer invitation"]')!.addEventListener('click', () => dialog.close());
  dialog.querySelector('[data-rotate]')!.addEventListener('click', () => void act('rotate'));
  dialog.querySelector('[data-revoke]')!.addEventListener('click', () => void act('revoke'));
  dialog.querySelector('[data-copy]')!.addEventListener('click', async () => {
    try { await navigator.clipboard.writeText(link.url ?? ''); status.textContent = 'Producer link copied.'; }
    catch { field.focus(); field.select(); status.textContent = 'Select and copy the link above.'; }
  });
  dialog.addEventListener('close', () => dialog.remove(), { once: true });
  render();
  document.body.appendChild(dialog);
  dialog.showModal();
}
