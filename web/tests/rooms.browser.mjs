// Persistent rooms and the call-first home [SL-P1-5] against the real daemon
// with Chromium fake devices. Verifies: the host creates a room on the home;
// a guest opening the room link before the host starts sees "Waiting for …"
// with a device check; the host starts the room's meeting and the guest asks
// to join automatically (no reload) and is admitted; both decode video; the
// host ends the meeting and everyone sees the ended screen; a second meeting
// in the same room accepts the same link; replacing the link refuses the old
// one and the new one works; the instant "New meeting" flow still works.
// Home (1280×720, 390 px) and waiting-for-host screenshots go to
// STUDIO_SCREENSHOT_DIR when set. Build first; STUDIO_BINARY is the daemon.
import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {mkdtemp,rm,mkdir} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {isolatedNetworkArgs,launchURL,readyToJoin,revealControl} from './lib/harness.mjs';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const data=await mkdtemp(path.join(tmpdir(),'studio-rooms-'));
const port=Number(process.env.STUDIO_TEST_PORT||18143),origin=`http://127.0.0.1:${port}`;
const daemon=spawn(process.env.STUDIO_BINARY||'../bin/p2pvideod',['--data-dir',data,'--port',String(port),'--no-funnel',...await isolatedNetworkArgs()],{stdio:['ignore','pipe','pipe']});
let logs='';daemon.stderr.on('data',b=>logs+=b);daemon.stdout.on('data',b=>logs+=b);
const screenshotDir=process.env.STUDIO_SCREENSHOT_DIR;
if(screenshotDir)await mkdir(screenshotDir,{recursive:true});
const screenshot=async(name,page)=>{if(screenshotDir)await page.screenshot({path:path.join(screenshotDir,name+'.png'),animations:'disabled',fullPage:true});};
const errors=[];let browser;
const wait=ms=>new Promise(r=>setTimeout(r,ms));
// Expected HTTP failures: the replaced link (401) and the Tailscale-less invite (422).
const expectedConsole=m=>/401|422|409/.test(m.text());
const noOverflow=page=>page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth);
const decodesRemote=(page,n=1)=>page.waitForFunction(n=>{const v=[...document.querySelectorAll('#video-grid .tile[data-participant-id] video')];return v.length>=n&&v.every(x=>x.videoWidth>0);},n,{timeout:30000});

try{
 let ready=false;for(let i=0;i<100;i++){try{ready=(await fetch(origin+'/health')).ok;if(ready)break;}catch{}await wait(100);}assert.ok(ready,'daemon did not start: '+logs);
 browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream','--autoplay-policy=no-user-gesture-required','--mute-audio']});
 const open=async(name,viewport={width:1280,height:720})=>{
  const context=await browser.newContext({permissions:['camera','microphone'],viewport});
  const page=await context.newPage();page.setDefaultTimeout(30000);
  page.on('console',m=>{if(m.type()==='error'&&!expectedConsole(m))errors.push(`${name} console: ${m.text()}`);});
  page.on('pageerror',e=>errors.push(`${name}: ${e.message}`));
  return page;
 };
 const roomsApi=async page=>(await page.evaluate(()=>fetch('/api/v1/rooms').then(r=>r.json()))).rooms;
 const roomRow=(page,name)=>page.locator('.room-row',{has:page.locator('.room-title strong',{hasText:new RegExp(`^${name}$`)})});

 // 1. Host home: create a room.
 const host=await open('Host');
 await host.goto(await launchURL(data,origin));
 // The camera stays off while the host browses the home; Ready to join opens
 // it and Back to home releases it again [SL-UX-2].
 await host.locator('#home-view').waitFor();await host.waitForTimeout(1500);
 const liveCapture=()=>host.evaluate(()=>{const s=document.querySelector('#preview-video').srcObject;return !!s&&s.getTracks().some(t=>t.readyState==='live');});
 assert.equal(await liveCapture(),false,'no camera or mic on the home');
 await readyToJoin(host);assert.equal(await liveCapture(),true,'Ready to join opens the camera');
 await host.locator('#ready-back-btn').click();await host.locator('#home-view').waitFor();
 assert.equal(await liveCapture(),false,'Back to home releases the camera');
 // Home [SL-UX-2a]: rooms live in the rail's Rooms view.
 await host.locator('#rail-rooms').click();
 await host.locator('#home-panels .rooms-panel').waitFor();
 await host.locator('.room-empty',{hasText:'No rooms yet'}).waitFor();
 await host.locator('#room-name-input').fill('The Podcast');
 // Creating a room refreshes the home in the background. A refresh that lands
 // after the user opened the new room's ⋯ menu must not close it (the
 // rehearsal hit this). Delay that refresh so the race is deterministic.
 let homeLanded;const landed=new Promise(r=>{homeLanded=r});
 await host.route('**/api/v1/home',async route=>{await new Promise(r=>setTimeout(r,800));await route.continue();homeLanded();},{times:1});
 await host.locator('#room-create-btn').click();
 await roomRow(host,'The Podcast').locator('.room-menu summary').click();
 await roomRow(host,'The Podcast').locator('details.room-menu').evaluate(d=>{d.dataset.probe='kept'});
 await landed;await host.waitForTimeout(300);
 assert.equal(await roomRow(host,'The Podcast').locator('details.room-menu[data-probe="kept"][open]').count(),1,'room menu stays open and attached across the background home refresh');
 await host.keyboard.press('Escape');
 await roomRow(host,'The Podcast').waitFor();
 let [room]=await roomsApi(host);
 assert.equal(room.slug,'the-podcast');
 assert.ok(room.guestPath.startsWith('/join?room=the-podcast&token='),'guest path');
 assert.equal(await roomRow(host,'The Podcast').locator('code').innerText(),'/join?room=the-podcast','the visible link hides its secret');
 const firstLink=origin+room.guestPath;
 assert.ok(await noOverflow(host),'home overflows at 1280');
 await screenshot('rooms-home-desktop',host);
 const narrow=await open('Narrow',{width:390,height:844});
 await narrow.goto(await launchURL(data,origin));await narrow.locator('#rail-rooms').click();await narrow.locator('.room-row').waitFor();
 assert.ok(await noOverflow(narrow),'home overflows at 390 px');
 await screenshot('rooms-home-390',narrow);
 await narrow.context().close();
 console.log('home: room created, link redacted on screen, no overflow at 1280/390');

 // 2. Guest opens the room link before the host starts: waiting for host.
 const guest=await open('Guest');
 await guest.goto(firstLink);
 await guest.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
 await guest.locator('#green-room h1',{hasText:'Join The Podcast'}).waitFor();
 await guest.locator('#name-input').fill('Ada');
 await guest.waitForFunction(()=>!document.querySelector('#join-btn').disabled);
 assert.match(await guest.locator('#join-btn').innerText(),/Join when the host starts/);
 await guest.locator('#join-btn').click();
 await guest.locator('#waiting-room.is-waiting-host h1',{hasText:'Waiting for the host to start The Podcast'}).waitFor();
 assert.ok(await guest.evaluate(()=>document.querySelector('#waiting-preview-video').srcObject?.active),'device check preview runs while waiting');
 await guest.evaluate(()=>{window.__noReload='still-here';});
 console.log('guest waits for the host with a device check');

 // 3. Host starts the room's meeting; the guest asks to join by itself.
 await roomRow(host,'The Podcast').locator('[data-act="start"]').click();
 // Start opens Ready to join for the room; its button starts the meeting.
 await host.locator('#green-room h1',{hasText:'Start The Podcast'}).waitFor();
 await host.locator('#name-input').fill('Ben');
 assert.match(await host.locator('#join-btn').innerText(),/Start The Podcast/);
 await host.locator('#join-btn').click();
 await host.locator('#studio-view').waitFor({state:'visible'});
 assert.equal(await host.locator('#display-session-title').innerText(),'The Podcast');
 assert.ok(!(await host.evaluate(()=>location.search)).includes('room='),'host URL carries the session, not the room');
 await host.waitForFunction(()=>document.querySelector('#host-admission-banner').style.display==='flex'&&document.querySelector('#admission-guest-name').textContent==='Ada');
 assert.equal(await guest.evaluate(()=>window.__noReload),'still-here','guest page reloaded');
 await guest.locator('#waiting-room h1',{hasText:'Asking to join'}).waitFor();
 await host.locator('#admit-btn').click();
 await guest.locator('#studio-view').waitFor({state:'visible'});
 await decodesRemote(host);await decodesRemote(guest);
 assert.equal(await guest.evaluate(()=>window.__noReload),'still-here');
 console.log('host started the room; guest auto-requested, was admitted without reload; both decode video');

 // 3b. A lower third set in a room meeting is remembered for the next episode [SL-P5-4].
 await (await revealControl(host,'#branding-btn')).click();
 await host.locator('#branding-panel.open').waitFor({state:'visible'});
 await host.locator('#brand-remember-hint').waitFor({state:'visible'});
 const adaRow=host.locator('form.branding-person',{has:host.locator('.branding-person-who strong',{hasText:/^Ada$/})});
 await adaRow.locator('[data-field=title]').fill('Founder, Acme');
 await adaRow.locator('[data-save]').click();
 await host.locator('#toast-container',{hasText:'remember it next episode'}).waitFor();
 await adaRow.locator('[data-clear]').waitFor();
 await host.locator('#branding-panel [data-close]').click();
 const titlesApi=async()=>(await host.evaluate(id=>fetch(`/api/v1/rooms/${id}/titles`).then(r=>r.json()),room.id)).titles;
 assert.deepEqual((await titlesApi()).map(t=>[t.displayName,t.title]),[['Ada','Founder, Acme']]);
 console.log('room remembers the lower third set in the meeting');

 // 4. Host ends the meeting.
 assert.equal(await guest.locator('#end-meeting-btn').isHidden(),true,'guests cannot end the meeting');
 await host.locator('#end-meeting-btn').click();
 await host.locator('.room-dialog [data-confirm]').click();
 await host.locator('#meeting-ended h1',{hasText:'Meeting ended'}).waitFor();
 await guest.locator('#meeting-ended h1',{hasText:'The host ended the meeting'}).waitFor();
 assert.equal(await guest.locator('#studio-view').isHidden(),true);
 await wait(4000);
 assert.equal(await guest.locator('#meeting-ended').isVisible(),true,'guest must not reconnect into an ended meeting');
 console.log('host ended the meeting for everyone');

 // 5. Second meeting, same link. The guest waits again (now for "Ben").
 await guest.locator('[data-rejoin]').click();
 await guest.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
 await guest.locator('#name-input').fill('Ada');
 await guest.waitForFunction(()=>!document.querySelector('#join-btn').disabled&&/Ben/.test(document.querySelector('#join-btn').textContent));
 await guest.locator('#join-btn').click();
 await guest.locator('#waiting-room.is-waiting-host h1',{hasText:'Waiting for Ben to start The Podcast'}).waitFor();
 assert.ok(await noOverflow(guest));
 await guest.waitForFunction(()=>document.querySelector('#waiting-preview-video').videoWidth>0);
 await screenshot('rooms-waiting-for-host',guest);
 const mobileGuest=await open('Mobile guest',{width:390,height:844});
 await mobileGuest.goto(firstLink);await mobileGuest.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
 await mobileGuest.locator('#name-input').fill('Grace');
 await mobileGuest.waitForFunction(()=>!document.querySelector('#join-btn').disabled);
 await mobileGuest.locator('#join-btn').click();
 await mobileGuest.locator('#waiting-room.is-waiting-host').waitFor();
 await mobileGuest.waitForFunction(()=>document.querySelector('#waiting-preview-video').videoWidth>0);
 assert.ok(await noOverflow(mobileGuest),'waiting-for-host overflows at 390 px');
 await screenshot('rooms-waiting-for-host-390',mobileGuest);
 await mobileGuest.locator('.waiting-host-back').click();
 await mobileGuest.locator('#green-room').waitFor({state:'visible'});
 await mobileGuest.context().close();

 await host.locator('#meeting-ended a',{hasText:'Back to home'}).click();
 await host.locator('#rail-rooms').click();
 await roomRow(host,'The Podcast').waitFor();
 await roomRow(host,'The Podcast').locator('[data-act="start"]').click();
 assert.equal(await host.locator('#name-input').inputValue(),'Ben','Ready to join remembers the name');
 await host.locator('#join-btn').click();
 await host.locator('#studio-view').waitFor({state:'visible'});
 await host.waitForFunction(()=>document.querySelector('#host-admission-banner').style.display==='flex');
 await host.locator('#admit-btn').click();
 await guest.locator('#studio-view').waitFor({state:'visible'});
 await decodesRemote(host);await decodesRemote(guest);
 console.log('second meeting in the same room accepted the same link');
 // The returning guest's title is filled in without anyone typing it.
 await (await revealControl(host,'#branding-btn')).click();
 await host.locator('#branding-panel.open').waitFor({state:'visible'});
 const adaRow2=host.locator('form.branding-person',{has:host.locator('.branding-person-who strong',{hasText:/^Ada$/})});
 await host.waitForFunction(()=>[...document.querySelectorAll('form.branding-person')].some(f=>f.querySelector('.branding-person-who strong')?.textContent==='Ada'&&f.querySelector('[data-field=title]').value==='Founder, Acme'));
 await screenshot('rooms-remembered-title',host);
 // Clear forgets it for this meeting and the room.
 await adaRow2.locator('[data-clear]').click();
 await host.locator('#toast-container',{hasText:'forgotten for this room'}).waitFor();
 await host.waitForFunction(async id=>(await fetch(`/api/v1/rooms/${id}/titles`).then(r=>r.json())).titles.length===0,room.id);
 await host.locator('#branding-panel [data-close]').click();
 console.log('episode 2 filled in the remembered title; Clear forgot it');

 // 6. Replace the link: the old one is refused, the new one works.
 await host.locator('#end-meeting-btn').click();
 await host.locator('.room-dialog [data-confirm]').click();
 await host.locator('#meeting-ended a',{hasText:'Back to home'}).click();
 await host.locator('#rail-rooms').click();
 await roomRow(host,'The Podcast').waitFor();
 await roomRow(host,'The Podcast').locator('.room-menu summary').click();
 await roomRow(host,'The Podcast').locator('[data-act="rotate"]').last().click();
 await host.locator('.room-dialog [data-confirm]').click();
 await host.locator('#toast-container',{hasText:'old link no longer works'}).waitFor();
 [room]=await roomsApi(host);
 const secondLink=origin+room.guestPath;
 assert.notEqual(secondLink,firstLink);
 const stale=await open('Stale guest');
 await stale.goto(firstLink);
 await stale.locator('#green-room h1',{hasText:'This link doesn’t work any more'}).waitFor();
 await stale.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
 await wait(300);
 assert.equal(await stale.locator('#join-btn').isDisabled(),true,'a replaced link must not offer to join');
 const fresh=await open('Fresh guest');
 await fresh.goto(secondLink);
 await fresh.locator('#green-room h1',{hasText:'Join The Podcast'}).waitFor();
 console.log('replaced link: old refused, new accepted');

 // 6b. Home → room menu → Saved titles: add, edit, forget [SL-P5-4].
 await roomRow(host,'The Podcast').locator('.room-menu summary').click();
 await roomRow(host,'The Podcast').locator('[data-act="titles"]').click();
 const titlesDialog=host.locator('dialog.room-titles-dialog');
 await titlesDialog.locator('.field-hint',{hasText:'No saved titles yet'}).waitFor();
 await titlesDialog.locator('.room-titles-add [data-field=displayName]').fill('Grace Hopper');
 await titlesDialog.locator('.room-titles-add [data-field=title]').fill('Rear Admiral');
 await titlesDialog.locator('.room-titles-add [data-save]').click();
 const graceRow=titlesDialog.locator('.room-title-row[data-key="grace hopper"]');
 await graceRow.waitFor();
 assert.equal(await graceRow.locator('[data-field=title]').inputValue(),'Rear Admiral');
 await graceRow.locator('[data-field=title]').fill('Computer scientist');
 await graceRow.locator('[data-save]').click();
 await host.locator('#toast-container',{hasText:'Saved for Grace Hopper'}).last().waitFor();
 assert.deepEqual((await titlesApi()).map(t=>[t.displayName,t.title]),[['Grace Hopper','Computer scientist']]);
 assert.ok(await noOverflow(host),'saved titles dialog overflows');
 await screenshot('rooms-saved-titles',host);
 await titlesDialog.locator('.room-title-row[data-key="grace hopper"] [data-forget]').click();
 await titlesDialog.locator('.field-hint',{hasText:'No saved titles yet'}).waitFor();
 assert.deepEqual(await titlesApi(),[]);
 await titlesDialog.locator('button[value="ok"]').click();
 await titlesDialog.waitFor({state:'detached'});
 console.log('saved titles dialog: add, edit, forget');

 // 7. Instant "New meeting" still works from the home.
 await host.goto(await launchURL(data,origin));
 await host.locator('#rail-meetings').click();
 await host.locator('.recent-row').first().waitFor();
 assert.ok((await host.locator('.recent-room').allInnerTexts()).includes('The Podcast'),'recent meetings list the room');
 await readyToJoin(host);
 assert.match(await host.locator('#join-btn').innerText(),/Start meeting/);
 await host.locator('#name-input').fill('Ben');
 const created=host.waitForResponse(r=>r.url().endsWith('/api/v1/sessions')&&r.request().method()==='POST');
 await host.locator('#join-btn').click();
 const session=await(await created).json();
 await host.locator('#studio-view').waitFor({state:'visible'});
 assert.equal(await host.locator('#display-session-title').innerText(),'Meeting with Ben');
 assert.equal(new URL(host.url()).searchParams.get('session'),session.id);
 console.log('instant meeting from the home still works');

 assert.ok(!logs.includes(new URL(firstLink).searchParams.get('token'))&&!logs.includes(new URL(secondLink).searchParams.get('token')),'a room link secret reached the daemon log');
 assert.deepEqual(errors,[]);
 console.log('rooms browser test passed');
}finally{
 await browser?.close();
 if(daemon.exitCode===null&&daemon.signalCode===null){
  daemon.kill('SIGTERM');
  await Promise.race([new Promise(r=>daemon.once('exit',r)),wait(3000)]);
  if(daemon.exitCode===null&&daemon.signalCode===null)daemon.kill('SIGKILL');
 }
 await rm(data,{recursive:true,force:true});
}
