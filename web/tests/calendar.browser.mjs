// Calendar integration [SL-P6-1] against the real daemon with Chromium fake
// devices. Verifies: the host schedules a meeting from the home (creating
// its room in the same form); it appears in Upcoming; the downloaded .ics is
// strict RFC 5545 (CRLF, ≤75-octet folded lines, METHOD:REQUEST with the
// organizer and guests, the room's guest link as URL/LOCATION, UTC times);
// copy-invite text and the Google/Outlook links carry the guest link and
// nothing host-side; the host's calendar feed serves the meeting on this
// Mac; editing bumps SEQUENCE; Start opens the room's meeting; cancelling
// yields METHOD:CANCEL/STATUS:CANCELLED with a higher SEQUENCE and drops it
// from the feed; the cancelled meeting can be removed. No overflow at
// 1280 or 390 px. Screenshots go to STUDIO_SCREENSHOT_DIR when set.
// ICAL_PYTHON=<python with the icalendar package> also parses every .ics
// with a real iCalendar parser. Build first; STUDIO_BINARY is the daemon.
import assert from 'node:assert/strict';
import {spawn, spawnSync} from 'node:child_process';
import {mkdtemp, readFile, rm, mkdir, writeFile} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {isolatedNetworkArgs, launchURL} from './lib/harness.mjs';
const {chromium} = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');

const data = await mkdtemp(path.join(tmpdir(), 'studio-calendar-'));
const port = Number(process.env.STUDIO_TEST_PORT || 18163), origin = `http://127.0.0.1:${port}`;
const daemon = spawn(process.env.STUDIO_BINARY || '../bin/p2pvideod', ['--data-dir', data, '--port', String(port), '--no-funnel', ...await isolatedNetworkArgs()], {stdio: ['ignore', 'pipe', 'pipe']});
let logs = ''; daemon.stderr.on('data', b => logs += b); daemon.stdout.on('data', b => logs += b);
const screenshotDir = process.env.STUDIO_SCREENSHOT_DIR;
if (screenshotDir) await mkdir(screenshotDir, {recursive: true});
const screenshot = async (name, page) => { if (screenshotDir) await page.screenshot({path: path.join(screenshotDir, name + '.png'), animations: 'disabled', fullPage: true}); };
const errors = []; let browser;
const wait = ms => new Promise(r => setTimeout(r, ms));
const noOverflow = page => page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth);

/** Strict hand check of an iCalendar object; returns unfolded content lines. */
function checkICS(text, label) {
  assert.ok(text.endsWith('\r\n'), `${label}: ends with CRLF`);
  assert.ok(!/[^\r]\n/.test(text) && !/\r[^\n]/.test(text), `${label}: CRLF line endings only`);
  const physical = text.slice(0, -2).split('\r\n');
  for (const l of physical) assert.ok(Buffer.byteLength(l, 'utf8') <= 75, `${label}: line over 75 octets: ${l}`);
  const lines = [];
  for (const l of physical) { if (l.startsWith(' ')) lines[lines.length - 1] += l.slice(1); else lines.push(l); }
  assert.equal(lines[0], 'BEGIN:VCALENDAR'); assert.equal(lines.at(-1), 'END:VCALENDAR');
  assert.equal(lines.filter(l => l.startsWith('BEGIN:')).length, lines.filter(l => l.startsWith('END:')).length, `${label}: balanced`);
  return lines;
}
const prop = (lines, name) => { const l = lines.find(x => x.split(':')[0].split(';')[0] === name); return l === undefined ? undefined : l.slice(l.indexOf(':') + 1); };
const unescape = s => s.replace(/\\([\\;,nN])/g, (_, c) => (c === 'n' || c === 'N' ? '\n' : c));

async function parseWithPython(file) {
  if (!process.env.ICAL_PYTHON) return 'skipped';
  const r = spawnSync(process.env.ICAL_PYTHON, ['-I', '-c', 'import sys; from icalendar import Calendar; c = Calendar.from_ical(open(sys.argv[1], "rb").read()); bad = [e for x in c.walk() for e in (getattr(x, "errors", None) or [])]; print(len(c.walk("VEVENT")), bad); sys.exit(1 if bad else 0)', file], {encoding: 'utf8'});
  assert.equal(r.status, 0, `icalendar rejected ${file}: ${r.stdout}${r.stderr}`);
  return r.stdout.trim();
}

async function download(page, trigger, name) {
  const [dl] = await Promise.all([page.waitForEvent('download'), trigger()]);
  const file = path.join(data, name);
  await dl.saveAs(file);
  return {file, text: await readFile(file, 'utf8'), suggested: dl.suggestedFilename()};
}

try {
  let ready = false; for (let i = 0; i < 100; i++) { try { ready = (await fetch(origin + '/health')).ok; if (ready) break; } catch {} await wait(100); }
  assert.ok(ready, 'daemon did not start: ' + logs);
  browser = await chromium.launch({headless: true, executablePath: process.env.CHROMIUM_PATH, args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream', '--autoplay-policy=no-user-gesture-required', '--mute-audio']});
  const open = async (name, viewport = {width: 1280, height: 720}) => {
    const context = await browser.newContext({permissions: ['camera', 'microphone', 'clipboard-read', 'clipboard-write'], viewport, acceptDownloads: true});
    const page = await context.newPage(); page.setDefaultTimeout(30000);
    page.on('console', m => { if (m.type() === 'error' && !/409/.test(m.text())) errors.push(`${name} console: ${m.text()}`); });
    page.on('pageerror', e => errors.push(`${name}: ${e.message}`));
    return page;
  };
  const row = (page, title) => page.locator('.upcoming-row', {has: page.locator('.room-title strong', {hasText: new RegExp(`^${title}$`)})});
  const api = (page, p) => page.evaluate(p => fetch(p).then(r => r.json()), p);

  // 1. Host home: empty Upcoming, then schedule with a new room.
  const host = await open('Host');
  await host.goto(await launchURL(data, origin));
  await host.locator('#upcoming-panel .room-empty', {hasText: 'Nothing scheduled'}).waitFor();
  const when = await host.evaluate(() => {
    const d = new Date(Date.now() + 3 * 3600_000); d.setMinutes(Math.ceil(d.getMinutes() / 5) * 5, 0, 0);
    const p = n => String(n).padStart(2, '0');
    return {date: `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`, time: `${p(d.getHours())}:${p(d.getMinutes())}`, utc: d.toISOString().replace(/[-:]/g, '').replace(/\.\d{3}/, ''), tz: Intl.DateTimeFormat().resolvedOptions().timeZone};
  });
  await host.locator('#schedule-btn').click();
  const form = host.locator('.schedule-dialog');
  await form.waitFor();
  assert.equal(await form.locator('#sch-room').inputValue(), '__new', 'no rooms yet: the form creates one');
  await form.locator('#sch-room-name').fill('The Podcast');
  await form.locator('#sch-title').fill('Episode 12');
  await form.locator('#sch-date').fill(when.date);
  await form.locator('#sch-time').fill(when.time);
  await form.locator('#sch-duration').selectOption('90');
  await form.locator('#sch-desc').fill('Agenda: intros; compilers, history & why it matters');
  await form.locator('#sch-guests').fill('ada@example.com, not-an-email');
  await form.locator('#sch-organizer').fill('ben@example.net');
  assert.match(await form.locator('.schedule-tz').innerText(), new RegExp(when.tz.replace('/', '\\/')));
  await form.locator('#sch-save').click();
  await form.locator('.record-form-error', {hasText: 'not-an-email'}).waitFor();
  await form.locator('#sch-guests').fill('ada@example.com, grace@example.org');
  await form.locator('#sch-save').click();
  const share = host.locator('.share-dialog');
  await share.locator('h2', {hasText: 'Meeting scheduled'}).waitFor();
  await screenshot('calendar-share-dialog', host);

  const [room] = (await api(host, '/api/v1/rooms')).rooms;
  assert.equal(room.name, 'The Podcast');
  const guestUrl = room.guestUrl;
  assert.ok(guestUrl.includes('/join?room=the-podcast&token='), 'room created with a guest link');
  let [meeting] = (await api(host, '/api/v1/schedule')).meetings;
  assert.equal(meeting.title, 'Episode 12');
  assert.equal(meeting.durationMin, 90);
  assert.deepEqual(meeting.guestEmails, ['ada@example.com', 'grace@example.org']);
  assert.equal(meeting.timeZone, when.tz);

  // 2. Copy invite text, then download the .ics from the share dialog.
  await share.locator('[data-share="copy"]').click();
  await host.waitForFunction(() => navigator.clipboard.readText().then(t => t.includes('Join the call:')));
  const invite = await host.evaluate(() => navigator.clipboard.readText());
  assert.ok(invite.startsWith('Episode 12\n') && invite.includes(guestUrl) && /Chrome/.test(invite) && /headphones/.test(invite), 'invite text');
  for (const bad of ['launch=', 'session=', 'hostToken']) assert.ok(!invite.includes(bad), `invite text leaks ${bad}`);
  let ics = await download(host, () => share.locator('[data-share="ics"]').click(), 'invite-1.ics');
  assert.equal(ics.suggested, 'episode-12.ics');
  let lines = checkICS(ics.text, 'invite');
  assert.equal(prop(lines, 'METHOD'), 'REQUEST');
  assert.equal(prop(lines, 'SEQUENCE'), '0');
  assert.equal(prop(lines, 'UID'), meeting.uid);
  assert.equal(prop(lines, 'DTSTART'), when.utc);
  assert.equal(prop(lines, 'URL'), guestUrl);
  assert.equal(unescape(prop(lines, 'LOCATION')), guestUrl);
  assert.equal(unescape(prop(lines, 'SUMMARY')), 'Episode 12');
  assert.ok(unescape(prop(lines, 'DESCRIPTION')).includes('Agenda: intros; compilers, history & why it matters'), 'agenda escaped and round-tripped');
  assert.equal(prop(lines, 'ORGANIZER'), 'mailto:ben@example.net');
  assert.equal(lines.filter(l => l.startsWith('ATTENDEE')).length, 2);
  console.log('scheduled: form validation, invite text, .ics REQUEST', await parseWithPython(ics.file));
  await share.locator('button[value="ok"]').click();

  // 3. Upcoming lists it; the invite menu has the template links.
  const r = row(host, 'Episode 12');
  await r.waitFor();
  assert.match(await r.locator('.room-meta').innerText(), /The Podcast[\s\S]*2 guests/);
  await r.locator('.upcoming-invite summary').click();
  const google = new URL(await r.locator('[data-act="google"]').getAttribute('href'));
  assert.equal(google.host, 'calendar.google.com');
  assert.equal(google.searchParams.get('location'), guestUrl);
  assert.equal(google.searchParams.get('dates'), `${when.utc}/${prop(lines, 'DTEND')}`);
  assert.equal(await r.locator('[data-act="google"]').getAttribute('rel'), 'noopener noreferrer');
  const outlook = new URL(await r.locator('[data-act="outlook"]').getAttribute('href'));
  assert.equal(outlook.host, 'outlook.live.com');
  assert.ok((await r.locator('[data-act="mail"]').getAttribute('href')).startsWith('mailto:ada@example.com,grace@example.org?'));
  await host.keyboard.press('Escape');
  await roomRowReady(host);
  assert.ok(await noOverflow(host), 'home overflows at 1280');
  await screenshot('calendar-home-desktop', host);

  const narrow = await open('Narrow', {width: 390, height: 844});
  await narrow.goto(await launchURL(data, origin));
  await row(narrow, 'Episode 12').waitFor();
  assert.ok(await noOverflow(narrow), 'home overflows at 390 px');
  await screenshot('calendar-home-390', narrow);
  await narrow.locator('#schedule-btn').click();
  await narrow.locator('.schedule-dialog #sch-room').waitFor();
  assert.ok(await narrow.evaluate(() => { const d = document.querySelector('.schedule-dialog'); return d.getBoundingClientRect().right <= window.innerWidth && d.scrollWidth <= d.clientWidth + 1; }), 'schedule dialog fits 390 px');
  await screenshot('calendar-schedule-390', narrow);
  await narrow.context().close();
  console.log('upcoming: listed with room and guests, template links, no overflow at 1280/390');

  // 4. The host's calendar feed (this Mac).
  await host.locator('#calendar-feed-btn').click();
  const feedDialog = host.locator('.feed-dialog');
  await feedDialog.locator('[data-feed="rotate"]', {hasText: 'Turn on feed'}).click();
  await feedDialog.locator('#feed-url').waitFor();
  const feed = await api(host, '/api/v1/calendar/feed');
  const feedLocal = feed.localUrl || feed.url;
  assert.ok(feedLocal.includes('/api/v1/calendar/feed.ics?key='), 'feed address');
  let feedRes = await fetch(feedLocal);
  assert.equal(feedRes.status, 200);
  assert.match(feedRes.headers.get('content-type'), /^text\/calendar/);
  let feedLines = checkICS(await feedRes.text(), 'feed');
  assert.equal(prop(feedLines, 'METHOD'), undefined, 'a feed has no METHOD');
  assert.equal(prop(feedLines, 'UID'), meeting.uid);
  assert.equal(prop(feedLines, 'X-WR-CALNAME'), 'p2p.video meetings');
  assert.equal((await fetch(feedLocal, {headers: {'Tailscale-Funnel-Request': '?1'}})).status, 403, 'feed refused through Funnel');
  assert.equal((await fetch(feedLocal.replace(/key=[0-9a-f]+/, 'key=' + '0'.repeat(64)))).status, 401, 'wrong key');
  await feedDialog.locator('button[value="ok"]').click();
  console.log('feed: served on this Mac, refused through Funnel and with a wrong key');

  // 5. Edit: SEQUENCE goes up and the share dialog says to resend.
  await r.locator('summary.icon-button').click();
  await r.locator('[data-act="edit"]').click();
  await form.locator('#sch-title').fill('Episode 12 with Ada');
  await form.locator('#sch-save').click();
  await share.locator('h2', {hasText: 'Meeting updated'}).waitFor();
  ics = await download(host, () => share.locator('[data-share="ics"]').click(), 'invite-2.ics');
  lines = checkICS(ics.text, 'update');
  assert.equal(prop(lines, 'SEQUENCE'), '1');
  assert.equal(prop(lines, 'UID'), meeting.uid, 'the UID never changes');
  assert.equal(unescape(prop(lines, 'SUMMARY')), 'Episode 12 with Ada');
  await share.locator('button[value="ok"]').click();
  console.log('edit: SEQUENCE 1, same UID', await parseWithPython(ics.file));

  // 6. Start opens the room's meeting.
  const edited = row(host, 'Episode 12 with Ada');
  await edited.locator('[data-act="start"]').click();
  // Start opens Ready to join for the meeting's room [SL-UX-2a].
  await host.locator('#green-room h1', {hasText: 'Start The Podcast'}).waitFor();
  await host.locator('#name-input').fill('Ben');
  await host.locator('#join-btn').click();
  await host.locator('#studio-view').waitFor({state: 'visible'});
  assert.equal(await host.locator('#display-session-title').innerText(), 'The Podcast');
  const [liveRoom] = (await api(host, '/api/v1/rooms')).rooms;
  assert.ok(liveRoom.live && liveRoom.liveSessionId, 'the room has a live meeting');
  await host.locator('#end-meeting-btn').click();
  await host.locator('.room-dialog [data-confirm]').click();
  await host.locator('#meeting-ended h1', {hasText: 'Meeting ended'}).waitFor();
  await host.locator('#meeting-ended a', {hasText: 'Back to home'}).click();
  await row(host, 'Episode 12 with Ada').waitFor();
  console.log('start: opened the room meeting from Upcoming');

  // 7. Cancel: METHOD:CANCEL, higher SEQUENCE, gone from the feed; then remove.
  const again = row(host, 'Episode 12 with Ada');
  await again.locator('summary.icon-button').click();
  await again.locator('[data-act="cancel"]').click();
  await host.locator('.room-dialog [data-confirm]').click();
  await share.locator('h2', {hasText: 'Meeting cancelled'}).waitFor();
  ics = await download(host, () => share.locator('[data-share="ics"]').click(), 'cancel.ics');
  lines = checkICS(ics.text, 'cancel');
  assert.equal(prop(lines, 'METHOD'), 'CANCEL');
  assert.equal(prop(lines, 'STATUS'), 'CANCELLED');
  assert.equal(prop(lines, 'SEQUENCE'), '2');
  assert.equal(prop(lines, 'UID'), meeting.uid);
  await share.locator('button[value="ok"]').click();
  await again.locator('.badge', {hasText: 'Cancelled'}).waitFor();
  assert.equal(await again.locator('[data-act="start"]').count(), 0, 'a cancelled meeting cannot be started');
  feedRes = await fetch(feedLocal);
  assert.ok(!(await feedRes.text()).includes(meeting.uid), 'cancelled meeting left the feed');
  console.log('cancel: CANCEL/CANCELLED/SEQUENCE 2, removed from the feed', await parseWithPython(ics.file));
  await again.locator('summary.icon-button').click();
  await again.locator('[data-act="delete"]').click();
  await host.locator('#upcoming-panel .room-empty', {hasText: 'Nothing scheduled'}).waitFor();
  assert.equal((await api(host, '/api/v1/schedule')).meetings.length, 0);

  assert.ok(!logs.includes(guestUrl.split('token=')[1]), 'guest link secret logged');
  assert.ok(!logs.includes(feedLocal.split('key=')[1]), 'feed key logged');
  assert.deepEqual(errors, [], 'page errors');
  console.log('calendar.browser.mjs: PASS');
} catch (err) {
  console.error(err);
  console.error('--- daemon log ---\n' + logs.slice(-4000));
  process.exitCode = 1;
} finally {
  await browser?.close();
  daemon.kill('SIGTERM');
  await wait(300);
  await rm(data, {recursive: true, force: true});
}

async function roomRowReady(page) {
  // Rooms live in the home's Rooms view [SL-UX-2a]; come back to Meetings after.
  await page.locator('#rail-rooms').click();
  await page.locator('.room-row', {has: page.locator('.room-title strong', {hasText: /^The Podcast$/})}).waitFor();
  await page.locator('#rail-meetings').click();
}
void writeFile;
