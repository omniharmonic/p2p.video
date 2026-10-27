// UX screenshots [SL-UX-2a]: the home (empty and with data in each rail view),
// the New menu, the schedule dialog, Ready to join (host, room guest, producer
// excluded), waiting for the host, asking to join, and the solo record page,
// in light and dark at 1280×800 and 390×844. Writes PNGs to
// STUDIO_SCREENSHOT_DIR (default: a temp dir). Also checks there is no horizontal
// overflow and that keyboard focus is visible on the home's first controls.
// Build first; STUDIO_BINARY is the daemon.
import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {mkdtemp, rm, mkdir} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {adminHeaders, freePort, isolatedNetworkArgs, launchURL} from './lib/harness.mjs';
const {chromium} = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');

const out = process.env.STUDIO_SCREENSHOT_DIR || await mkdtemp(path.join(tmpdir(), 'studio-ux-screens-'));
await mkdir(out, {recursive: true});
const data = await mkdtemp(path.join(tmpdir(), 'studio-ux-'));
const port = Number(process.env.STUDIO_TEST_PORT || await freePort('tcp')), origin = `http://127.0.0.1:${port}`;
const daemon = spawn(process.env.STUDIO_BINARY || '../bin/p2pvideod', ['--data-dir', data, '--port', String(port), '--no-funnel', ...await isolatedNetworkArgs()], {stdio: ['ignore', 'pipe', 'pipe']});
let logs = ''; daemon.stderr.on('data', b => logs += b); daemon.stdout.on('data', b => logs += b);
const wait = ms => new Promise(r => setTimeout(r, ms));
const errors = []; let browser;
const SCHEMES = ['light', 'dark'];
const SIZES = [{width: 1280, height: 800}, {width: 390, height: 844}];

try {
  let ready = false; for (let i = 0; i < 100 && !ready; i++) { try { ready = (await fetch(origin + '/health')).ok; } catch {} if (!ready) await wait(100); }
  assert.ok(ready, 'daemon did not start: ' + logs);
  browser = await chromium.launch({headless: true, executablePath: process.env.CHROMIUM_PATH, args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream', '--autoplay-policy=no-user-gesture-required', '--mute-audio']});
  const open = async (scheme, viewport) => {
    const context = await browser.newContext({permissions: ['camera', 'microphone', 'clipboard-read', 'clipboard-write'], viewport, colorScheme: scheme, reducedMotion: 'no-preference'});
    const page = await context.newPage(); page.setDefaultTimeout(30000);
    page.on('pageerror', e => errors.push(`${scheme} ${viewport.width}: ${e.message}`));
    page.on('console', m => { if (m.type() === 'error' && !/401|409|422/.test(m.text())) errors.push(`${scheme} ${viewport.width} console: ${m.text()}`); });
    return page;
  };
  const shot = async (page, name, {full = true} = {}) => {
    const vp = page.viewportSize();
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), `${name} overflows at ${vp.width}`);
    const scheme = await page.evaluate(() => matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
    await page.screenshot({path: path.join(out, `${name}-${scheme}-${vp.width}.png`), fullPage: full, animations: 'disabled'});
  };
  const each = async fn => { for (const s of SCHEMES) for (const v of SIZES) { const p = await open(s, v); try { await fn(p, s, v); } finally { await p.context().close(); } } };
  const home = async page => { await page.goto(await launchURL(data, origin)); await page.locator('#home-view').waitFor(); await page.waitForFunction(() => !document.querySelector('#upcoming-panel .room-empty span')?.textContent?.startsWith('Loading')); await wait(300); };

  // 1. Empty home + keyboard focus on the first control.
  await each(async page => {
    await home(page);
    await page.locator('.recent-list .room-empty', {hasText: 'No meetings yet'}).waitFor();
    await shot(page, 'home-empty');
    await page.keyboard.press('Tab');
    const focused = await page.evaluate(() => { const el = document.activeElement; const s = getComputedStyle(el); return {id: el.id || el.className, outline: s.outlineStyle !== 'none' && parseFloat(s.outlineWidth) > 0}; });
    assert.ok(focused.outline, `first Tab stop shows a focus ring (${focused.id})`);
  });
  console.log('empty home: light/dark, 1280/390; focus ring visible');

  // 2. Seed: rooms (one live), a meeting later today, an ended meeting, a record link.
  const admin = {...await adminHeaders(data), 'Content-Type': 'application/json'};
  const post = async (p, body = {}, headers = admin) => { const r = await fetch(origin + p, {method: 'POST', headers, body: JSON.stringify(body)}); const text = await r.text(); assert.ok(r.ok, `${p}: ${r.status} ${text}`); try { return JSON.parse(text); } catch { return {}; } };
  const podcast = await post('/api/v1/rooms', {name: 'The Podcast'});
  const standup = await post('/api/v1/rooms', {name: 'Team standup'});
  await post('/api/v1/rooms', {name: 'Office hours'});
  const now = new Date();
  const later = new Date(Math.min(now.getTime() + 90 * 60_000, new Date(now.getFullYear(), now.getMonth(), now.getDate(), 23, 0).getTime()));
  later.setMinutes(Math.ceil(later.getMinutes() / 5) * 5, 0, 0);
  const tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
  await post('/api/v1/schedule', {roomId: podcast.id, title: 'Episode 12 with Ada', description: '', startsAt: later.toISOString(), durationMin: 60, timeZone: tz, guestEmails: ['ada@example.com', 'grace@example.org'], organizerEmail: ''});
  const tomorrow = new Date(now.getFullYear(), now.getMonth(), now.getDate() + 1, 10, 0);
  await post('/api/v1/schedule', {roomId: standup.id, title: 'Team standup', description: '', startsAt: tomorrow.toISOString(), durationMin: 30, timeZone: tz, guestEmails: [], organizerEmail: ''});
  const past = await post('/api/v1/sessions', {title: 'Meeting with Ben', autoAdmit: false});
  await post(`/api/v1/sessions/${past.id}/end`, {}, {Authorization: `Bearer ${past.hostToken}`});
  await post(`/api/v1/rooms/${standup.id}/start`, {hostName: 'Ben'});
  const link = await post('/api/v1/recordlinks', {title: 'Intro for episode 12', prompt: '# Intro\nSay your name and what you are working on.', maxDurationSec: 300});
  const rooms = (await (await fetch(origin + '/api/v1/rooms', {headers: admin})).json()).rooms;
  const podcastLink = rooms.find(r => r.id === podcast.id).guestPath;
  const open1 = await post('/api/v1/sessions', {title: 'Meeting with Ben', autoAdmit: false});
  const sessionLink = `/join?session=${open1.id}&token=${open1.token}`;

  // 3. Home with data: each rail view, the New menu, the schedule dialog, Ready to join (host).
  await each(async (page, scheme, vp) => {
    await home(page);
    await page.locator('.upcoming-row').first().waitFor();
    await page.locator('.live-row').first().waitFor();
    await shot(page, 'home-meetings');
    await page.locator('#rail-rooms').click();
    await page.locator('.room-row').first().waitFor();
    await shot(page, 'home-rooms');
    await page.locator('#rail-recordings').click();
    await page.locator('.record-link').first().waitFor();
    await shot(page, 'home-recordings');
    await page.locator('#rail-meetings').click();
    await page.locator('#home-new-btn').click();
    await page.locator('#home-new-menu [data-new="now"]').waitFor();
    await shot(page, 'home-new-menu', {full: false});
    await page.keyboard.press('Escape');
    if (vp.width > 600) {
      await page.locator('#schedule-btn').click();
      await page.locator('.schedule-dialog #sch-title').waitFor();
      await shot(page, 'schedule-dialog', {full: false});
      await page.keyboard.press('Escape');
    }
    await page.locator('#home-new-btn').click();
    await page.locator('#home-new-menu [data-new="now"]').click();
    await page.locator('#green-room').waitFor({state: 'visible'});
    await page.waitForFunction(() => document.querySelector('#preview-video').videoWidth > 0);
    await page.locator('#name-input').fill('Ben');
    await page.waitForFunction(() => !document.querySelector('#preflight-panel .pf-summary')?.textContent?.includes('Checking'));
    await wait(5500); // the first periodic re-check after the camera started
    await shot(page, 'ready-host');
    await page.locator('#preflight-panel .pf-summary').click();
    await page.locator('#recording-options summary').click();
    await wait(300);
    await shot(page, 'ready-host-expanded');
    await page.locator('#ready-cam-btn').click();
    await page.locator('#ready-bg-btn').click();
    await wait(200);
    await shot(page, 'ready-host-camera-off', {full: false});
  });
  console.log('home with data, New menu, schedule dialog, Ready to join (host)');

  // 4. Guests: Ready to join for a room, waiting for the host, asking to join; solo record page.
  await each(async page => {
    await page.goto(origin + podcastLink);
    await page.locator('#green-room h1', {hasText: 'Join The Podcast'}).waitFor();
    await page.waitForFunction(() => document.querySelector('#preview-video').videoWidth > 0);
    await page.locator('#name-input').fill('Ada');
    await page.waitForFunction(() => !document.querySelector('#join-btn').disabled);
    await wait(5500);
    await shot(page, 'ready-guest');
    await page.locator('#join-btn').click();
    await page.locator('#waiting-room.is-waiting-host').waitFor();
    await page.waitForFunction(() => document.querySelector('#waiting-preview-video').videoWidth > 0);
    await shot(page, 'waiting-for-host');
    await page.goto(origin + sessionLink);
    await page.waitForFunction(() => document.querySelector('#preview-video').videoWidth > 0);
    await page.locator('#name-input').fill('Grace');
    await page.waitForFunction(() => /Ask to join/.test(document.querySelector('#join-btn').textContent) && !document.querySelector('#join-btn').disabled);
    await page.locator('#join-btn').click();
    await page.locator('#waiting-room').waitFor({state: 'visible'});
    await page.waitForFunction(() => document.querySelector('#waiting-preview-video').videoWidth > 0);
    await wait(300);
    await shot(page, 'asking-to-join');
    await page.goto(link.url.replace(/^https?:\/\/[^/]+/, origin));
    await page.locator('#rec-main').waitFor({state: 'visible'});
    await page.waitForFunction(() => document.querySelector('#rec-preview').videoWidth > 0);
    await wait(800);
    await shot(page, 'record-solo');
  });
  console.log('guest Ready to join, waiting for host, asking to join, solo record page');

  assert.deepEqual(errors, []);
  console.log(`ux-screens: PASS (${out})`);
} catch (err) {
  console.error(err);
  console.error('--- daemon log ---\n' + logs.slice(-3000));
  process.exitCode = 1;
} finally {
  await browser?.close();
  daemon.kill('SIGTERM');
  await wait(300);
  await rm(data, {recursive: true, force: true});
}
