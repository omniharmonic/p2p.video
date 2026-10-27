// Screen share, views and mid-call device switching (ROADMAP P1) against the
// real daemon with Chromium fake devices only. --use-fake-ui-for-media-stream
// together with --use-fake-device-for-media-stream makes getDisplayMedia
// return Chromium's synthetic 1280×720 "screen" (plus fake system audio)
// without a picker, headless.
//
// Host + 2 guests. Verifies: Guest A presents → host and Guest B decode the
// screen in the presentation layout (screen on stage, presenter's camera
// first in the filmstrip) while cameras keep flowing; screen sender caps; a
// second presenter is refused; a take started while sharing records the screen
// as its own late "screen" track; stopping via the browser's native control
// (track `ended`) mid-take finalizes it; the export contains a decodable
// video+audio screen angle at a positive offset; the host stops another
// guest's share remotely (and guests can't); device switching keeps remote
// media flowing and is locked while recording; speaker view, pin and hide-self;
// no horizontal overflow at 1280×720 and 390px.
// Build first; STUDIO_BINARY points at the daemon.
import assert from 'node:assert/strict';
import {spawn,execFileSync} from 'node:child_process';
import {mkdtemp,rm,readFile,mkdir} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {startTake,revealControl,launchURL,readyToJoin} from './lib/harness.mjs';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const data=await mkdtemp(path.join(tmpdir(),'studio-screenshare-'));
const port=Number(process.env.STUDIO_TEST_PORT||18043),origin=`http://127.0.0.1:${port}`;
const udpPort=String(process.env.STUDIO_TEST_UDP_PORT||port+1);
const daemon=spawn(process.env.STUDIO_BINARY||'../bin/p2pvideod',['--data-dir',data,'--port',String(port),'--udp-port',udpPort,'--no-funnel'],{stdio:['ignore','pipe','pipe']});
let logs='';daemon.stderr.on('data',b=>logs+=b);daemon.stdout.on('data',b=>logs+=b);
const screenshotDir=process.env.STUDIO_SCREENSHOT_DIR;
if(screenshotDir)await mkdir(screenshotDir,{recursive:true});
const screenshot=async(name,page)=>{if(screenshotDir)await page.screenshot({path:path.join(screenshotDir,name+'.png'),animations:'disabled'});};
const errors=[];let browser;
const names=['Host','Guest A','Guest B'];
const pages=new Map();
const wait=ms=>new Promise(r=>setTimeout(r,ms));
const noHorizontalOverflow=page=>page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth);
const quietToasts=page=>page.waitForFunction(()=>document.querySelectorAll('.toast').length===0,{},{timeout:15000});

// A remote screen tile from `owner` decodes and its frames advance.
async function assertScreenDecodes(page,owner){
 await page.waitForFunction(owner=>{const t=[...document.querySelectorAll('#video-grid > .tile-screen:not(.tile-screen-local)')].find(t=>t.querySelector('.tile-name')?.textContent===`${owner} is presenting`);return t&&t.querySelector('video').videoWidth>0;},owner,{timeout:30000});
 const frames=()=>page.evaluate(()=>document.querySelector('#video-grid > .tile-screen:not(.tile-screen-local) video').getVideoPlaybackQuality().totalVideoFrames);
 const before=await frames();
 await page.waitForFunction(before=>document.querySelector('#video-grid > .tile-screen:not(.tile-screen-local) video')?.getVideoPlaybackQuality().totalVideoFrames>before+5,before,{timeout:15000});
}
// Every remote camera tile decodes with advancing frames.
async function assertCamerasFlow(page,expected){
 await page.waitForFunction(n=>{const v=[...document.querySelectorAll('.tile[data-participant-id] video')];return v.length===n&&v.every(x=>x.videoWidth>0);},expected,{timeout:30000});
 const frames=()=>page.evaluate(()=>[...document.querySelectorAll('.tile[data-participant-id] video')].map(v=>v.getVideoPlaybackQuality().totalVideoFrames));
 const before=await frames();
 await page.waitForFunction(before=>{const now=[...document.querySelectorAll('.tile[data-participant-id] video')].map(v=>v.getVideoPlaybackQuality().totalVideoFrames);return now.length===before.length&&now.every((n,i)=>n>before[i]+5);},before,{timeout:15000});
}
// Inbound audio bytes from one remote participant keep increasing.
async function assertAudioFlowsFrom(page,name){
 const bytes=()=>page.evaluate(async name=>{
  const tile=[...document.querySelectorAll('.tile[data-participant-id]')].find(t=>t.querySelector('.tile-name').textContent===name);
  const ids=new Set(tile.querySelector('video').srcObject.getAudioTracks().map(t=>t.id));
  const pc=window.__pcs.filter(p=>p.connectionState==='connected').at(-1);let total=0;
  (await pc.getStats()).forEach(s=>{if(s.type==='inbound-rtp'&&s.kind==='audio'&&ids.has(s.trackIdentifier))total+=s.bytesReceived;});
  return total;
 },name);
 const before=await bytes();
 for(let i=0;i<50;i++){await wait(200);if(await bytes()>before+2000)return;}
 assert.fail(`${name} audio stopped flowing to the receiver`);
}
const presentationOk=page=>page.evaluate(owner=>{
 const grid=document.querySelector('#video-grid');
 const stage=grid.querySelector(':scope > .tile-stage');
 const film=[...grid.querySelectorAll(':scope > .tile-filmstrip')].sort((a,b)=>Number(getComputedStyle(a).order)-Number(getComputedStyle(b).order));
 const firstFilm=film[0]?.querySelector('.tile-name')?.textContent;
 const s=stage?.getBoundingClientRect(),f=film[0]?.getBoundingClientRect();
 return {presentation:grid.classList.contains('layout-presentation'),stageIsScreen:!!stage?.classList.contains('tile-screen'),firstFilm,stageLarger:!!(s&&f&&s.width>f.width*2)};
},null);

try{
 let ready=false;for(let i=0;i<100;i++){try{ready=(await fetch(origin+'/health')).ok;if(ready)break;}catch{}await wait(100);}assert.ok(ready,'daemon did not start: '+logs);
 browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream','--autoplay-policy=no-user-gesture-required','--mute-audio']});
 const open=async name=>{
  const context=await browser.newContext({permissions:['camera','microphone'],viewport:{width:1280,height:720}});
  await context.addInitScript(()=>{const Original=window.RTCPeerConnection;window.__pcs=[];window.RTCPeerConnection=class extends Original{constructor(...args){super(...args);window.__pcs.push(this);}};});
  const page=await context.newPage();page.setDefaultTimeout(45000);
  page.on('console',m=>{if(m.type()==='error'&&!(m.text().includes('422')&&m.location().url.endsWith('/invite')))errors.push(`${name} console: ${m.text()}`);});
  page.on('pageerror',e=>errors.push(`${name}: ${e.message}`));
  pages.set(name,page);return page;
 };
 const host=await open('Host');
 await host.goto(await launchURL(data,origin));await readyToJoin(host);
 await host.locator('#name-input').fill('Host');
 const created=host.waitForResponse(r=>r.url().endsWith('/api/v1/sessions')&&r.request().method()==='POST');
 await host.locator('#join-btn').click();const session=await(await created).json();
 await host.locator('#studio-view').waitFor({state:'visible'});
 for(const name of names.slice(1)){
  const guest=await open(name);
  await guest.goto(`${origin}/join?session=${session.id}&token=${session.token}`);
  await guest.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
  await guest.locator('#name-input').fill(name);await guest.locator('#join-btn').click();
  await host.waitForFunction(name=>document.querySelector('#host-admission-banner').style.display==='flex'&&document.querySelector('#admission-guest-name').textContent===name,name);
  await host.locator('#admit-btn').click();await guest.locator('#studio-view').waitFor({state:'visible'});
 }
 const a=pages.get('Guest A'),b=pages.get('Guest B');
 for(const page of pages.values())await assertCamerasFlow(page,2);
 console.log('three participants decode each other');

 // ---- Guest A presents.
 await a.locator('#screen-share-btn').click();
 await a.waitForFunction(()=>document.querySelector('#screen-share-btn').getAttribute('aria-pressed')==='true'&&document.querySelector('#local-screen-tile'));
 for(const page of [host,b]){
  await assertScreenDecodes(page,'Guest A');
  await page.waitForFunction(()=>document.querySelector('#video-grid').classList.contains('layout-presentation'));
  const p=await presentationOk(page);
  assert.deepEqual(p,{presentation:true,stageIsScreen:true,firstFilm:'Guest A',stageLarger:true},'presentation layout');
  await assertCamerasFlow(page,2); // cameras keep flowing during the share
 }
 // The presenter sees its own share on stage too, and the roster marks it.
 assert.equal(await a.evaluate(()=>document.querySelector('#video-grid > .tile-stage')?.id),'local-screen-tile');
 // Screen sender: separate track with the screen policy.
 const screenSender=await a.evaluate(()=>{
  const pc=window.__pcs.filter(p=>p.connectionState==='connected').at(-1);
  const local=document.querySelector('#local-screen-tile video').srcObject.getVideoTracks()[0];
  const s=pc.getSenders().find(s=>s.track===local);const p=s.getParameters();
  return {contentHint:local.contentHint,maxBitrate:p.encodings[0].maxBitrate,maxFramerate:p.encodings[0].maxFramerate,degradation:p.degradationPreference,senders:pc.getSenders().filter(s=>s.track).length,pcs:window.__pcs.length};
 });
 assert.equal(screenSender.contentHint,'detail');
 assert.equal(screenSender.maxBitrate,2_500_000);
 assert.equal(screenSender.maxFramerate,15);
 assert.equal(screenSender.degradation,'maintain-resolution');
 assert.equal(screenSender.senders,4,'camera, mic, screen video and screen audio on one PeerConnection');
 assert.equal(screenSender.pcs,1,'sharing renegotiated the existing PeerConnection');
 console.log('Guest A presents; host and Guest B decode it in presentation layout',JSON.stringify(screenSender));
 // Screenshots: presentation layout, desktop and phone width.
 for(const page of pages.values())await quietToasts(page);
 assert.ok(await noHorizontalOverflow(host),'presentation overflows at 1280');
 await screenshot('screenshare-desktop-host',host);await screenshot('screenshare-desktop-presenter',a);
 for(const page of [host,a]){
  await page.setViewportSize({width:390,height:844});
  assert.ok(await noHorizontalOverflow(page),'presentation overflows at 390px');
 }
 await screenshot('screenshare-390-host',host);await screenshot('screenshare-390-presenter',a);
 for(const page of [host,a])await page.setViewportSize({width:1280,height:720});

 // A second presenter is refused (one share at a time).
 await b.locator('#screen-share-btn').click();
 await b.getByText('Guest A is presenting').first().waitFor();
 assert.equal(await b.locator('#screen-share-btn').getAttribute('aria-pressed'),'false');

 // ---- Take while sharing; native "Stop sharing" mid-take.
 await startTake(host);
 for(const page of pages.values())await page.waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='inline-flex');
 // Device switching is locked while recording.
 await (await revealControl(a,'#devices-btn')).click();
 await a.waitForFunction(()=>document.querySelector('#call-mic-select').disabled&&document.querySelector('.device-note').textContent.includes('locked'));
 await a.keyboard.press('Escape');await a.waitForFunction(()=>document.querySelector('#device-popover').hidden);
 await wait(3000);
 await a.evaluate(()=>document.querySelector('#local-screen-tile video').srcObject.getVideoTracks()[0].dispatchEvent(new Event('ended')));
 await a.waitForFunction(()=>document.querySelector('#screen-share-btn').getAttribute('aria-pressed')==='false'&&!document.querySelector('#local-screen-tile'));
 for(const page of [host,b])await page.waitForFunction(()=>!document.querySelector('#video-grid > .tile-screen')&&document.querySelector('#video-grid').classList.contains('layout-split'),{},{timeout:20000});
 await a.getByText('The screen recording for this take is saved').first().waitFor({timeout:30000});
 console.log('native stop ended the share mid-take; screen recording finalized');
 await wait(1500);
 await host.locator('#rec-btn').click();
 await host.locator('#transfers-recovery-modal').waitFor({state:'visible'});
 const headers={Authorization:`Bearer ${session.hostToken}`};let snapshot;
 for(let i=0;i<300;i++){
  const detail=await(await fetch(`${origin}/api/v1/sessions/${session.id}`,{headers})).json();
  const take=detail.takes[0];if(take){snapshot=await(await fetch(`${origin}/api/v1/takes/${take.id}`,{headers})).json();if(snapshot.tracks.length===4&&snapshot.tracks.every(t=>t.status==='assembled'))break;}
  await wait(100);
 }
 assert.equal(snapshot.tracks.length,4,JSON.stringify(snapshot));
 assert.ok(snapshot.tracks.every(t=>t.status==='assembled'),JSON.stringify(snapshot));
 assert.equal(snapshot.missingParticipants.length,0);
 const screenTrack=snapshot.tracks.find(t=>t.sourceLabel==='Screen');
 assert.ok(screenTrack&&screenTrack.participantName==='Guest A'&&screenTrack.totalBytes>1000,JSON.stringify(snapshot.tracks));
 const post=await fetch(`${origin}/api/v1/takes/${snapshot.take.id}/postprocess`,{method:'POST',headers});
 assert.ok(post.ok,await post.text());
 const out=path.join(data,'sessions',session.id,'takes',snapshot.take.id,'out');
 const manifest=JSON.parse(await readFile(path.join(out,'manifest.json'),'utf8'));
 const screenAngle=manifest.sources.find(s=>s.source==='screen'&&s.kind==='video');
 const screenAudio=manifest.sources.find(s=>s.source==='screen'&&s.kind==='audio');
 assert.ok(screenAngle,JSON.stringify(manifest.sources));
 // Shared before the take: the screen recording starts with the take.
 assert.ok(Math.abs(screenAngle.offsetSec)<1,`screen offset ${screenAngle.offsetSec}`);
 assert.ok(screenAngle.durationSec>2,`screen duration ${screenAngle.durationSec}`);
 assert.ok(screenAudio,'shared audio exported with the screen angle');
 const probe=JSON.parse(execFileSync('ffprobe',['-v','error','-count_frames','-select_streams','v:0','-show_entries','stream=nb_read_frames,width,height','-of','json',path.join(out,screenAngle.file)],{encoding:'utf8'}));
 assert.ok(Number(probe.streams[0].nb_read_frames)>10,JSON.stringify(probe));
 execFileSync('ffmpeg',['-v','error','-i',path.join(out,screenAudio.file),'-f','null','-'],{stdio:'pipe'});
 const fcpxml=await readFile(path.join(out,'take_01_multicam.fcpxml'),'utf8');
 assert.ok(fcpxml.includes('name="Guest A Screen"')&&fcpxml.includes('name="Guest A Camera"'),'multicam has the screen as its own angle');
 console.log('export includes the screen angle',JSON.stringify({offsetSec:screenAngle.offsetSec,durationSec:screenAngle.durationSec,frames:probe.streams[0].nb_read_frames,size:`${probe.streams[0].width}x${probe.streams[0].height}`}));
 await host.locator('#transfers-recovery-modal').evaluate(m=>m.style.display='none').catch(()=>{});
 for(const page of [a,b])await page.locator('#transfers-recovery-modal').evaluate(m=>m.style.display='none').catch(()=>{});

 // ---- Take 2: Guest B starts sharing mid-take (late screen track), the
 // host stops it remotely (guests can't), then the take stops.
 await startTake(host);
 for(const page of pages.values())await page.waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='inline-flex');
 await wait(2500);
 await b.locator('#screen-share-btn').click();
 await b.waitForFunction(()=>document.querySelector('#screen-share-btn').getAttribute('aria-pressed')==='true');
 await assertScreenDecodes(host,'Guest B');await assertScreenDecodes(a,'Guest B');
 assert.equal(await a.locator('#video-grid > .tile-screen .tile-screen-stop').isVisible(),false,'guest sees a stop control for someone else');
 await wait(2000);
 await host.locator('#video-grid > .tile-screen .tile-screen-stop').click();
 await b.waitForFunction(()=>document.querySelector('#screen-share-btn').getAttribute('aria-pressed')==='false');
 await b.getByText('Host stopped your presentation').first().waitFor();
 for(const page of [host,a])await page.waitForFunction(()=>!document.querySelector('#video-grid > .tile-screen'),{},{timeout:20000});
 await b.getByText('The screen recording for this take is saved').first().waitFor({timeout:30000});
 console.log('host stopped Guest B\'s share remotely mid-take');
 await wait(1000);
 await host.locator('#rec-btn').click();
 let snapshot2;
 for(let i=0;i<300;i++){
  const detail=await(await fetch(`${origin}/api/v1/sessions/${session.id}`,{headers})).json();
  const take=detail.takes.find(t=>t.number===2);if(take){snapshot2=await(await fetch(`${origin}/api/v1/takes/${take.id}`,{headers})).json();if(snapshot2.tracks.length===4&&snapshot2.tracks.every(t=>t.status==='assembled'))break;}
  await wait(100);
 }
 assert.ok(snapshot2.tracks.length===4&&snapshot2.tracks.every(t=>t.status==='assembled'),JSON.stringify(snapshot2));
 assert.ok(snapshot2.tracks.some(t=>t.sourceLabel==='Screen'&&t.participantName==='Guest B'));
 const post2=await fetch(`${origin}/api/v1/takes/${snapshot2.take.id}/postprocess`,{method:'POST',headers});
 assert.ok(post2.ok,await post2.text());
 const manifest2=JSON.parse(await readFile(path.join(data,'sessions',session.id,'takes',snapshot2.take.id,'out','manifest.json'),'utf8'));
 const lateAngle=manifest2.sources.find(s=>s.source==='screen'&&s.kind==='video');
 assert.ok(lateAngle&&lateAngle.participant==='Guest B',JSON.stringify(manifest2.sources));
 assert.ok(lateAngle.offsetSec>2,`late screen offset ${lateAngle.offsetSec}`);
 const camB=manifest2.sources.find(s=>s.participant==='Guest B'&&s.kind==='video'&&!s.source);
 assert.ok(lateAngle.offsetSec>camB.offsetSec+2,'late screen sits after the camera on the timeline');
 execFileSync('ffmpeg',['-v','error','-i',path.join(data,'sessions',session.id,'takes',snapshot2.take.id,'out',lateAngle.file),'-f','null','-'],{stdio:'pipe'});
 console.log('late screen track exported',JSON.stringify({offsetSec:lateAngle.offsetSec,durationSec:lateAngle.durationSec}));
 for(const page of pages.values())await page.locator('#transfers-recovery-modal').evaluate(m=>m.style.display='none').catch(()=>{});

 // ---- Device switching mid-call keeps media flowing.
 await (await revealControl(a,'#devices-btn')).click();
 await a.waitForFunction(()=>!document.querySelector('#call-mic-select').disabled&&document.querySelector('#call-mic-select').options.length>0);
 const devices=await a.evaluate(()=>({mics:[...document.querySelector('#call-mic-select').options].map(o=>o.value),mic:document.querySelector('#call-mic-select').value,cams:[...document.querySelector('#call-camera-select').options].map(o=>o.value),cam:document.querySelector('#call-camera-select').value}));
 const senderTracks=()=>a.evaluate(()=>{const pc=window.__pcs.filter(p=>p.connectionState==='connected').at(-1);return Object.fromEntries(pc.getSenders().filter(s=>s.track).map(s=>[s.track.kind,s.track.id]));});
 const before=await senderTracks();
 const otherMic=devices.mics.find(id=>id!==devices.mic)||devices.mic;
 if(otherMic!==devices.mic)await a.locator('#call-mic-select').selectOption(otherMic);
 else await a.locator('#call-mic-select').evaluate(s=>s.dispatchEvent(new Event('change')));
 await a.getByText('Microphone switched').first().waitFor();
 const otherCam=devices.cams.find(id=>id!==devices.cam)||devices.cam;
 if(otherCam!==devices.cam)await a.locator('#call-camera-select').selectOption(otherCam);
 else await a.locator('#call-camera-select').evaluate(s=>s.dispatchEvent(new Event('change')));
 await a.getByText('Camera switched').first().waitFor();
 const after=await senderTracks();
 assert.notEqual(after.audio,before.audio,'mic sender track replaced');
 assert.notEqual(after.video,before.video,'camera sender track replaced');
 assert.equal(await a.evaluate(()=>window.__pcs.length),1,'device switch did not rebuild the PeerConnection');
 await assertCamerasFlow(host,2);await assertCamerasFlow(b,2);
 await assertAudioFlowsFrom(host,'Guest A');await assertAudioFlowsFrom(b,'Guest A');
 await a.keyboard.press('Escape');await a.waitForFunction(()=>document.querySelector('#device-popover').hidden);
 console.log('device switch kept media flowing',JSON.stringify({mics:devices.mics.length,cams:devices.cams.length,switchedMic:otherMic!==devices.mic,switchedCam:otherCam!==devices.cam}));

 // ---- Views: speaker view follows the only unmuted remote; pin; hide self.
 await b.locator('#toggle-mic-btn').click();
 await (await revealControl(host,host.getByRole('button',{name:'Speaker',exact:true}))).click();
 const guestAId=await host.evaluate(()=>[...document.querySelectorAll('.tile[data-participant-id]')].find(t=>t.querySelector('.tile-name').textContent==='Guest A').dataset.participantId);
 await host.waitForFunction(id=>document.querySelector('#video-grid').dataset.activeSpeaker===id,guestAId,{timeout:20000});
 await host.waitForFunction(()=>document.querySelector('#video-grid > .tile-stage .tile-name')?.textContent==='Guest A');
 assert.ok(await host.evaluate(()=>document.querySelector('#video-grid').classList.contains('layout-stage')));
 await host.locator('#video-grid .tile[data-participant-id]',{hasText:'Guest B'}).hover();
 await host.getByRole('button',{name:'Pin Guest B'}).click();
 await host.waitForFunction(()=>document.querySelector('#video-grid > .tile-stage .tile-name')?.textContent==='Guest B');
 await (await revealControl(host,host.getByRole('button',{name:'Hide self'}))).click();
 assert.equal(await host.locator('#local-tile').isVisible(),false,'self view hidden');
 assert.ok(await noHorizontalOverflow(host));
 await quietToasts(host);
 await screenshot('screenshare-speaker-pinned-host',host);
 await host.setViewportSize({width:390,height:844});
 assert.ok(await noHorizontalOverflow(host),'speaker view overflows at 390px');
 await screenshot('screenshare-speaker-390-host',host);
 await host.setViewportSize({width:1280,height:720});
 await host.getByRole('button',{name:'Unpin Guest B'}).click();
 await host.waitForFunction(()=>document.querySelector('#video-grid > .tile-stage .tile-name')?.textContent==='Guest A');
 await (await revealControl(host,host.getByRole('button',{name:'Show self'}))).click();
 assert.equal(await host.locator('#local-tile').isVisible(),true);
 await (await revealControl(host,host.getByRole('button',{name:'Grid',exact:true}))).click();
 assert.ok(await host.evaluate(()=>document.querySelector('#video-grid').classList.contains('layout-split')));
 console.log('speaker view, pin and hide-self work');

 assert.deepEqual(errors,[]);
 console.log(JSON.stringify({presentation:true,screenSender,screenAngle:{offsetSec:screenAngle.offsetSec,durationSec:screenAngle.durationSec,audio:!!screenAudio},hostStop:true,deviceSwitch:true,views:true,errors}));
}catch(e){
 console.error('page errors:',JSON.stringify(errors));
 for(const [name,page] of pages)if(!page.isClosed()){
  console.error(`--- ${name}:`,(await page.locator('body').innerText({timeout:2000}).catch(()=>'')).slice(-600));
  console.error(`--- ${name} media:`,JSON.stringify(await page.evaluate(()=>({tiles:[...document.querySelectorAll('#video-grid > .tile')].map(t=>`${t.id}.${t.className}`),pcs:window.__pcs.map(pc=>({state:pc.connectionState,sig:pc.signalingState,senders:pc.getSenders().map(s=>s.track?.kind+':'+s.track?.id.slice(0,6)),receivers:pc.getTransceivers().map(t=>t.mid+':'+t.direction+':'+t.currentDirection+':'+t.receiver.track.kind)}))})).catch(e=>String(e))));
 }
 console.error(logs.slice(-8000));throw e;
}finally{
 await browser?.close();
 if(daemon.exitCode===null&&daemon.signalCode===null){
  daemon.kill('SIGTERM');
  await Promise.race([new Promise(r=>daemon.once('exit',r)),wait(3000)]);
  if(daemon.exitCode===null&&daemon.signalCode===null)daemon.kill('SIGKILL');
 }
 await rm(data,{recursive:true,force:true});
}
