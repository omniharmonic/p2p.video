// Four-participant group call (host + 3 guests) against the real daemon with
// Chromium fake devices only, playback muted. Verifies: everyone decodes
// everyone with correct name labels, call sender caps follow the participant
// count, mic/camera indicators propagate, a failed PeerConnection recovers by
// re-offering (during a take) while local recording is unaffected, all four
// participants' tracks assemble, the grid has no horizontal overflow at 1280×720
// and 390px, and departed participants' tiles disappear everywhere.
// Build first; STUDIO_BINARY points at the daemon.
import assert from 'node:assert/strict';
import {spawn,execFileSync} from 'node:child_process';
import {mkdtemp,rm,readdir,mkdir} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
import {isolatedNetworkArgs,startTake,launchURL,readyToJoin} from './lib/harness.mjs';
const data=await mkdtemp(path.join(tmpdir(),'studio-group-'));
const port=Number(process.env.STUDIO_TEST_PORT||17553),origin=`http://127.0.0.1:${port}`;
const daemon=spawn(process.env.STUDIO_BINARY||'../bin/p2pvideod',['--data-dir',data,'--port',String(port),'--no-funnel',...await isolatedNetworkArgs()],{stdio:['ignore','pipe','pipe']});
let logs='';daemon.stderr.on('data',b=>logs+=b);daemon.stdout.on('data',b=>logs+=b);
const screenshotDir=process.env.STUDIO_SCREENSHOT_DIR;
if(screenshotDir)await mkdir(screenshotDir,{recursive:true});
const screenshot=async(name,page)=>{if(screenshotDir)await page.screenshot({path:path.join(screenshotDir,name+'.png'),animations:'disabled'});};
const errors=[];let browser;
const names=['Host','Guest A','Guest B','Guest C'];
const pages=new Map(); // name -> page
const others=name=>names.filter(n=>n!==name&&pages.has(n));
const wait=ms=>new Promise(r=>setTimeout(r,ms));

// Every remote tile on the page decodes video, frames keep advancing, and the
// labels are exactly the expected participant names.
async function assertDecodesEveryone(name,{timeout=30000}={}){
 const page=pages.get(name),expected=others(name).sort();
 await page.waitForFunction(expected=>{
  const tiles=[...document.querySelectorAll('.tile[data-participant-id]')];
  const labels=tiles.map(t=>t.querySelector('.tile-name')?.textContent).sort();
  return tiles.length===expected.length&&JSON.stringify(labels)===JSON.stringify(expected)&&tiles.every(t=>t.querySelector('video')?.videoWidth>0);
 },expected,{timeout});
 const frames=()=>page.evaluate(()=>[...document.querySelectorAll('.tile[data-participant-id] video')].map(v=>v.getVideoPlaybackQuality().totalVideoFrames));
 const before=await frames();
 await page.waitForFunction(before=>{const now=[...document.querySelectorAll('.tile[data-participant-id] video')].map(v=>v.getVideoPlaybackQuality().totalVideoFrames);return now.length===before.length&&now.every((n,i)=>n>before[i]+5);},before,{timeout:15000});
}
// Parameters of the live call senders (the last PeerConnection the page made).
const senderParams=page=>page.evaluate(()=>{
 const pc=window.__pcs.filter(p=>p.connectionState!=='closed').at(-1);
 return pc.getSenders().filter(s=>s.track).map(s=>{const p=s.getParameters();return {kind:s.track.kind,settings:s.track.kind==='video'?s.track.getSettings():null,degradationPreference:p.degradationPreference,encoding:p.encodings[0]};});
});
async function assertCallCaps(name,{maxBitrate,shortSide}){
 const page=pages.get(name);
 await page.waitForFunction(maxBitrate=>{const pc=window.__pcs.filter(p=>p.connectionState!=='closed').at(-1);const v=pc?.getSenders().find(s=>s.track?.kind==='video');return v?.getParameters().encodings[0]?.maxBitrate===maxBitrate;},maxBitrate,{timeout:10000});
 const params=await senderParams(page);
 const video=params.find(p=>p.kind==='video'),audio=params.find(p=>p.kind==='audio');
 const capShort=Math.min(video.settings.width,video.settings.height);
 assert.equal(video.encoding.maxBitrate,maxBitrate,name);
 assert.equal(video.encoding.scaleResolutionDownBy,capShort>shortSide?Math.round(capShort/shortSide*1000)/1000:1,`${name} scale for ${video.settings.width}x${video.settings.height}`);
 assert.equal(audio.encoding.maxBitrate,64000,`${name} audio`);
 return {capture:`${video.settings.width}x${video.settings.height}`,...video.encoding,degradationPreference:video.degradationPreference};
}
const noHorizontalOverflow=page=>page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth);

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
 await host.locator('#studio-view').waitFor({state:'visible'});console.log('host in studio');
 for(const name of names.slice(1)){
  const guest=await open(name);
  await guest.goto(`${origin}/join?session=${session.id}&token=${session.token}`);
  await guest.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
  await guest.locator('#name-input').fill(name);await guest.locator('#join-btn').click();
  await host.waitForFunction(name=>document.querySelector('#host-admission-banner').style.display==='flex'&&document.querySelector('#admission-guest-name').textContent===name,name);
  await host.locator('#admit-btn').click();await guest.locator('#studio-view').waitFor({state:'visible'});
  console.log(`${name} admitted`);
 }
 for(const name of names)await assertDecodesEveryone(name);
 console.log('all four pages decode three remote videos with correct labels');

 // Sender caps: 4 participants → 540p tier.
 const caps4={};for(const name of names)caps4[name]=await assertCallCaps(name,{maxBitrate:900_000,shortSide:540});
 console.log('call caps (4 participants)',JSON.stringify(caps4.Host));

 // Mic/camera indicators propagate via participant_state.
 const a=pages.get('Guest A');
 await a.locator('#toggle-mic-btn').click();await a.locator('#toggle-cam-btn').click();
 for(const name of others('Guest A'))await pages.get(name).waitForFunction(()=>{const tile=[...document.querySelectorAll('.tile[data-participant-id]')].find(t=>t.querySelector('.tile-name').textContent==='Guest A');return tile&&!tile.querySelector('.tile-state-mic').hidden&&!tile.querySelector('.tile-state-cam').hidden&&tile.classList.contains('cam-off');});
 assert.equal(await a.locator('#local-tile .tile-state-mic').isVisible(),true);
 await host.waitForFunction(()=>document.querySelectorAll('.toast').length===0);
 await screenshot('group-01-host-1280',host);await screenshot('group-02-guest-1280',a);
 await a.locator('#toggle-mic-btn').click();await a.locator('#toggle-cam-btn').click();
 for(const name of others('Guest A'))await pages.get(name).waitForFunction(()=>{const tile=[...document.querySelectorAll('.tile[data-participant-id]')].find(t=>t.querySelector('.tile-name').textContent==='Guest A');return tile&&tile.querySelector('.tile-state-mic').hidden&&tile.querySelector('.tile-state-cam').hidden&&!tile.classList.contains('cam-off');});
 console.log('mic/camera indicators propagate');

 // Layout: 2×2 at 1280×720 without horizontal overflow, two columns at 390px.
 for(const name of names)assert.ok(await noHorizontalOverflow(pages.get(name)),`${name} overflows at 1280`);
 const columns=async page=>page.evaluate(()=>new Set([...document.querySelectorAll('#video-grid > .tile')].map(t=>Math.round(t.getBoundingClientRect().left))).size);
 assert.equal(await columns(host),2,'four tiles lay out as 2 columns');
 const fitsViewport=await host.evaluate(()=>[...document.querySelectorAll('#video-grid > .tile')].every(t=>t.getBoundingClientRect().right<=window.innerWidth));
 assert.ok(fitsViewport);
 await host.setViewportSize({width:390,height:844});
 assert.ok(await noHorizontalOverflow(host),'host overflows at 390px');
 assert.equal(await columns(host),2,'four tiles lay out as 2 columns on mobile');
 await screenshot('group-03-host-390',host);
 await host.setViewportSize({width:1280,height:720});

 // Record, and while recording kill one participant's call media: the client
 // must re-offer, the SFU must replace its peer, everyone must re-subscribe,
 // and recording must not notice.
 await startTake(host);
 for(const name of names)await pages.get(name).waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='inline-flex');
 console.log('recording');
 await wait(1500);
 const b=pages.get('Guest B');
 const pcsBefore=await b.evaluate(()=>window.__pcs.length);
 await b.evaluate(()=>{const pc=window.__pcs.at(-1);Object.defineProperty(pc,'connectionState',{get:()=>'failed',configurable:true});pc.onconnectionstatechange?.(new Event('connectionstatechange'));});
 await b.waitForFunction(n=>window.__pcs.length>n&&window.__pcs.at(-1).connectionState==='connected',pcsBefore,{timeout:20000});
 assert.equal(await b.evaluate(()=>window.__pcs.at(-2).signalingState),'closed','failed PeerConnection was not replaced');
 for(const name of names)await assertDecodesEveryone(name);
 assert.equal(await b.locator('#rec-indicator').isVisible(),true,'media recovery interrupted local recording');
 console.log('media recovered mid-take; everyone re-subscribed');
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
 assert.ok(snapshot.tracks.every(t=>t.status==='assembled'&&t.totalBytes>1000),JSON.stringify(snapshot));
 assert.equal(snapshot.missingParticipants?.length??0,0,JSON.stringify(snapshot));
 const raw=path.join(data,'sessions',session.id,'takes',snapshot.take.id,'raw');
 const media=(await readdir(raw)).filter(f=>/\.(webm|mp4|wav)$/.test(f));
 assert.equal(media.length,4);
 for(const file of media){
  const info=JSON.parse(execFileSync('ffprobe',['-v','error','-show_streams','-of','json',path.join(raw,file)],{encoding:'utf8'}));
  assert.ok(info.streams.some(s=>s.codec_type==='video')&&info.streams.some(s=>s.codec_type==='audio'),file);
  execFileSync('ffmpeg',['-v','error','-i',path.join(raw,file),'-f','null','-'],{stdio:'pipe'});
 }
 console.log('four participants assembled and decode');

 // Departures: tiles vanish everywhere; caps rise as the room shrinks.
 await pages.get('Guest C').close();pages.delete('Guest C');
 for(const name of pages.keys())await assertDecodesEveryone(name);
 for(const name of pages.keys())await assertCallCaps(name,{maxBitrate:900_000,shortSide:540});
 await pages.get('Guest B').close();pages.delete('Guest B');
 for(const name of pages.keys())await assertDecodesEveryone(name);
 const caps2=await assertCallCaps('Host',{maxBitrate:1_500_000,shortSide:720});
 await assertCallCaps('Guest A',{maxBitrate:1_500_000,shortSide:720});
 console.log('departed tiles removed; caps back to 720p tier',JSON.stringify(caps2));

 assert.deepEqual(errors,[]);
 console.log(JSON.stringify({participants:4,everyoneDecodesEveryone:true,labels:true,indicators:true,mediaRecoveredDuringTake:true,assembledTracks:snapshot.tracks.length,decodedFiles:media.length,leaveRemovesTiles:true,caps4:caps4.Host,caps2,errors}));
}catch(e){
 console.error('page errors:',JSON.stringify(errors));
 for(const [name,page] of pages)if(!page.isClosed())console.error(`--- ${name}:`,(await page.locator('body').innerText({timeout:2000}).catch(()=>'')).slice(-1500));
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
