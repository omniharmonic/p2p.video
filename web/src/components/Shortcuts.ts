import { matchShortcut, shortcutLabels, type ShortcutAction } from '../lib/shortcuts';

// Global call shortcuts plus the "?" help dialog. Keys are ignored while the
// user types in a field, and only act while `enabled()` (i.e. in the call).
export class Shortcuts {
  readonly dialog: HTMLDialogElement;
  private readonly isMac = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);
  private returnFocus: HTMLElement | null = null;

  constructor(private readonly opts: { enabled: () => boolean; run: (action: Exclude<ShortcutAction, 'help'>) => void }) {
    const d = document.createElement('dialog');
    d.id = 'shortcut-dialog';
    d.className = 'shortcut-dialog';
    d.setAttribute('aria-labelledby', 'shortcut-title');
    const rows = shortcutLabels(this.isMac)
      .map(s => `<div class="shortcut-row"><dt>${s.label}</dt><dd>${s.keys.map(k => `<kbd>${k}</kbd>`).join('<span aria-hidden="true">+</span>')}</dd></div>`)
      .join('');
    d.innerHTML = `
      <div class="call-panel-head"><h2 id="shortcut-title">Keyboard shortcuts</h2>
      <button type="button" class="icon-button" data-close aria-label="Close keyboard shortcuts">×</button></div>
      <dl class="shortcut-list">${rows}</dl>
      <p class="call-panel-note">Shortcuts don’t fire while you’re typing in a field.</p>`;
    d.querySelector<HTMLButtonElement>('[data-close]')!.onclick = () => d.close();
    d.addEventListener('click', e => { if (e.target === d) d.close(); });
    d.addEventListener('close', () => { this.returnFocus?.focus(); this.returnFocus = null; });
    document.body.append(d);
    this.dialog = d;

    document.addEventListener('keydown', e => {
      if (!this.opts.enabled() || d.open) return;
      const action = matchShortcut(e, this.isMac, e.target as HTMLElement | null);
      if (!action) return;
      e.preventDefault();
      if (action === 'help') this.showHelp();
      else this.opts.run(action);
    });
  }

  showHelp(): void {
    if (this.dialog.open) return;
    this.returnFocus = document.activeElement as HTMLElement | null;
    this.dialog.showModal();
    this.dialog.querySelector<HTMLButtonElement>('[data-close]')?.focus();
  }
}
