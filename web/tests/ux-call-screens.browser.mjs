// The in-call screens [SL-UX-2] against the real daemon with Chromium fake
// devices, in light and dark at 1280×800 and 390×844. Renders: the host alone
// with "Your meeting's ready", a 3-person grid, the reactions bar, chat, live
// captions, People with someone waiting, speaker view with a shared screen,
// recording with the Studio panel open, the livestream dialog, uploads and
// "Get files for editing", and the meeting-ended screen. Checks the things the
// redesign promises: the ready card is the host's only, technical chips are
// gone from the top bar, the REC pill shows while recording, every Studio
// tool is reachable, keyboard focus is visible and the page never scrolls
// sideways. Screenshots go to STUDIO_SCREENSHOT_DIR (skipped when unset).
// Build first; STUDIO_BINARY points at the daemon. ffmpeg (PATH, or
// STUDIO_FFMPEG_DIR) makes the speech audio and the editing files.
import assert from 'node:assert/strict';
import {spawn,execFileSync} from 'node:child_process';
import {existsSync} from 'node:fs';
import {mkdtemp,rm,mkdir} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {isolatedNetworkArgs,launchURL,revealControl,startTake,readyToJoin} from './lib/harness.mjs';
import {connectFakeProvider} from './lib/fake-caption-provider.mjs';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');

const data=await mkdtemp(path.join(tmpdir(),'studio-ux-call-'));
const port=Number(process.env.STUDIO_TEST_PORT||18943),origin=`http://127.0.0.1:${port}`;
const bundledFF=path.resolve('../build/media-tools/bin');
const ffDir=process.env.STUDIO_FFMPEG_DIR||(existsSync(path.join(bundledFF,'ffmpeg'))?bundledFF:'');
// Test audio needs lavfi, which the bundled LGPL build leaves out: use the system ffmpeg for it.
const ffmpeg=process.env.FFMPEG||'ffmpeg';
const env={...process.env,PATH:ffDir?`${ffDir}:${process.env.PATH}`:process.env.PATH};
const shotDir=process.env.STUDIO_SCREENSHOT_DIR;
if(shotDir)await mkdir(shotDir,{recursive:true});
const wait=ms=>new Promise(r=>setTimeout(r,ms));
const VIEWPORTS={desktop:{width:1280,height:800},phone:{width:390,height:844}};

// Guests talk (so the sound ring moves); the host hums a tone.
const toneWav=path.join(data,'tone.wav'),speechWav=path.join(data,'speech.wav');
execFileSync(ffmpeg,['-loglevel','error','-y','-f','lavfi','-i','sine=frequency=600:sample_rate=48000:duration=30','-af','volume=0.3','-ac','1','-c:a','pcm_s16le',toneWav]);
try{
 const aiff=path.join(data,'speech.aiff');
 execFileSync('say',['-v','Samantha','-o',aiff,'Welcome back to the show. Today we talk about recording podcasts with friends, from anywhere, without losing a single word.']);
 execFileSync(ffmpeg,['-loglevel','error','-y','-i',aiff,'-ar','48000','-ac','1','-c:a','pcm_s16le',speechWav]);
}catch{
 execFileSync(ffmpeg,['-loglevel','error','-y','-f','lavfi','-i','anoisesrc=color=pink:sample_rate=48000:duration=20,volume=0.4','-af','tremolo=f=4:d=0.9','-ac','1','-c:a','pcm_s16le',speechWav]);
}

const daemon=spawn(process.env.STUDIO_BINARY||'../bin/p2pvideod',['--data-dir',data,'--port',String(port),'--no-funnel','--no-public-stun',...await isolatedNetworkArgs()],{stdio:['ignore','pipe','pipe'],env});
let logs='';daemon.stderr.on('data',b=>logs+=b);daemon.stdout.on('data',b=>logs+=b);
const errors=[];const browsers=[];let provider;
const launch=wav=>chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream',`--use-file-for-fake-audio-capture=${wav}`,'--autoplay-policy=no-user-gesture-required','--mute-audio']}).then(b=>{browsers.push(b);return b;});
const open=async(browser,name)=>{
 const context=await browser.newContext({permissions:['camera','microphone','clipboard-read','clipboard-write'],viewport:VIEWPORTS.desktop,acceptDownloads:true,colorScheme:'light'});
 const page=await context.newPage();page.setDefaultTimeout(30000);
 page.on('console',m=>{if(m.type()==='error'&&!(m.text().includes('422')&&m.location().url.endsWith('/invite'))&&!/Failed to load resource/.test(m.text()))errors.push(`${name} console: ${m.text()}`);});
 page.on('pageerror',e=>errors.push(`${name}: ${e.message}`));
 return page;
};

/** Every scene in both appearances and both sizes; returns to light desktop. */
const shoot=async(page,name,{phone=true,before}={})=>{
 const noSideScroll=await page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth);
 assert.ok(noSideScroll,`${name}: the page scrolls sideways`);
 if(!shotDir)return;
 for(const scheme of ['light','dark'])for(const [size,vp] of Object.entries(VIEWPORTS)){
  if(size==='phone'&&!phone)continue;
  await page.emulateMedia({colorScheme:scheme});await page.setViewportSize(vp);
  await before?.(size);
  await wait(450);
  await page.screenshot({path:path.join(shotDir,`${name}-${scheme}-${size}.png`),animations:'disabled'});
  if(size==='phone')assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth),`${name}: sideways scroll at 390px`);
 }
 await page.emulateMedia({colorScheme:'light'});await page.setViewportSize(VIEWPORTS.desktop);await wait(250);
};

try{
 let ready=false;for(let i=0;i<100;i++){try{ready=(await fetch(origin+'/health')).ok;if(ready)break;}catch{}await wait(100);}assert.ok(ready,'daemon did not start: '+logs);
 provider=await connectFakeProvider({origin,dataDir:data});

 // ---------------------------------------------------------------- host alone
 const host=await open(await launch(toneWav),'Host');
 await host.goto(await launchURL(data,origin));await readyToJoin(host);
 await host.locator('#name-input').fill('Ben');
 const created=host.waitForResponse(r=>r.url().endsWith('/api/v1/sessions')&&r.request().method()==='POST');
 await host.locator('#join-btn').click();const session=await(await created).json();
 await host.locator('#studio-view').waitFor({state:'visible'});
 await host.locator('#meeting-ready').waitFor({state:'visible'});
 // The top bar is time | title ⓘ and people: no technical chips.
 const bar=await host.locator('.call-bar').innerText();
 assert.doesNotMatch(bar,/Pre-flight|decoded|Take \d|No take|track uploaded/,`technical chip in the top bar: ${bar}`);
 assert.equal(await host.locator('#copy-invite-btn').isVisible(),true,'Add others is the host’s');
 await shoot(host,'call-01-host-ready');
 // Keyboard: Tab reaches the ready card's actions with a visible focus ring.
 await host.locator('#meeting-ready [data-close]').focus();await host.keyboard.press('Tab');
 assert.equal(await host.evaluate(()=>document.activeElement?.hasAttribute('data-add-others')),true,'Tab order in the ready card');
 assert.notEqual(await host.evaluate(()=>getComputedStyle(document.activeElement).outlineStyle),'none','focus ring missing');
 await host.locator('#meeting-ready [data-close]').click();
 assert.equal(await host.locator('#meeting-ready').isHidden(),true);

 // Meeting details popover (ⓘ) and the way back to the ready card.
 await host.locator('#call-details-btn').click();await host.locator('#call-details').waitFor({state:'visible'});
 await shoot(host,'call-02-details',{phone:false});
 await host.keyboard.press('Escape');assert.equal(await host.locator('#call-details').isHidden(),true);

 // ---------------------------------------------------------------- guests
 const speech=await launch(speechWav);
 const join=async name=>{
  const guest=await open(speech,name);
  await guest.goto(`${origin}/join?session=${session.id}&token=${session.token}`);
  await guest.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
  await guest.locator('#name-input').fill(name);await guest.locator('#join-btn').click();
  return guest;
 };
 const admit=async name=>{
  const guest=await join(name);
  await host.waitForFunction(n=>document.querySelector('#host-admission-banner').style.display==='flex'&&document.querySelector('#admission-guest-name').textContent===n,name);
  await host.locator('#admit-btn').click();await guest.locator('#studio-view').waitFor({state:'visible'});
  return guest;
 };
 const ada=await admit('Ada Lovelace');
 const grace=await admit('Grace Hopper');
 assert.equal(await ada.locator('#meeting-ready').isHidden(),true,'guests never see the ready card');
 for(const p of [host,ada,grace])await p.waitForFunction(()=>[...document.querySelectorAll('#video-grid .tile[data-participant-id]')].filter(t=>t.querySelector('video')?.videoWidth>0).length===2,null,{timeout:30000});
 await host.waitForFunction(()=>document.querySelector('.tile.tile-speaking'),null,{timeout:15000}).catch(()=>{});
 await shoot(host,'call-03-grid');
 await shoot(ada,'call-03-grid-guest',{phone:true});

 // Reactions bar above the dock.
 await (await revealControl(host,'#react-btn')).click();await host.locator('#reaction-picker').waitFor({state:'visible'});
 await host.locator('.reaction-choice[data-emoji="🎉"]').click();
 await ada.locator('#react-btn').click();await ada.locator('.reaction-choice[data-emoji="❤️"]').click();
 await wait(500);
 await shoot(host,'call-04-reactions',{phone:false});
 await host.keyboard.press('Escape');

 // Chat.
 await ada.locator('#chat-btn').click();await ada.locator('#chat-input').fill('Hi Ben! Ready when you are.');await ada.keyboard.press('Enter');
 await grace.locator('#chat-btn').click();await grace.locator('#chat-input').fill('Notes for today: https://example.com/show-notes');await grace.keyboard.press('Enter');
 await host.waitForFunction(()=>document.querySelector('#chat-btn .collab-badge')?.textContent==='2');
 await host.locator('#chat-btn').click();await host.locator('#chat-input').fill('Rolling in a minute. Raise a hand if you need a pause.');await host.keyboard.press('Enter');
 await ada.locator('#hand-btn').click();
 await host.locator('.tile-hand').first().waitFor();
 await shoot(host,'call-05-chat');
 await host.locator('#chat-btn').click();

 // Live captions for everyone (host turns them on with `c`).
 await host.locator('body').click({position:{x:640,y:300}});await host.keyboard.press('KeyC');
 await host.locator('#captions-indicator',{hasText:'Captions on'}).waitFor();
 await host.waitForFunction(()=>document.querySelectorAll('#captions-overlay .caption-line').length>0,null,{timeout:20000});
 await shoot(host,'call-06-captions');

 // Someone waiting: "Guest wants to join · Deny · Admit", People open.
 const late=await join('Linus');
 await host.waitForFunction(()=>document.querySelector('#host-admission-banner').style.display==='flex');
 await host.locator('#people-btn').click();
 await (await revealControl(host,'#toggle-health-btn')).click();
 await host.locator('#health-participants-list .health-source').first().waitFor();
 await shoot(host,'call-07-people-waiting');
 await host.locator('#deny-btn').click();
 await host.locator('#people-btn').click();
 await late.context().close();

 // Speaker view with a shared screen.
 await ada.locator('#screen-share-btn').click();
 await host.waitForFunction(()=>document.querySelector('#video-grid .tile-screen video')?.videoWidth>0,null,{timeout:20000});
 await shoot(host,'call-08-present');
 await ada.locator('#screen-share-btn').click();
 await host.waitForFunction(()=>!document.querySelector('#video-grid .tile-screen'));

 // Recording, with the Studio panel open.
 await startTake(host);
 for(const p of [host,ada,grace])await p.waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='inline-flex');
 await wait(4000);
 assert.match(await host.locator('#rec-indicator').innerText(),/Recording\s+0:0\d/);
 assert.match(await host.locator('#rec-btn').getAttribute('data-label'),/^Stop recording/);
 await host.locator('#studio-btn').click();await host.locator('#studio-panel').waitFor({state:'visible'});
 for(const id of ['#preflight-chip','#dock-transfers-btn','#postproc-btn','#livestream-btn','#branding-btn','#prompter-btn','#phone-camera-btn','#settings-btn','#producer-link-btn','#studio-captions-btn'])
  assert.equal(await host.locator(`#studio-panel ${id}`).count(),1,`${id} is not in Studio`);
 await shoot(host,'call-09-recording-studio');
 await shoot(ada,'call-09-recording-guest',{phone:false});

 // Go live.
 await host.locator('#livestream-btn').click();await host.locator('#livestream-panel.open').waitFor();
 await host.locator('#livestream-dest-list [data-field=key]').first().fill('live-key-hidden');
 await shoot(host,'call-10-livestream');
 await host.keyboard.press('Escape');

 // Stop: uploads, then "Get files for editing".
 await host.locator('#rec-btn').click();
 await host.locator('#transfers-recovery-modal').waitFor({state:'visible'});
 await host.waitForFunction(()=>!document.querySelector('#prepare-export-btn').disabled,null,{timeout:60000});
 await host.waitForFunction(()=>document.querySelectorAll('.toast').length===0,null,{timeout:10000}).catch(()=>{});
 await shoot(host,'call-11-uploads');
 const pending=host.waitForResponse(r=>r.url().endsWith('/postprocess')&&r.request().method()==='POST',{timeout:120000});
 await host.locator('#prepare-export-btn').click();
 if((await pending).ok()){
  await host.locator('#export-modal').waitFor({state:'visible'});
  await host.waitForFunction(()=>document.querySelectorAll('.toast').length===0,null,{timeout:10000}).catch(()=>{});
  await shoot(host,'call-12-export');
  await host.locator('#close-modal-btn').click();
 }else console.log('export skipped (no ffmpeg for the daemon)');

 // End meeting for everyone.
 await host.locator('#studio-panel [data-close]').click().catch(()=>{});
 await host.locator('#end-meeting-btn').click();
 await host.locator('.confirm-dialog[open] [data-confirm], dialog.room-dialog [data-confirm]').first().click();
 for(const p of [host,ada])await p.locator('#meeting-ended').waitFor();
 // The host is offered the caption transcript first.
 await host.locator('#captions-transcript-dialog').waitFor({state:'visible'});
 await shoot(host,'call-13-caption-transcript',{phone:false});
 await host.locator('#captions-transcript-dialog [data-close]').click();
 await shoot(host,'call-13-ended-host');
 await shoot(ada,'call-13-ended-guest');

 assert.deepEqual(errors,[]);
 console.log(`PASS ux-call-screens${shotDir?` (screenshots in ${shotDir})`:''}`);
}catch(err){
 console.error(err);console.error(errors.join('\n'));console.error(logs.slice(-4000));process.exitCode=1;
}finally{
 provider?.close?.();
 for(const b of browsers)await b.close().catch(()=>{});
 daemon.kill('SIGTERM');await new Promise(r=>daemon.once('exit',r));
 await rm(data,{recursive:true,force:true});
}
