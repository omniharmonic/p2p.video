import { escapeHtml as h } from '../lib/html';
import { privateConnectionHelp } from './PrivateConnectionHelp';

export interface WizardCallbacks {
  onComplete: () => void;
  onRequestPermissions: () => Promise<boolean>;
}

type StudioRole = 'host' | 'guest';
type GuestAccess = 'own-device' | 'another-person';

/** Setup explains two independent invitations: network access and studio admission. */
export class OnboardingWizard {
  private container: HTMLElement;
  private currentStep = 1;
  private role: StudioRole = 'host';
  private guestAccess: GuestAccess = 'another-person';
  private callbacks: WizardCallbacks;
  private tailscaleStatus: { online: boolean; dnsName?: string } | null = null;
  private guestUrl = '';
  private serveReady = false;
  private hostPort = 7443;
  private checking = false;

  constructor(callbacks: WizardCallbacks) {
    this.callbacks = callbacks;
    this.container = document.createElement('div');
    this.container.id = 'onboarding-wizard-overlay';
    this.container.className = 'wizard-overlay';
    this.container.setAttribute('role', 'dialog');
    this.container.setAttribute('aria-modal', 'true');
    this.container.setAttribute('aria-labelledby', 'wiz-title');
    this.render();
  }

  public show(): void {
    this.currentStep = 1;
    document.body.appendChild(this.container);
    this.container.style.display = 'flex';
    this.render();
    void this.checkTailscaleStatus();
  }

  public hide(): void { this.container.remove(); }

  private async checkTailscaleStatus(): Promise<void> {
    this.checking = true;
    this.render();
    try {
      this.tailscaleStatus = null;
      this.serveReady = false;
      this.guestUrl = '';
      const res = await fetch('/api/v1/doctor');
      if (res.ok) {
        const data = await res.json();
        this.tailscaleStatus = { online: data.tailscale?.online === true, dnsName: data.tailscale?.dns_name };
        this.guestUrl = typeof data.connection?.guestUrl === 'string' ? data.connection.guestUrl : '';
        this.serveReady = data.connection?.privateServeReady === true;
        const port = Number(data.connection?.port);
        this.hostPort = Number.isInteger(port) && port > 0 && port <= 65535 ? port : 7443;
      }
    } catch { this.tailscaleStatus = null; }
    this.checking = false;
    if (this.container.isConnected) this.render();
  }

  private render(): void {
    this.container.innerHTML = `
      <div class="wizard-card glass-panel">
        <div class="wizard-header">
          <span class="brand-badge">Setup guide</span>
          <span class="setup-stage">${this.currentStep === 1 ? 'Step 1 of 3: You' : this.currentStep === 2 ? 'Step 2 of 3: Letting people in' : 'Step 3 of 3: Camera and mic'}</span>
          <button class="icon-button" id="wiz-close-btn" aria-label="Close setup">×</button>
        </div>
        <div class="wizard-body" id="wizard-body-content">${this.getStepHtml()}</div>
        <div class="wizard-footer">
          ${this.currentStep > 1 ? '<button class="btn btn-secondary" id="wiz-prev-btn">Back</button>' : '<span></span>'}
          <button class="btn btn-primary" id="wiz-next-btn">${this.currentStep === 3 ? 'Check camera and mic' : 'Continue'}</button>
        </div>
      </div>`;
    this.attachEvents();
  }

  private attachEvents(): void {
    this.container.querySelector('#wiz-close-btn')?.addEventListener('click', () => this.hide());
    this.container.querySelector('#wiz-prev-btn')?.addEventListener('click', () => { this.currentStep--; this.render(); });
    this.container.querySelector('#wiz-next-btn')?.addEventListener('click', async () => {
      if (this.currentStep < 3) { this.currentStep++; this.render(); return; }
      const next = this.container.querySelector<HTMLButtonElement>('#wiz-next-btn');
      if (next) { next.disabled = true; next.textContent = 'Checking your camera and mic…'; }
      try {
        if (await this.callbacks.onRequestPermissions()) { this.hide(); this.callbacks.onComplete(); return; }
        this.permissionError();
      } catch { this.permissionError(); }
    });
    this.container.querySelectorAll<HTMLButtonElement>('[data-role]').forEach(button => {
      button.addEventListener('click', () => { this.role = button.dataset.role as StudioRole; this.render(); });
    });
    this.container.querySelectorAll<HTMLButtonElement>('[data-access]').forEach(button => {
      button.addEventListener('click', () => { this.guestAccess = button.dataset.access as GuestAccess; this.render(); });
    });
    this.container.querySelector('#wiz-recheck-network')?.addEventListener('click', () => void this.checkTailscaleStatus());
    this.container.querySelector('#wiz-copy-serve')?.addEventListener('click', async () => {
      const result = this.container.querySelector('#wiz-copy-result');
      try { await navigator.clipboard.writeText(`TAILSCALE_BE_CLI=1 "$(command -v tailscale || printf /Applications/Tailscale.app/Contents/MacOS/Tailscale)" serve --bg http://127.0.0.1:${this.hostPort}`); if (result) result.textContent = 'Command copied. Run it in Terminal on this Mac.'; }
      catch { if (result) result.textContent = 'Select and copy the command above.'; }
    });
  }

  private permissionError(): void {
    const badge = this.container.querySelector('#perm-status-badge');
    if (badge) badge.textContent = 'Your camera or microphone isn’t available. Allow them for this site in your browser, check they’re plugged in, then try again.';
    const next = this.container.querySelector<HTMLButtonElement>('#wiz-next-btn');
    if (next) { next.disabled = false; next.textContent = 'Try again'; }
  }

  private getStepHtml(): string {
    if (this.currentStep === 1) return `
      <h3 class="wiz-title" id="wiz-title">How are you using p2p.video?</h3>
      <p class="wiz-subtitle">The host’s Mac keeps the recordings. Everyone else joins from a browser.</p>
      <div class="setup-choices">
        <button class="setup-choice ${this.role === 'host' ? 'selected' : ''}" data-role="host" aria-pressed="${this.role === 'host'}">
          <strong>Set up my studio</strong><span>p2p.video runs on my Mac, and I invite people to meetings.</span>
        </button>
        <button class="setup-choice ${this.role === 'guest' ? 'selected' : ''}" data-role="guest" aria-pressed="${this.role === 'guest'}">
          <strong>Join someone’s studio</strong><span>Someone sent me a link. I don’t need to install p2p.video.</span>
        </button>
      </div>
      <p class="connection-note">Each person is recorded on their own device, then the files go to the host. Keep the browser open until the host has them.</p>`;

    if (this.currentStep === 2) return this.role === 'host' ? this.hostAccessHtml() : this.guestAccessHtml();

    return `<h3 class="wiz-title" id="wiz-title">Choose your camera and microphone</h3>
      <p class="wiz-subtitle">Your browser asks for permission. You can change them before you join a meeting.</p>
      <div class="setup-note"><strong>Wear headphones</strong><p>They keep other people’s voices out of your recording.</p></div>
      <p id="perm-status-badge" role="status" class="connection-note">Nothing is recorded during this check.</p>
      <p class="connection-note">Before the real thing, record a short test together and check that everyone’s recording reaches the host. When you edit, check that sound and picture line up at the start and the end.</p>`;
  }

  private hostAccessHtml(): string {
    const ready = this.tailscaleStatus?.online === true && this.serveReady;
    const status = this.checking ? 'Checking this Mac…' : ready ? 'This Mac is ready for private links' : this.tailscaleStatus?.online ? 'This Mac is online, but private links need setting up' : 'Couldn’t check this Mac’s connection';
    return `<h3 class="wiz-title" id="wiz-title">Give your guest private access</h3>
      <div class="status-box ${ready ? 'status-box-online' : 'status-box-warn'}" role="status">
        <span class="status-indicator-dot ${ready ? 'dot-online' : 'dot-warn'}"></span><div><strong>${status}</strong>
        ${this.tailscaleStatus?.dnsName ? `<div class="status-meta">${h(this.tailscaleStatus.dnsName)}</div>` : ''}
        <div class="status-meta">This checks the host only. It does not prove your guest is connected.</div></div>
      </div>
      ${!ready ? `<div class="setup-note"><strong>Prepare this Mac</strong><p>Install Tailscale, sign in, and keep it connected. Then run this command on the Mac running p2p.video, and approve HTTPS if it asks.</p><code>tailscale serve --bg http://127.0.0.1:${this.hostPort}</code><div class="setup-links"><a href="https://tailscale.com/download" target="_blank" rel="noopener noreferrer">Install Tailscale ↗</a><button class="btn btn-secondary" id="wiz-copy-serve">Copy command</button></div><p id="wiz-copy-result" role="status"></p></div>` : ''}
      <div class="setup-segment" aria-label="Who is joining?">
        <button data-access="another-person" aria-pressed="${this.guestAccess === 'another-person'}">Another person</button>
        <button data-access="own-device" aria-pressed="${this.guestAccess === 'own-device'}">My second device</button>
      </div>
      ${this.guestAccess === 'another-person' ? `<div class="setup-note"><strong>Share this host with their Tailscale account</strong><p>Open Machines, find this host${this.tailscaleStatus?.dnsName ? ` (${h(this.tailscaleStatus.dnsName)})` : ''}, and choose Share. Send the machine-sharing link to your guest. They sign in with their own account and accept it. Your Tailscale administrator may need to allow sharing.</p><a class="btn btn-secondary" href="https://login.tailscale.com/admin/machines" target="_blank" rel="noopener noreferrer">Open Tailscale Machines ↗</a></div>` : `<div class="setup-note"><strong>Connect your second device</strong><p>Install and open Tailscale on that device. Sign in with the same account as the host. You don’t need a machine-sharing invitation.</p><a href="https://tailscale.com/download" target="_blank" rel="noopener noreferrer">Install Tailscale ↗</a></div>`}
      <div class="setup-note"><strong>Then send the meeting link</strong><p>In a meeting, choose Add others and copy the link. Your guest opens it with Tailscale connected, checks their camera and mic, and asks to join. Let them in when their request appears.</p></div>
      <div class="setup-links"><button class="btn btn-secondary" id="wiz-recheck-network" ${this.checking ? 'disabled' : ''}>Check again</button>${this.guestUrl ? `<span class="connection-note">Private link: ${h(this.guestUrl)}</span>` : ''}</div>
      ${privateConnectionHelp()}
      <p class="connection-note">Reaching this Mac and being let into a meeting are two separate steps. Neither link contains your Tailscale password or account details. Never send an auth key.</p>`;
  }

  private guestAccessHtml(): string {
    return `<h3 class="wiz-title" id="wiz-title">Join with your own account</h3>
      <div class="setup-note"><strong>Connect Tailscale on this device</strong><p>Install Tailscale, sign in with your own account, and leave it connected. You don’t need the host’s password or a Tailscale auth key.</p><a class="btn btn-secondary" href="https://tailscale.com/download" target="_blank" rel="noopener noreferrer">Install Tailscale ↗</a></div>
      <div class="setup-note"><strong>Accept the host’s machine share</strong><p>Ask the host for their Tailscale machine-sharing link. Open it and accept access to their host. Skip this only if this device already uses the host’s Tailscale account.</p></div>
      <div class="setup-note"><strong>Open the meeting link</strong><p>Open the meeting link the host sent. Check your camera and mic, ask to join, and wait for the host to let you in. A page that won’t open usually needs Tailscale access first.</p></div>
      ${privateConnectionHelp()}
      <p class="connection-note">This page can’t see your Tailscale app. The host’s Mac being ready doesn’t mean your device can reach it yet. Once you’re in, check you can see and hear each other before recording.</p>`;
  }
}
