import { privateConnectionHelp } from './PrivateConnectionHelp';

export function showInviteDialog(inviteUrl: string): void {
  const dialog = document.createElement('dialog');
  dialog.className = 'invite-dialog';
  dialog.setAttribute('aria-labelledby', 'invite-heading');
  dialog.innerHTML = `<div class="modal-header"><h2 id="invite-heading">Add others</h2><button class="icon-button" aria-label="Close invitation">×</button></div>
    <p>Send the meeting link. People who use it must get your permission before they can join. Guests reach your computer privately through Tailscale.</p>
    <div class="setup-segment" aria-label="Who is joining?">
      <button data-invite-access="person" aria-pressed="true">Another person</button>
      <button data-invite-access="device" aria-pressed="false">My second device</button>
    </div>
    <div class="setup-note" data-access-instructions></div>
    <div class="setup-note"><strong>Send the meeting link</strong><p>Once they can reach this computer, they open the link, check their camera and ask to join. You let them in from the call.</p>
      <label class="field-label" for="guest-invite-value">Meeting link</label><textarea id="guest-invite-value" readonly rows="3" spellcheck="false"></textarea>
      <button class="btn btn-primary" data-copy>Copy meeting link</button><p data-result role="status"></p>
    </div>
    ${privateConnectionHelp()}
    <p class="connection-note">Before you record, check that you can see and hear each other. After you stop, keep every tab open until each recording reaches this computer.</p>
    <p class="connection-note">The link doesn’t include any Tailscale sign-in. Keep it private, and never send a password or auth key.</p>`;
  const field = dialog.querySelector('textarea')!;
  field.value = inviteUrl;
  function selectAccess(access: string): void {
    dialog.querySelectorAll<HTMLButtonElement>('[data-invite-access]').forEach(button => button.setAttribute('aria-pressed', String(button.dataset.inviteAccess === access)));
    dialog.querySelector('[data-access-instructions]')!.innerHTML = access === 'device'
      ? `<strong>Connect your second device</strong><p>Install and open Tailscale on that device, using the same account as this host. No machine share is needed.</p><a href="https://tailscale.com/download" target="_blank" rel="noopener noreferrer">Install Tailscale ↗</a>`
      : `<strong>Share this host in Tailscale first</strong><p>Open Machines, find the p2p.video host, and choose Share. Send that machine-sharing link separately. Your guest installs Tailscale, signs in with their own account, and accepts it. An administrator may need to allow sharing.</p><div class="setup-links"><a href="https://login.tailscale.com/admin/machines" target="_blank" rel="noopener noreferrer">Open Tailscale Machines ↗</a><a href="https://tailscale.com/download" target="_blank" rel="noopener noreferrer">Guest: install Tailscale ↗</a></div>`;
  }
  selectAccess('person');
  dialog.querySelectorAll<HTMLButtonElement>('[data-invite-access]').forEach(button => button.addEventListener('click', () => selectAccess(button.dataset.inviteAccess!)));
  dialog.querySelector('[aria-label="Close invitation"]')!.addEventListener('click', () => dialog.close());
  dialog.querySelector('[data-copy]')!.addEventListener('click', async () => {
    const status = dialog.querySelector('[data-result]')!;
    try { await navigator.clipboard.writeText(inviteUrl); status.textContent = 'Meeting link copied.'; }
    catch { field.focus(); field.select(); status.textContent = 'Select and copy the link above.'; }
  });
  dialog.addEventListener('close', () => dialog.remove(), { once: true });
  document.body.appendChild(dialog);
  dialog.showModal();
}
