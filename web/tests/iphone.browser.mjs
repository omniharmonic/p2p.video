// iPhone Safari guest [SL-P1-12]: a Chromium host plus a WebKit guest with
// Playwright's iPhone 13 profile (390 px wide, touch, mobile user agent,
// device scale 3) in an ephemeral context — like a Safari private window, it
// has no OPFS, so recording runs on the memory backend (lib/opfs.ts).
//
// iOS autoplay policy is simulated in the page (macOS WebKit doesn't enforce
// it the way iOS does): an unmuted play() outside a user gesture rejects with
// NotAllowedError. Capture is WebKit's page-level synthetic stream (see
// lib/browsers.mjs). What this can't prove: real iOS hardware, the iOS
// MediaRecorder/encoder, real Safari private-mode quotas, the on-screen
// keyboard, rotation and the home indicator inset.
//
// Verifies: the green room at 390 px has no horizontal overflow, the preview
// is playsinline, the join button is on screen and the browser note names the
// private-window limitation; in the call the layout fits 390 px, the dock sits
// inside the viewport with its main controls on screen and ≥ 40 px tap
// targets, every remote video is playsinline and decodes; remote media held
// back by the autoplay policy starts muted with a "Turn on call sound"
// button, and one tap unmutes it (playing, inbound audio flowing); a take
// records on the memory backend, uploads during the take and assembles as
// MP4 (H.264 + AAC) with video and audio. Build first; STUDIO_BINARY is the daemon.
import assert from 'node:assert/strict';
import {spawn,execFileSync} from 'node:child_process';
import {mkdtemp,rm,readdir,mkdir} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {isolatedNetworkArgs,startTake,launchURL,readyToJoin} from './lib/harness.mjs';
import {launchBrowser,contextOptions,prepareContext,closeBrowser} from './lib/browsers.mjs';
const pw=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const data=await mkdtemp(path.join(tmpdir(),'studio-iphone-'));
const port=Number(process.env.STUDIO_TEST_PORT||18443),origin=`http://127.0.0.1:${port}`;
const daemon=spawn(process.env.STUDIO_BINARY||'../bin/p2pvideod',['--data-dir',data,'--port',String(port),'--no-funnel',...await isolatedNetworkArgs()],{stdio:['ignore','pipe','pipe']});
let logs='';daemon.stderr.on('data',b=>logs+=b);daemon.stdout.on('data',b=>logs+=b);
const screenshotDir=process.env.STUDIO_SCREENSHOT_DIR;
if(screenshotDir)await mkdir(screenshotDir,{recursive:true});
const screenshot=async(name,page)=>{if(screenshotDir)await page.screenshot({path:path.join(screenshotDir,name+'.png'),animations:'disabled'});};
const errors=[];const browsers=[];
const wait=ms=>new Promise(r=>setTimeout(r,ms));
const noHorizontalOverflow=page=>page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth);
let host,phone;

// iOS autoplay policy: unmuted media may only start from inside a user
// gesture's event handling (strict: the same task as the trusted event).
const iosAutoplayPolicy=()=>{
 let inGesture=false;
 for(const type of ['pointerdown','touchstart','touchend','click','keydown'])addEventListener(type,e=>{if(e.isTrusted){inGesture=true;setTimeout(()=>{inGesture=false;},0);}},true);
 const play=HTMLMediaElement.prototype.play;
 window.__autoplayRefusals=0;
 HTMLMediaElement.prototype.play=function(){
  if(!this.muted&&!inGesture){window.__autoplayRefusals++;this.pause();return Promise.reject(new DOMException('The request is not allowed by the user agent or the platform in the current context, possibly because the user denied permission.','NotAllowedError'));}
  return play.call(this);
 };
 // <video autoplay> follows the same rule: start it through play().
 const desc=Object.getOwnPropertyDescriptor(HTMLMediaElement.prototype,'srcObject');
 Object.defineProperty(HTMLMediaElement.prototype,'srcObject',{configurable:true,get(){return desc.get.call(this);},set(v){desc.set.call(this,v);if(this.autoplay&&!this.muted&&!inGesture){this.autoplay=false;this.dataset.autoplayHeld='1';}}});
};

try{
 let ready=false;for(let i=0;i<100;i++){try{ready=(await fetch(origin+'/health')).ok;if(ready)break;}catch{}await wait(100);}assert.ok(ready,'daemon did not start: '+logs);
 const hb=await launchBrowser(pw,'chromium',{chromiumArgs:['--mute-audio']});browsers.push(hb);
 const hc=await hb.newContext(contextOptions('chromium',{viewport:{width:1280,height:800}}));await prepareContext(hc,'chromium');
 host=await hc.newPage();host.setDefaultTimeout(45000);
 const wb=await launchBrowser(pw,'webkit');browsers.push(wb);
 const pc=await wb.newContext({...pw.devices['iPhone 13']});await prepareContext(pc,'webkit');await pc.addInitScript(iosAutoplayPolicy);
 phone=await pc.newPage();phone.setDefaultTimeout(45000);
 for(const [name,page] of [['Host',host],['iPhone',phone]]){
  page.on('console',m=>{if(m.type()==='error'&&!/422|NotAllowedError|Button failed to load, iconName/.test(m.text()))errors.push(`${name} console: ${m.text()}`);});
  page.on('pageerror',e=>errors.push(`${name}: ${e.message}`));
 }
 await host.goto(await launchURL(data,origin));await readyToJoin(host);
 await host.locator('#name-input').fill('Host');
 const created=host.waitForResponse(r=>r.url().endsWith('/api/v1/sessions')&&r.request().method()==='POST');
 await host.locator('#join-btn').click();const session=await(await created).json();
 await host.locator('#studio-view').waitFor({state:'visible'});

 // ---- Green room at 390 px.
 await phone.goto(`${origin}/join?session=${session.id}&token=${session.token}`);
 await phone.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
 const viewport=await phone.evaluate(()=>({w:innerWidth,h:innerHeight,ua:navigator.userAgent,touch:navigator.maxTouchPoints}));
 assert.equal(viewport.w,390);assert.match(viewport.ua,/iPhone/);
 assert.ok(await noHorizontalOverflow(phone),'green room overflows at 390 px');
 assert.equal(await phone.evaluate(()=>document.querySelector('#preview-video').playsInline),true,'preview must be playsinline');
 const note=phone.locator('#browser-note');await note.waitFor({state:'visible'});
 const noteText=await note.innerText();
 assert.match(noteText,/Safari on iPhone/);assert.match(noteText,/private window/);
 assert.match(await note.getAttribute('data-limitations'),/\bstorage\b/);
 // Pre-flight: local recording storage is a warning (memory backend), not a block.
 await phone.waitForFunction(()=>/Private browsing/.test(document.querySelector('[data-preflight-id="opfs"]')?.textContent||''));
 const storageItem=await phone.locator('[data-preflight-id="opfs"]').first().evaluate(e=>({cls:e.className,text:e.textContent}));
 assert.equal(storageItem.cls,'pf-warn',JSON.stringify(storageItem));
 await screenshot('iphone-01-green-room',phone);
 await phone.locator('#name-input').fill('iPhone guest');
 await phone.locator('#join-btn').scrollIntoViewIfNeeded();
 const joinBox=await phone.locator('#join-btn').boundingBox();
 assert.ok(joinBox&&joinBox.x>=0&&joinBox.x+joinBox.width<=390,'join button off screen');
 await phone.locator('#join-btn').tap();
 await host.waitForFunction(()=>document.querySelector('#host-admission-banner').style.display==='flex');
 await host.locator('#admit-btn').click();await phone.locator('#studio-view').waitFor({state:'visible'});
 console.log('iPhone guest admitted; note:',noteText);

 // ---- Call layout at 390 px.
 await phone.waitForFunction(()=>{const v=[...document.querySelectorAll('.tile[data-participant-id] video')];return v.length===1&&v[0].videoWidth>0;},null,{timeout:45000});
 const layout=await phone.evaluate(()=>{
  const dock=document.querySelector('.dock').getBoundingClientRect();
  const controls=['#toggle-mic-btn','#toggle-cam-btn','#leave-btn'].map(sel=>document.querySelector(sel)).filter(Boolean).map(el=>{const r=el.getBoundingClientRect();return {id:el.id,w:r.width,h:r.height,left:r.left,right:r.right,top:r.top,bottom:r.bottom,visible:!!el.offsetParent};});
  return {overflow:document.documentElement.scrollWidth>innerWidth,dock:{top:dock.top,bottom:dock.bottom,left:dock.left,right:dock.right},inner:{w:innerWidth,h:innerHeight},controls,remotePlaysInline:[...document.querySelectorAll('.tile video')].every(v=>v.playsInline),fullscreen:[...document.querySelectorAll('video')].some(v=>v.webkitDisplayingFullscreen)};
 });
 assert.equal(layout.overflow,false,'call overflows at 390 px');
 assert.ok(layout.dock.bottom<=layout.inner.h+1&&layout.dock.top>=0,`dock outside viewport ${JSON.stringify(layout)}`);
 assert.ok(layout.controls.length>=2,`dock controls missing ${JSON.stringify(layout.controls)}`);
 for(const c of layout.controls){assert.ok(c.visible&&c.left>=0&&c.right<=390&&c.bottom<=layout.inner.h+1,`${c.id} off screen ${JSON.stringify(c)}`);assert.ok(c.w>=40&&c.h>=40,`${c.id} tap target ${c.w}x${c.h}`);}
 assert.equal(layout.remotePlaysInline,true,'remote video must be playsinline');assert.equal(layout.fullscreen,false);
 console.log('layout at 390',JSON.stringify(layout));

 // ---- Autoplay with sound after a gesture.
 const unlock=phone.locator('#sound-unlock-btn');
 await unlock.waitFor({state:'visible'});
 const before=await phone.evaluate(()=>({refusals:window.__autoplayRefusals,media:[...document.querySelectorAll('.tile[data-participant-id] video')].map(v=>({muted:v.muted,paused:v.paused}))}));
 assert.ok(before.refusals>0&&before.media.every(m=>m.muted),`held media should start muted ${JSON.stringify(before)}`);
 // The picture still plays while sound waits for the tap.
 const f0=await phone.evaluate(()=>window.__frames(document.querySelector('.tile[data-participant-id] video')));
 await phone.waitForFunction(f0=>window.__frames(document.querySelector('.tile[data-participant-id] video'))>f0+5,f0,{timeout:20000});
 await screenshot('iphone-02-call-sound-held',phone);
 await unlock.tap();
 await unlock.waitFor({state:'detached'});
 await phone.waitForFunction(()=>[...document.querySelectorAll('.tile[data-participant-id] video')].every(v=>!v.muted&&!v.paused));
 const audioBytes=()=>phone.evaluate(async()=>{const pc=window.__pcs.filter(p=>p.connectionState==='connected').at(-1);let n=0;(await pc.getStats()).forEach(s=>{if(s.type==='inbound-rtp'&&s.kind==='audio')n+=s.bytesReceived;});return n;});
 const a0=await audioBytes();await wait(1500);const a1=await audioBytes();
 assert.ok(a1>a0+1000,`inbound audio not flowing ${a0}->${a1}`);
 console.log('autoplay: held muted, one tap turned call sound on');
 await screenshot('iphone-03-call',phone);

 // ---- A take on the memory backend.
 await startTake(host);
 await phone.waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='inline-flex',null,{timeout:20000});
 assert.match(await phone.locator('#telemetry-storage').innerText(),/Private browsing/);
 let acked=0;for(let i=0;i<60&&!acked;i++){await wait(250);acked=await phone.evaluate(()=>parseFloat((document.querySelector('#telemetry-chunks')?.textContent||'').match(/([0-9.]+) MB saved on the host/)?.[1]||'0'));}
 assert.ok(acked>0,'nothing uploaded during the take');
 await wait(3000);
 await host.locator('#rec-btn').click();
 const headers={Authorization:`Bearer ${session.hostToken}`};let snapshot;
 for(let i=0;i<300;i++){const detail=await(await fetch(`${origin}/api/v1/sessions/${session.id}`,{headers})).json();const take=detail.takes[0];if(take){snapshot=await(await fetch(`${origin}/api/v1/takes/${take.id}`,{headers})).json();if(snapshot.tracks.length===2&&snapshot.tracks.every(t=>t.status==='assembled'))break;}await wait(150);}
 assert.ok(snapshot.tracks.length===2&&snapshot.tracks.every(t=>t.status==='assembled'&&t.totalBytes>1000),JSON.stringify(snapshot));
 const track=snapshot.tracks.find(t=>t.participantName==='iPhone guest');assert.ok(track);
 const raw=path.join(data,'sessions',session.id,'takes',snapshot.take.id,'raw');
 const file=(await readdir(raw)).find(f=>f.startsWith(track.trackId)&&/\.(mp4|webm)$/.test(f));
 const info=JSON.parse(execFileSync('ffprobe',['-v','error','-show_streams','-of','json',path.join(raw,file)],{encoding:'utf8'}));
 const codecs=info.streams.map(s=>`${s.codec_type}:${s.codec_name}`);
 assert.ok(codecs.some(c=>c.startsWith('video:'))&&codecs.some(c=>c.startsWith('audio:')),codecs.join());
 execFileSync('ffmpeg',['-v','error','-i',path.join(raw,file),'-f','null','-'],{stdio:'pipe'});
 await phone.waitForFunction(()=>/saved on the host/.test(document.querySelector('#telemetry-status').textContent),null,{timeout:30000});
 // Acknowledged fragments are dropped from memory.
 const held=await phone.evaluate(async()=>{const root=await navigator.storage.getDirectory().catch(()=>null);return root===null;});
 assert.equal(held,true,'this context should have no OPFS (private-window condition)');
 assert.ok(await noHorizontalOverflow(phone),'transfers view overflows at 390 px');
 await screenshot('iphone-04-after-take',phone);
 assert.deepEqual(errors,[]);
 console.log(JSON.stringify({device:'iPhone 13 (WebKit emulation)',viewport,note:noteText,storagePreflight:storageItem,layout:{dock:layout.dock,controls:layout.controls.map(c=>`${c.id} ${Math.round(c.w)}x${Math.round(c.h)}`)},autoplayRefusals:before.refusals,soundAfterTap:true,acknowledgedMbDuringTake:acked,original:{file:path.extname(file),codecs},errors}));
}catch(e){
 console.error('page errors:',JSON.stringify(errors));
 for(const [name,page] of [['Host',host],['iPhone',phone]])if(page&&!page.isClosed())console.error(`--- ${name}:`,(await page.locator('body').innerText({timeout:2000}).catch(()=>'')).slice(-1500));
 console.error(logs.slice(-6000));throw e;
}finally{
 for(const b of browsers)await closeBrowser(b);
 if(daemon.exitCode===null&&daemon.signalCode===null){
  daemon.kill('SIGTERM');
  await Promise.race([new Promise(r=>daemon.once('exit',r)),wait(3000)]);
  if(daemon.exitCode===null&&daemon.signalCode===null)daemon.kill('SIGKILL');
 }
 await rm(data,{recursive:true,force:true});
}
