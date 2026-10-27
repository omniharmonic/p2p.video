// Shared helpers for the fake-device browser tests.
import net from 'node:net';
import {readFile} from 'node:fs/promises';
import path from 'node:path';

/**
 * The host dashboard's sign-in URL [SL-SEC-1]: the daemon writes a per-boot
 * launch token to <data-dir>/.launch_token (0600) and sets the admin cookie
 * when a page is opened with ?launch=<token>, exactly as the Mac app does.
 * `extra` adds query parameters (e.g. {room: id}).
 */
export async function launchURL(dataDir, origin, extra = {}) {
  const token = (await readFile(path.join(dataDir, '.launch_token'), 'utf8')).trim();
  return `${origin}/?${new URLSearchParams({...extra, launch: token})}`;
}

/** Node-side management requests (create session, rooms): the desktop token. */
export async function adminHeaders(dataDir) {
  const token = (await readFile(path.join(dataDir, '.desktop_token'), 'utf8')).trim();
  return {Authorization: `Bearer ${token}`};
}

export const freePort = type => new Promise((resolve, reject) => {
  if (type === 'udp') {
    import('node:dgram').then(({ createSocket }) => {
      const s = createSocket('udp4');
      s.on('error', reject);
      s.bind(0, '127.0.0.1', () => { const p = s.address().port; s.close(() => resolve(p)); });
    });
    return;
  }
  const s = net.createServer();
  s.on('error', reject);
  s.listen(0, '127.0.0.1', () => { const p = s.address().port; s.close(() => resolve(p)); });
});

/**
 * Daemon flags that keep a test off machine-wide defaults (SFU UDP mux 50000,
 * TURN 127.0.0.1:3478), so concurrent test runs never share sockets.
 */
export async function isolatedNetworkArgs() {
  return ['--udp-port', String(await freePort('udp')), '--turn-listen', `127.0.0.1:${await freePort('tcp')}`];
}

/**
 * Host presses Record. Red pre-flight items open a confirm [SL-P0-5]; headless
 * contexts are incognito with only a few GB of quota, so a 2 h take honestly
 * doesn't fit and tests choose "Record anyway". Returns the listed problems.
 */
export async function startTake(host, { timeout = 15000, expectRecording = true } = {}) {
  await host.locator('#rec-btn').click();
  // With expectRecording false (injected capture failures) only a confirm is
  // awaited, for as long as the pre-flight re-check can take.
  const outcome = await host.waitForFunction(expect => document.querySelector('#preflight-confirm') ? 'confirm'
    : expect && document.querySelector('#rec-indicator')?.style.display === 'inline-flex' ? 'other' : null,
  expectRecording, { timeout: expectRecording ? timeout : 5000 }).catch(() => null);
  if (!outcome && expectRecording) throw new Error('Record did not start or ask for pre-flight confirmation');
  if (!outcome || await outcome.jsonValue() !== 'confirm') return '';
  const problems = await host.locator('#preflight-confirm .preflight-problems').innerText();
  await host.locator('#preflight-confirm-record').click();
  return problems;
}

/**
 * Secondary call controls live behind a panel [SL-UX-1, SL-UX-2]: recording,
 * uploads, export, livestream, teleprompter, branding, phone camera and
 * settings in the Studio panel (✦); per-person connection in People; layout,
 * devices, background, shortcuts and setup guide in "More options" (⋯).
 * Opens whichever holds the target when it isn't already visible and returns
 * its locator, ready to click.
 */
export async function revealControl(page, target) {
  const control = typeof target === 'string' ? page.locator(target) : target;
  if (await control.isVisible().catch(() => false)) return control;
  const where = await control.evaluate(el => el.closest('#studio-panel') ? 'studio' : el.closest('#call-panel') ? 'people' : 'more').catch(() => 'more');
  if (where === 'studio') {
    if (await page.locator('#studio-panel').isHidden()) await page.locator('#studio-btn').click();
    return control;
  }
  if (where === 'people') {
    if (await page.locator('#people-btn').getAttribute('aria-expanded') !== 'true') {
      if (await page.locator('#people-btn').isVisible()) await page.locator('#people-btn').click();
      else await page.locator('#call-avatars').click();
    }
    return control;
  }
  const more = page.locator('#more-btn');
  if (await more.isVisible() && await page.locator('#more-panel').isHidden()) await more.click();
  return control;
}

/**
 * The host home [SL-UX-2a] opens Ready to join (#green-room) from New, then
 * "Start a meeting now". A no-op when the page is not on the home (guests,
 * session links, ?room= quick opens), so it is safe to call after any goto.
 */
export async function readyToJoin(page, {camera = true} = {}) {
  const home = page.locator('#home-view');
  if (!(await home.isVisible().catch(() => false))) return;
  await page.locator('#home-new-btn').click();
  await page.locator('#home-new-menu [data-new="now"]').click();
  await page.locator('#green-room').waitFor({state: 'visible'});
  // The host's camera opens on Ready to join, not on the home [SL-UX-2].
  if (camera) await page.waitForFunction(() => document.querySelector('#preview-video')?.videoWidth > 0);
}

/** Ready to join keeps the recording options (mic mode, studio-quality audio) folded. */
export async function openRecordingOptions(page) {
  const options = page.locator('#recording-options');
  if (!(await options.evaluate(d => d.open).catch(() => true))) await options.locator('summary').click();
}
