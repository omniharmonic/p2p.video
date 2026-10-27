/** Guest-side diagnostics. The host doctor cannot inspect another device's VPN or DNS. */
export function privateConnectionHelp(): string {
  return `<details class="setup-note connection-help">
    <summary>The invitation won’t open</summary>
    <p>Check these on the guest’s device before trying the studio link again.</p>
    <ol>
      <li><strong>Check the account and machine share.</strong> Tailscale must say Connected under the account that accepted the host’s machine share. The host should appear in its device list.</li>
      <li><strong>If Safari cannot find the server:</strong> enable Use Tailscale DNS Settings in the Tailscale app. Check that MagicDNS is enabled in the receiving account’s DNS settings, then reconnect Tailscale. Use the full HTTPS hostname from the invitation.</li>
      <li><strong>If the page times out:</strong> ask the host to check that their Mac is awake, p2p.video is running, and the machine share and access rules still allow this guest.</li>
    </ol>
    <p>Keep the HTTPS hostname. Don’t replace it with a bare IP address or bypass a certificate warning.</p>
    <div class="setup-links"><a href="https://login.tailscale.com/admin/dns" target="_blank" rel="noopener noreferrer">Guest’s Tailscale DNS settings ↗</a><a href="https://tailscale.com/docs/features/sharing#sharing-and-magicdns" target="_blank" rel="noopener noreferrer">Shared-device DNS help ↗</a></div>
  </details>`;
}
