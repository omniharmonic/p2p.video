// Product scenes for the p2p.video site [SL-REL-2].
//
// Starts a throwaway daemon (free port, temporary data dir) and three Chromium
// browsers, each with its own illustrated stand-in camera feed, then captures:
//   call-{light,dark}.png    host view: three people, recording
//   studio-{light,dark}.png  the same with the Studio panel open
//   people-{light,dark}.png  host view: People panel with someone waiting
//   phone-{light,dark}.png   guest view at 390 px while recording
// Run from web/ (it reuses web/tests/lib/harness.mjs):
//   STUDIO_BINARY=../bin/<daemon> PLAYWRIGHT_MODULE=... node ../site/scripts/scenes.mjs <feeds-dir> <out-dir>
// <feeds-dir> holds p0.y4m, p1.y4m, p2.y4m (capture-screenshots.sh makes them).
import {spawn} from 'node:child_process';
import {mkdtemp, rm, mkdir} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {freePort, isolatedNetworkArgs, launchURL, readyToJoin, startTake} from '../../web/tests/lib/harness.mjs';

const {chromium} = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const [feeds, out] = process.argv.slice(2).map(p => path.resolve(p));
if (!feeds || !out) throw new Error('usage: node scenes.mjs <feeds-dir> <out-dir>');
await mkdir(out, {recursive: true});

const wait = ms => new Promise(r => setTimeout(r, ms));
const data = await mkdtemp(path.join(tmpdir(), 'p2p-site-scenes-'));
const port = await freePort('tcp');
if (port === 7443) throw new Error('refusing to use port 7443');
const origin = `http://127.0.0.1:${port}`;
const daemon = spawn(process.env.STUDIO_BINARY, ['--data-dir', data, '--port', String(port), '--no-funnel', '--no-public-stun', ...await isolatedNetworkArgs()], {stdio: ['ignore', 'pipe', 'pipe']});
let logs = ''; daemon.stderr.on('data', b => logs += b); daemon.stdout.on('data', b => logs += b);
const browsers = [];
const DESKTOP = {width: 1280, height: 800}, PHONE = {width: 390, height: 844};

const launch = feed => chromium.launch({
  headless: true,
  executablePath: process.env.CHROMIUM_PATH,
  args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream', `--use-file-for-fake-video-capture=${path.join(feeds, feed)}`, '--autoplay-policy=no-user-gesture-required', '--mute-audio'],
}).then(b => { browsers.push(b); return b; });
const open = async (browser, viewport = DESKTOP) => {
  const context = await browser.newContext({permissions: ['camera', 'microphone'], viewport, deviceScaleFactor: 2, colorScheme: 'light', reducedMotion: 'reduce'});
  const page = await context.newPage(); page.setDefaultTimeout(30000);
  return page;
};
const shoot = async (page, name, viewport = DESKTOP) => {
  for (const scheme of ['light', 'dark']) {
    await page.emulateMedia({colorScheme: scheme}); await page.setViewportSize(viewport);
    await page.mouse.move(viewport.width / 2, 10);
    await wait(600);
    await page.screenshot({path: path.join(out, `${name}-${scheme}.png`), animations: 'disabled'});
  }
  await page.emulateMedia({colorScheme: 'light'});
};
const dismissToasts = page => page.waitForFunction(() => document.querySelectorAll('.toast').length === 0, null, {timeout: 12000}).catch(() => {});

try {
  let ready = false;
  for (let i = 0; i < 100 && !ready; i++) { try { ready = (await fetch(origin + '/health')).ok; } catch {} if (!ready) await wait(100); }
  if (!ready) throw new Error('daemon did not start: ' + logs);

  const host = await open(await launch('p0.y4m'));
  await host.goto(await launchURL(data, origin)); await readyToJoin(host);
  await host.locator('#name-input').fill('Ben');
  const created = host.waitForResponse(r => r.url().endsWith('/api/v1/sessions') && r.request().method() === 'POST');
  await host.locator('#join-btn').click();
  const session = await (await created).json();
  await host.locator('#studio-view').waitFor({state: 'visible'});
  await host.locator('#meeting-ready').waitFor({state: 'visible'});
  await host.locator('#meeting-ready [data-close]').click();

  const join = async (browser, name, viewport) => {
    const guest = await open(browser, viewport);
    await guest.goto(`${origin}/join?session=${session.id}&token=${session.token}`);
    await guest.waitForFunction(() => document.querySelector('#preview-video').videoWidth > 0);
    await guest.locator('#name-input').fill(name); await guest.locator('#join-btn').click();
    return guest;
  };
  const admit = async (browser, name, viewport) => {
    const guest = await join(browser, name, viewport);
    await host.waitForFunction(n => document.querySelector('#host-admission-banner').style.display === 'flex' && document.querySelector('#admission-guest-name').textContent === n, name);
    await host.locator('#admit-btn').click(); await guest.locator('#studio-view').waitFor({state: 'visible'});
    return guest;
  };
  const adaBrowser = await launch('p1.y4m');
  const ada = await admit(adaBrowser, 'Ada', PHONE);
  const kofi = await admit(await launch('p2.y4m'), 'Ruth');
  for (const p of [host, ada, kofi]) await p.waitForFunction(() => [...document.querySelectorAll('#video-grid .tile[data-participant-id]')].filter(t => t.querySelector('video')?.videoWidth > 0).length === 2, null, {timeout: 30000});
  await dismissToasts(host);

  // Someone waiting, People open.
  const lena = await join(adaBrowser, 'Lena');
  await host.waitForFunction(() => document.querySelector('#host-admission-banner').style.display === 'flex');
  await host.locator('#people-btn').click();
  await shoot(host, 'people');
  await host.locator('#deny-btn').click();
  await host.locator('#people-btn').click();
  await lena.context().close();

  // Recording with Studio open.
  await startTake(host);
  for (const p of [host, ada, kofi]) await p.waitForFunction(() => document.querySelector('#rec-indicator').style.display === 'inline-flex');
  await wait(6000);
  await dismissToasts(host); await dismissToasts(ada);
  await shoot(host, 'call');
  await host.locator('#studio-btn').click(); await host.locator('#studio-panel').waitFor({state: 'visible'});
  await shoot(host, 'studio');
  await shoot(ada, 'phone', PHONE);
  console.log('scenes written to', out);
} finally {
  for (const b of browsers) await b.close().catch(() => {});
  daemon.kill('SIGTERM');
  await new Promise(r => daemon.once('exit', r)).catch(() => {});
  await rm(data, {recursive: true, force: true}).catch(() => {});
}
