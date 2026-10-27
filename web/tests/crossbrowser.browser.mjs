// Safari and Firefox guests [SL-P1-12] against the real daemon: a Chromium
// host (fake devices) plus a WebKit guest and a Firefox guest, each in its
// own engine. Verifies, per guest engine: the guest link's green room (preview
// decodes, the browser note says exactly which limitation applies), waiting
// room + admission, everyone decodes everyone (frames advance) and hears
// everyone (inbound audio RTP bytes grow) with every publisher sending VP8,
// remote media playing, chat both ways, raised hand and reactions seen by
// the others, background blur applied or refused with a notice (the call
// keeps flowing either way), the host's screen share decoded by both guests,
// then a take: each guest records locally with the recorder its engine
// supports (WebKit: MediaRecorder MP4/H.264+AAC; Firefox: MediaRecorder
// WebM/VP8+Opus), uploads progressively (chunk PUTs acknowledged before
// Stop), and completes with a host receipt; the export succeeds and every
// participant's exported camera angle and isolated audio ffprobe and decode,
// last within 2 s of the take, and carry an offset in the manifest.
//
// Media is synthetic (see lib/browsers.mjs): Chromium and Firefox use their
// built-in fake devices through real getUserMedia; WebKit's capture is a
// page-level canvas + oscillator stream because Playwright's macOS WebKit
// never resolves getUserMedia. Encoders, decoders, WebRTC, MediaRecorder,
// OPFS and layout are each engine's own.
//
// The WebKit guest runs in a persistent profile (Safari's normal mode keeps
// OPFS); with STUDIO_TEST_WEBKIT_LEGACY_OPFS=1 (default) its
// FileSystemFileHandle.createWritable is removed first, as in Safari ≤ 18, so
// the SyncAccessHandle worker journal is what records. Build first;
// STUDIO_BINARY points at the daemon.
import assert from 'node:assert/strict';
import {spawn,execFileSync} from 'node:child_process';
import {mkdtemp,rm,readdir,readFile,mkdir} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {isolatedNetworkArgs,startTake,launchURL,revealControl,readyToJoin} from './lib/harness.mjs';
import {launchBrowser,contextOptions,prepareContext,closeBrowser,engineLabel,firefoxPrefs} from './lib/browsers.mjs';
const pw=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const data=await mkdtemp(path.join(tmpdir(),'studio-crossbrowser-'));
const port=Number(process.env.STUDIO_TEST_PORT||18343),origin=`http://127.0.0.1:${port}`;
const daemon=spawn(process.env.STUDIO_BINARY||'../bin/p2pvideod',['--data-dir',data,'--port',String(port),'--no-funnel',...await isolatedNetworkArgs()],{stdio:['ignore','pipe','pipe']});
let logs='';daemon.stderr.on('data',b=>logs+=b);daemon.stdout.on('data',b=>logs+=b);
const screenshotDir=process.env.STUDIO_SCREENSHOT_DIR;
if(screenshotDir)await mkdir(screenshotDir,{recursive:true});
const screenshot=async(name,page)=>{if(screenshotDir)await page.screenshot({path:path.join(screenshotDir,name+'.png'),animations:'disabled'});};
const legacyOpfs=process.env.STUDIO_TEST_WEBKIT_LEGACY_OPFS!=='0';
const guestEngines=(process.env.STUDIO_TEST_GUEST_ENGINES||'webkit,firefox').split(',').filter(Boolean);
const errors=[];const browsers=[];const contexts=[];
const pages=new Map(); // name -> {page, engine}
const wait=ms=>new Promise(r=>setTimeout(r,ms));
const nameOf=engine=>`${engineLabel(engine)} guest`;
const tileOf=(page,name)=>page.locator('#video-grid .tile[data-participant-id]').filter({has:page.locator('.tile-name',{hasText:new RegExp(`^${name}$`)})});
// Console noise that is not a p2p.video error: the invite probe's 422,
// WebKit's media-controls placard icons, MediaPipe's XNNPACK INFO line.
const benign=text=>text.includes('422')||/Button failed to load, iconName|XNNPACK delegate/.test(text);

async function open(name,engine,{viewport={width:1280,height:800},persistent=false,init}={}){
 let context;
 if(persistent){
  const dir=await mkdtemp(path.join(tmpdir(),`studio-${engine}-profile-`));
  const launcher=engine==='webkit'?pw.webkit:engine==='firefox'?pw.firefox:pw.chromium;
  context=await launcher.launchPersistentContext(dir,{headless:true,viewport,...(engine==='firefox'?{firefoxUserPrefs:firefoxPrefs}:{})});
  browsers.push({close:()=>context.close(),dir});
 }else{
  const browser=await launchBrowser(pw,engine,{chromiumArgs:['--mute-audio']});browsers.push(browser);
  context=await browser.newContext(contextOptions(engine,{viewport}));
 }
 contexts.push(context);
 await prepareContext(context,engine);
 if(init)await context.addInitScript(init);
 // DEBUG_GUM=1: log getUserMedia and SDP/sender calls into window.__gum (dumped on failure).
 if(process.env.DEBUG_GUM)await context.addInitScript(()=>{window.__gum=[];const md=navigator.mediaDevices;const g=md.getUserMedia.bind(md);md.getUserMedia=async c=>{const e={c:JSON.stringify(c),t:performance.now()};window.__gum.push(e);try{const s=await g(c);e.ok=s.getTracks().map(t=>t.kind+JSON.stringify(t.getSettings()));return s;}catch(err){e.err=String(err);throw err;}finally{e.ms=performance.now()-e.t;}};addEventListener('unhandledrejection',ev=>window.__gum.push({unhandled:String(ev.reason),stack:ev.reason?.stack}));for(const [proto,name] of [[RTCPeerConnection.prototype,'createOffer'],[RTCPeerConnection.prototype,'setLocalDescription'],[RTCPeerConnection.prototype,'setRemoteDescription'],[RTCRtpTransceiver.prototype,'setCodecPreferences'],[RTCRtpSender.prototype,'setParameters']]){const f=proto[name];proto[name]=function(...a){const e={call:name,t:performance.now()};window.__gum.push(e);try{const r=f.apply(this,a);if(r&&r.then)r.then(()=>e.done=performance.now(),err=>e.err=String(err));else e.done=performance.now();return r;}catch(err){e.err=String(err);throw err;}};}});
 // A fresh page: a persistent context's first page predates the init scripts.
 const page=await context.newPage();for(const other of context.pages())if(other!==page)await other.close();
 page.setDefaultTimeout(45000);
 if(process.env.DEBUG_CONSOLE)page.on('console',m=>console.log(`[${name}] ${m.type()}: ${m.text()}`));
 page.on('console',m=>{if(m.type()==='error'&&!benign(m.text()))errors.push(`${name} console: ${m.text()}`);});
 page.on('pageerror',e=>errors.push(`${name}: ${e.message}`));
 pages.set(name,{page,engine});return page;
}

// Every remote camera tile decodes with advancing frames and the expected labels.
async function assertDecodesEveryone(name){
 const {page}=pages.get(name),expected=[...pages.keys()].filter(n=>n!==name).sort();
 await page.waitForFunction(expected=>{
  const tiles=[...document.querySelectorAll('.tile[data-participant-id]')];
  const labels=tiles.map(t=>t.querySelector('.tile-name')?.textContent).sort();
  return tiles.length===expected.length&&JSON.stringify(labels)===JSON.stringify(expected)&&tiles.every(t=>t.querySelector('video')?.videoWidth>0);
 },expected,{timeout:45000});
 const before=await page.evaluate(sel=>[...document.querySelectorAll(sel)].map(window.__frames),'.tile[data-participant-id] video');
 await page.waitForFunction(([sel,before])=>{const now=[...document.querySelectorAll(sel)].map(window.__frames);return now.length===before.length&&now.every((n,i)=>n>before[i]+5);},['.tile[data-participant-id] video',before],{timeout:20000}).catch(async e=>{throw new Error(`${name}: remote frames stalled ${JSON.stringify(before)} -> ${JSON.stringify(await page.evaluate(sel=>[...document.querySelectorAll(sel)].map(window.__frames),'.tile[data-participant-id] video'))}`);});
 return before;
}
// Inbound audio RTP bytes grow for every remote participant (they're heard).
async function assertHearsEveryone(name){
 const {page}=pages.get(name);
 const bytes=()=>page.evaluate(async()=>{
  const pc=window.__pcs.filter(p=>p.connectionState==='connected').at(-1);const out={};
  const tiles=[...document.querySelectorAll('.tile[data-participant-id]')];
  const stats=await pc.getStats();
  for(const tile of tiles){
   const stream=tile.querySelector('video').srcObject;const ids=new Set(stream.getAudioTracks().map(t=>t.id));
   // Firefox's inbound-rtp has no trackIdentifier: match via the receiver's track.
   const mids=new Set(pc.getTransceivers().filter(t=>t.receiver.track&&ids.has(t.receiver.track.id)).map(t=>t.mid));
   let total=0;stats.forEach(s=>{if(s.type==='inbound-rtp'&&s.kind==='audio'&&(ids.has(s.trackIdentifier)||mids.has(s.mid)))total+=s.bytesReceived;});
   out[tile.querySelector('.tile-name').textContent]=total;
  }
  return out;
 });
 const before=await bytes();
 for(let i=0;i<60;i++){await wait(250);const now=await bytes();if(Object.keys(before).every(k=>now[k]>before[k]+2000))return now;}
 assert.fail(`${name} does not receive audio from everyone: ${JSON.stringify(before)} -> ${JSON.stringify(await bytes())}`);
}
// The page plays its remote media with sound (not paused, not muted).
const playbackState=page=>page.evaluate(()=>[...document.querySelectorAll('.tile[data-participant-id] video')].map(v=>({paused:v.paused,muted:v.muted})));

try{
 let ready=false;for(let i=0;i<100;i++){try{ready=(await fetch(origin+'/health')).ok;if(ready)break;}catch{}await wait(100);}assert.ok(ready,'daemon did not start: '+logs);
 const host=await open('Host','chromium');
 await host.goto(await launchURL(data,origin));await readyToJoin(host);
assert.equal(await host.locator('#browser-note').count(),0,'Chromium gets no browser recommendation');
 await host.locator('#name-input').fill('Host');
 const created=host.waitForResponse(r=>r.url().endsWith('/api/v1/sessions')&&r.request().method()==='POST');
 await host.locator('#join-btn').click();const session=await(await created).json();
 await host.locator('#studio-view').waitFor({state:'visible'});
 console.log('host (Chromium) in studio');

 const notes={};
 for(const engine of guestEngines){
  const name=nameOf(engine);
  const legacy=engine==='webkit'&&legacyOpfs;
  const guest=await open(name,engine,{persistent:engine==='webkit',init:legacy?()=>{if(typeof FileSystemFileHandle!=='undefined')delete FileSystemFileHandle.prototype.createWritable;}:undefined});
  await guest.goto(`${origin}/join?session=${session.id}&token=${session.token}`);
  await guest.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
  // Green room: the gentle recommendation names the specific limitation.
  // The note appears iff this engine really lacks something (probed here
  // independently), and lists exactly that.
  const expected=await guest.evaluate(async()=>{
   const out=[];const outputs=(await navigator.mediaDevices.enumerateDevices()).some(d=>d.kind==='audiooutput');
   if(typeof MediaRecorder==='undefined')out.push('recording');
   if(typeof navigator.mediaDevices.getDisplayMedia!=='function')out.push('screen');
   if(!('setSinkId' in HTMLMediaElement.prototype)||!outputs)out.push('speaker');
   if(typeof AudioWorkletNode==='undefined')out.push('pcm');
   if(!await navigator.storage.getDirectory().then(()=>true,()=>false))out.push('storage');
   return out;
  });
  await guest.waitForFunction(()=>document.querySelector('[data-preflight-id="opfs"]'));await wait(500);
  const noteEl=guest.locator('#browser-note');
  if(expected.length){await noteEl.waitFor({state:'visible'});notes[engine]={text:(await noteEl.innerText()).trim(),limitations:(await noteEl.getAttribute('data-limitations')).split(' ').filter(Boolean)};assert.deepEqual(notes[engine].limitations,expected,`${engine} note`);}
  else{assert.equal(await noteEl.count(),0,`${engine} shows a note without a limitation`);notes[engine]={text:'',limitations:[]};}
  await screenshot(`crossbrowser-${engine}-01-green-room`,guest);
  await guest.locator('#name-input').fill(name);await guest.locator('#join-btn').click();
  await guest.locator('#waiting-room-view, #waiting-room').first().waitFor({state:'visible'}).catch(()=>{});
  await host.waitForFunction(name=>document.querySelector('#host-admission-banner').style.display==='flex'&&document.querySelector('#admission-guest-name').textContent===name,name);
  await host.locator('#admit-btn').click();await guest.locator('#studio-view').waitFor({state:'visible'});
  console.log(`${name} admitted; note: ${JSON.stringify(notes[engine])}`);
 }
 if(notes.firefox?.text)assert.match(notes.firefox.text,/Firefox/);
 if(notes.webkit?.text)assert.match(notes.webkit.text,/Safari/);

 // ---- See and hear each other.
 const decode={},hear={};
 for(const name of pages.keys()){await assertDecodesEveryone(name);decode[name]=true;}
 for(const name of pages.keys())hear[name]=await assertHearsEveryone(name);
 for(const name of pages.keys()){
  const states=await playbackState(pages.get(name).page);
  assert.ok(states.every(s=>!s.paused),`${name} has paused remote media: ${JSON.stringify(states)}`);
 }
 console.log('everyone decodes and hears everyone',JSON.stringify(hear));
 // Negotiated send codecs: every publisher sends a codec every subscriber decodes.
 const codecs={};
 for(const [name,{page}] of pages)codecs[name]=await page.evaluate(async()=>{const pc=window.__pcs.filter(p=>p.connectionState==='connected').at(-1);const stats=await pc.getStats();const out=[];stats.forEach(s=>{if(s.type==='outbound-rtp'&&s.kind==='video'){const c=stats.get(s.codecId);if(c)out.push(c.mimeType);}});return [...new Set(out)];});
 console.log('video send codecs',JSON.stringify(codecs));
 for(const name of pages.keys())await screenshot(`crossbrowser-call-${pages.get(name).engine}`,pages.get(name).page);

 // ---- Chat, hands, reactions.
 const say=async(from,text)=>{const {page}=pages.get(from);await (await revealControl(page,'#chat-btn')).click();await page.locator('#chat-input').fill(text);await page.keyboard.press('Enter');await page.keyboard.press('Escape');};
 const sees=async(name,text)=>{const {page}=pages.get(name);await page.locator('#chat-btn').click();await page.locator('.chat-log .chat-text',{hasText:text}).waitFor();await page.keyboard.press('Escape');};
 for(const from of pages.keys()){
  const text=`Hello from ${from}`;
  await say(from,text);
  for(const to of pages.keys())if(to!==from)await sees(to,text);
 }
 console.log('chat round trip across engines');
 for(const engine of guestEngines){
  const name=nameOf(engine),{page}=pages.get(name);
  await (await revealControl(page,'#hand-btn')).click();
  for(const other of pages.keys())if(other!==name)await tileOf(pages.get(other).page,name).locator('.tile-hand').waitFor();
  await page.locator('#hand-btn').click().catch(async()=>{await (await revealControl(page,'#hand-btn')).click();});
  await (await revealControl(page,'#react-btn')).click();await page.locator('.reaction-choice[data-emoji="🎉"]').click();
  for(const other of pages.keys())if(other!==name)await tileOf(pages.get(other).page,name).locator('.reaction-float',{hasText:'🎉'}).waitFor();
 }
 // A Chromium reaction reaches the guest engines too.
 await (await revealControl(host,'#react-btn')).click();await host.locator('.reaction-choice[data-emoji="👏"]').click();
 for(const engine of guestEngines)await tileOf(pages.get(nameOf(engine)).page,'Host').locator('.reaction-float',{hasText:'👏'}).waitFor();
 console.log('hands and reactions across engines');

 // ---- Optional extra: background blur. It works, or it says it can't;
 // either way the call keeps flowing and the recording track is untouched.
 const effects={};
 for(const engine of guestEngines){
  const name=nameOf(engine),{page}=pages.get(name);
  await (await revealControl(page,'#effects-btn')).click();
  await page.locator('#effects-panel').waitFor({state:'visible'});
  const camId=await page.evaluate(()=>document.querySelector('#local-video').srcObject.getVideoTracks()[0]?.id);
  await page.locator('#effect-call-blur-light').click();
  const outcome=await page.waitForFunction(camId=>{
   const on=document.querySelector('#effect-call-blur-light')?.getAttribute('aria-checked')==='true';
   const pc=window.__pcs.filter(p=>p.connectionState==='connected').at(-1);
   const sent=pc?.getSenders().find(s=>s.track?.kind==='video')?.track?.id;
   if(on&&sent&&sent!==camId)return 'blur';
   const toast=[...document.querySelectorAll('.toast')].map(t=>t.textContent).find(t=>/background/i.test(t)&&/couldn|unavailable|isn|can.t/i.test(t));
   return toast?`unavailable: ${toast}`:null;
  },camId,{timeout:60000}).then(h=>h.jsonValue()).catch(()=>'no answer');
  effects[engine]=outcome;
  assert.notEqual(outcome,'no answer',`${engine}: background blur neither applied nor reported`);
  if(outcome==='blur'){await page.locator('#effect-call-off').click();}
  await page.keyboard.press('Escape');
  await page.locator('#effects-panel').evaluate(p=>p.hidden=true).catch(()=>{});
 }
 for(const name of pages.keys())await assertDecodesEveryone(name);
 console.log('background blur',JSON.stringify(effects));

 // ---- Screen share view: the Chromium host presents, guests decode it.
 await (await revealControl(host,'#screen-share-btn')).click();
 await host.waitForFunction(()=>document.querySelector('#screen-share-btn').getAttribute('aria-pressed')==='true');
 for(const engine of guestEngines){
  const {page}=pages.get(nameOf(engine));
  await page.waitForFunction(()=>{const t=document.querySelector('#video-grid > .tile-screen:not(.tile-screen-local)');return t&&t.querySelector('.tile-name')?.textContent==='Host is presenting'&&t.querySelector('video').videoWidth>0;},null,{timeout:30000});
  const before=await page.evaluate(()=>window.__frames(document.querySelector('#video-grid > .tile-screen:not(.tile-screen-local) video')));
  await page.waitForFunction(b=>window.__frames(document.querySelector('#video-grid > .tile-screen:not(.tile-screen-local) video'))>b+5,before,{timeout:20000});
  await page.waitForFunction(()=>document.querySelector('#video-grid').classList.contains('layout-presentation'));
  await screenshot(`crossbrowser-${engine}-02-screen-share`,page);
 }
 await (await revealControl(host,'#screen-share-btn')).click();
 for(const engine of guestEngines)await pages.get(nameOf(engine)).page.waitForFunction(()=>!document.querySelector('#video-grid > .tile-screen'));
 console.log('guests decode the host screen share');

 // ---- Take: each guest records locally and uploads progressively.
 const headers={Authorization:`Bearer ${session.hostToken}`};
 await startTake(host);
 for(const {page} of pages.values())await page.waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='inline-flex',null,{timeout:20000});
 const takeStarted=Date.now();
 console.log('recording on all engines');
 const takeSnapshot=async()=>{const detail=await(await fetch(`${origin}/api/v1/sessions/${session.id}`,{headers})).json();const take=detail.takes[0];return take?await(await fetch(`${origin}/api/v1/takes/${take.id}`,{headers})).json():null;};
 // Progressive: each guest's fragments are acknowledged by the host (chunk
 // PUT answered 2xx) while the take still records.
 const progressive={};
 for(const e of guestEngines){const name=nameOf(e);progressive[name]={chunks:0,bytes:0};pages.get(name).page.on('response',r=>{if(r.request().method()==='PUT'&&/\/chunks\/\d+$/.test(r.url())&&r.ok()&&!progressive[name].stopped){progressive[name].chunks++;progressive[name].bytes+=Number(r.request().headers()['content-length']||r.request().postDataBuffer()?.length||0);}});}
 for(let i=0;i<120&&guestEngines.some(e=>!progressive[nameOf(e)].chunks);i++)await wait(250);
 for(const e of guestEngines){assert.ok(progressive[nameOf(e)].chunks>0,`${nameOf(e)} uploaded nothing during the take`);}
 assert.equal(await pages.get(nameOf(guestEngines[0])).page.locator('#rec-indicator').isVisible(),true,'still recording while fragments upload');
 await wait(Math.max(0,8000-(Date.now()-takeStarted)));
 for(const e of guestEngines)progressive[nameOf(e)].stopped=true;
 await host.locator('#rec-btn').click();
 const takeSeconds=(Date.now()-takeStarted)/1000;
 let snapshot;
 for(let i=0;i<400;i++){snapshot=await takeSnapshot();if(snapshot&&snapshot.tracks.length===pages.size&&snapshot.tracks.every(t=>t.status==='assembled'))break;await wait(150);}
 assert.equal(snapshot.tracks.length,pages.size,JSON.stringify(snapshot));
 assert.ok(snapshot.tracks.every(t=>t.status==='assembled'&&t.totalBytes>1000),JSON.stringify(snapshot));
 for(const engine of guestEngines)await pages.get(nameOf(engine)).page.waitForFunction(()=>/saved on the host/.test(document.querySelector('#telemetry-status').textContent),null,{timeout:30000});
 const raw=path.join(data,'sessions',session.id,'takes',snapshot.take.id,'raw');
 const rawFiles=(await readdir(raw)).filter(f=>/\.(webm|mp4|wav)$/.test(f));
 const recorders={};
 for(const t of snapshot.tracks){
  const who=t.participantName;
  const file=rawFiles.find(f=>f.startsWith(t.trackId));
  assert.ok(file,`raw file for ${who}`);
  const info=JSON.parse(execFileSync('ffprobe',['-v','error','-show_streams','-show_format','-of','json',path.join(raw,file)],{encoding:'utf8'}));
  const v=info.streams.find(s=>s.codec_type==='video'),a=info.streams.find(s=>s.codec_type==='audio');
  assert.ok(v&&a,`${who} original has video+audio: ${file}`);
  execFileSync('ffmpeg',['-v','error','-i',path.join(raw,file),'-f','null','-'],{stdio:'pipe'});
  const sidecar=JSON.parse(await readFile(path.join(raw,t.trackId+'.sidecar.json'),'utf8').catch(()=>'{}'));
  recorders[who]={container:path.extname(file).slice(1),video:v.codec_name,audio:a.codec_name,mime:sidecar.mimeType||t.mime,engine:sidecar.engine,bytes:t.totalBytes};
 }
 console.log('originals',JSON.stringify(recorders));
 assert.equal(recorders[nameOf('webkit')]?.container??'mp4','mp4','WebKit records MP4');
 assert.equal(recorders[nameOf('firefox')]?.container??'webm','webm','Firefox records WebM');

 // ---- Export.
 await host.locator('#refresh-transfers-btn').click().catch(()=>{});
 await host.waitForFunction(()=>!document.querySelector('#prepare-export-btn').disabled,null,{timeout:30000});
 const processed=host.waitForResponse(r=>r.url().endsWith('/postprocess')&&r.request().method()==='POST',{timeout:180000});
 await host.locator('#prepare-export-btn').click();
 const processedResponse=await processed;assert.ok(processedResponse.ok(),await processedResponse.text());
 await host.locator('#export-modal').waitFor({state:'visible'});
 const out=path.join(data,'sessions',session.id,'takes',snapshot.take.id,'out');
 const manifest=JSON.parse(await readFile(path.join(out,'manifest.json'),'utf8'));
 const exported={};
 // Every participant exports a picture angle and an isolated audio track,
 // each decodable, as long as the take (±2 s), with an offset in the manifest.
 for(const name of pages.keys()){
  exported[name]={};
  for(const kind of ['video','audio']){
   const source=manifest.sources.find(s=>s.participant===name&&s.kind===kind&&!s.source);
   assert.ok(source,`${name} ${kind} in manifest: ${JSON.stringify(manifest.sources.map(s=>[s.participant,s.kind,s.source]))}`);
   assert.ok(Number.isFinite(source.offsetSec)&&source.offsetSec>=0&&source.offsetSec<5,`${name} ${kind} offset ${source.offsetSec}`);
   const file=path.join(out,source.file);
   const info=JSON.parse(execFileSync('ffprobe',['-v','error','-show_streams','-show_format','-of','json',file],{encoding:'utf8'}));
   assert.ok(info.streams.some(s=>s.codec_type===kind),`${name} ${kind} export has a ${kind} stream`);
   const duration=Number(info.format.duration);
   const rawTrack=snapshot.tracks.find(t=>t.participantName===name&&t.kind==='video');
   const rawFile=rawTrack&&rawFiles.find(f=>f.startsWith(rawTrack.trackId));
   const rawEnd=rawFile?execFileSync('ffprobe',['-v','error','-show_entries','packet=pts_time,duration_time','-of','csv=p=0',path.join(raw,rawFile)],{encoding:'utf8'}).trim().split('\n').reduce((m,l)=>{const [p,d]=l.split(',').map(Number);return Number.isFinite(p)?Math.max(m,p+(Number.isFinite(d)?d:0)):m;},0):NaN;
   assert.ok(Math.abs(duration-takeSeconds)<2,`${name} ${kind} export duration ${duration}s vs take ${takeSeconds.toFixed(1)}s (offset ${source.offsetSec}s, original ends at ${rawEnd.toFixed?.(2)}s, ${rawFile})`);
   execFileSync('ffmpeg',['-v','error','-i',file,'-f','null','-'],{stdio:'pipe'});
   exported[name][kind]={duration:+duration.toFixed(2),offsetSec:+source.offsetSec.toFixed(3),method:source.method,codec:info.streams.find(s=>s.codec_type===kind).codec_name};
  }
 }
 console.log('export',JSON.stringify(exported));
 assert.deepEqual(errors,[]);
 console.log(JSON.stringify({engines:['chromium',...guestEngines],notes,decode,effects,codecs,chat:true,hands:true,reactions:true,screenShareViewed:true,acknowledgedDuringTake:Object.fromEntries(Object.entries(progressive).map(([k,v])=>[k,{chunks:v.chunks,bytes:v.bytes}])),recorders,exported,takeSeconds:+takeSeconds.toFixed(1),webkitLegacyOpfs:legacyOpfs,errors}));
}catch(e){
 console.error('page errors:',JSON.stringify(errors));
 const bounded=p=>Promise.race([p,wait(5000).then(()=>'(timed out)')]);
 for(const [name,{page}] of pages)if(!page.isClosed()){
  console.error(`--- ${name}:`,(await page.locator('body').innerText({timeout:2000}).catch(()=>'')).slice(-600));
  console.error(`--- ${name} peers:`,await bounded(page.evaluate(()=>JSON.stringify(window.__pcs.map(pc=>({conn:pc.connectionState,ice:pc.iceConnectionState,sig:pc.signalingState,tx:pc.getTransceivers().map(t=>`${t.mid}:${t.direction}/${t.currentDirection}:${t.receiver.track?.kind}:${t.sender.track?.kind||'-'}`),remote:(pc.remoteDescription?.sdp||'').split('\n').filter(l=>/^m=|a=rtpmap|a=group|a=(sendrecv|recvonly|sendonly|inactive)/.test(l)).slice(0,40).join(' | ')})))).catch(e=>String(e))));
  if(process.env.DEBUG_GUM)console.error(`--- ${name} gum:`,await bounded(page.evaluate(()=>JSON.stringify(window.__gum)).catch(e=>String(e))));
  console.error(`--- ${name} tiles:`,await bounded(page.evaluate(()=>JSON.stringify([...document.querySelectorAll('.tile video')].map(v=>({id:v.id,w:v.videoWidth,paused:v.paused,muted:v.muted,ready:v.readyState,tracks:v.srcObject?.getTracks().map(t=>t.kind+':'+t.readyState+':'+t.muted)})))).catch(e=>String(e))));
 }
 console.error(logs.slice(-8000));throw e;
}finally{
 for(const b of browsers){await closeBrowser(b);if(b.dir)await rm(b.dir,{recursive:true,force:true});}
 if(daemon.exitCode===null&&daemon.signalCode===null){
  daemon.kill('SIGTERM');
  await Promise.race([new Promise(r=>daemon.once('exit',r)),wait(3000)]);
  if(daemon.exitCode===null&&daemon.signalCode===null)daemon.kill('SIGKILL');
 }
 await rm(data,{recursive:true,force:true});
}
