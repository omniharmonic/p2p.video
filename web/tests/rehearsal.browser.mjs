// Full-episode rehearsal [SL-QA-1]: every podcast-day feature exercised
// together, the way docs/PODCAST_DAY.md runs a real episode, against the real
// daemon with Chromium fake devices (one browser per person so each has its
// own synthesized voice as the fake microphone).
//
//  1. Host signs in (launch URL), creates room "Rehearsal", copies the guest
//     and producer links from the home.
//  2. Guest A (headphones → raw mic) and Guest B (no headphones → clean mic)
//     open the room link before the host starts, both with the uncompressed
//     microphone; a producer opens the producer link. All wait for the host.
//  3. Host starts the room; host admits the producer, the producer admits both
//     guests. Everyone decodes everyone; signal bars; pre-flight.
//  4. Captions on (fake provider), chat both ways, hand up/down, a reaction.
//  5. Producer starts the take. During it: A screen-shares ~10 s; the host
//     livestreams ~15 s to a local ffmpeg RTMP sink; B's network is impaired
//     (CDP) then restored; A's PeerConnection is forced to fail and recovers;
//     B's WebSocket drops once; A adds a phone camera mid-take.
//  6. Take runs REHEARSAL_MINUTES (default 3). Producer stops; all uploads finish.
//  7. Host downloads originals while the export runs; editor ZIP downloaded and
//     verified with ffprobe (angles, mics, offsets + durations, micProcessing,
//     FCP7 XML references, no producer).
//  8. Transcribe with the default Built-in engine (fake provider speaking the
//     Mac app's protocol; Minutes stand-in CLI also detected) and Send to
//     Parachute (local fake vault); outputs checked.
//  9. Host ends the meeting; caption transcript; end screen everywhere; no
//     unexpected page/console errors; no secrets or caption text in the log.
// 10. Timings and daemon resources (RSS, goroutines) → JSON summary in
//     REHEARSAL_OUT (default: <tmp>/studiolink-rehearsal).
//
//   make rehearsal            (or: cd web && STUDIO_BINARY=$PWD/../bin/p2pvideod node tests/rehearsal.browser.mjs)
// Env: REHEARSAL_MINUTES, REHEARSAL_OUT, STUDIO_TEST_PORT, STUDIO_RTMP_PORT,
//      STUDIO_FFMPEG_DIR (RTMP-capable ffmpeg for the daemon + sink; default
//      ../build/media-tools/bin when present), REHEARSAL_KEEP=1 keeps the data dir.
import assert from 'node:assert/strict';
import {spawn,execFileSync} from 'node:child_process';
import {createWriteStream,existsSync} from 'node:fs';
import {mkdtemp,rm,readFile,writeFile,mkdir,readdir,stat} from 'node:fs/promises';
import http from 'node:http';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
import {freePort,isolatedNetworkArgs,startTake,launchURL,revealControl,readyToJoin,openRecordingOptions} from './lib/harness.mjs';
import {connectFakeProvider} from './lib/fake-caption-provider.mjs';
import {connectFakeTranscriber} from './lib/fake-transcription-provider.mjs';
const {chromium,devices}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');

const MINUTES=Number(process.env.REHEARSAL_MINUTES||3);
const repo=fileURLToPath(new URL('../../',import.meta.url));
const bundledFF=path.join(repo,'build','media-tools','bin');
const ffDir=process.env.STUDIO_FFMPEG_DIR||(existsSync(path.join(bundledFF,'ffmpeg'))?bundledFF:'');
const ffmpeg=ffDir?path.join(ffDir,'ffmpeg'):'ffmpeg';
const ffprobe=ffDir?path.join(ffDir,'ffprobe'):'ffprobe';
const env={...process.env,PATH:ffDir?`${ffDir}:${process.env.PATH}`:process.env.PATH};
const outRoot=process.env.REHEARSAL_OUT||path.join(tmpdir(),'studiolink-rehearsal');
const runId=new Date().toISOString().replace(/[:.]/g,'-');
const runDir=path.join(outRoot,`run-${runId}`);
await mkdir(runDir,{recursive:true});
const data=await mkdtemp(path.join(tmpdir(),'studio-rehearsal-'));
const port=Number(process.env.STUDIO_TEST_PORT||await freePort('tcp')),origin=`http://127.0.0.1:${port}`;
const rtmpPort=Number(process.env.STUDIO_RTMP_PORT||await freePort('tcp'));
const streamKey='rehearsal-stream-key-91c2e7';
const wait=ms=>new Promise(r=>setTimeout(r,ms));
const t0=Date.now();
const ts=()=>((Date.now()-t0)/1000).toFixed(1);
const log=(...a)=>console.log(`[${ts()}s]`,...a);

// ---- Phase timings and resource samples.
const phases=[];let phase=null;
const begin=name=>{if(phase)phase.sec=+((Date.now()-phase.start)/1000).toFixed(1);phase={name,start:Date.now()};phases.push(phase);log('──',name);};
const resources=[];

// ---- Distinct synthesized voices (fake microphones), 48 kHz mono WAV.
const voices={Host:['Samantha','Hello and welcome to the rehearsal episode. I am your host and this is a test of the whole show.'],
 'Guest A':['Daniel','Thanks for having me. I am wearing headphones today, so my microphone is recorded raw.'],
 'Guest B':['Karen','Hi everyone. I am on laptop speakers, so p2p.video records a clean microphone for me.'],
 Pat:['Fred','Producer here. I will admit the guests and run the take. Rolling in three, two, one.']};
const wavs={};
for(const [who,[voice,text]] of Object.entries(voices)){
 const wav=path.join(data,`${who.replace(/\s/g,'-')}.wav`),aiff=wav.replace('.wav','.aiff');
 try{execFileSync('say',['-v',voice,'-o',aiff,text]);execFileSync(ffmpeg,['-loglevel','error','-y','-i',aiff,'-af','apad=pad_dur=1','-ar','48000','-ac','1','-c:a','pcm_s16le',wav],{env});}
 catch{execFileSync(ffmpeg,['-loglevel','error','-y','-f','lavfi','-i',`anoisesrc=color=pink:sample_rate=48000:duration=12:seed=${Object.keys(wavs).length+1},volume=0.4`,'-af','tremolo=f=4:d=0.9','-ac','1','-c:a','pcm_s16le',wav],{env});}
 wavs[who]=wav;
}

// ---- Stand-in minutes-cli (docs/integrations/MINUTES.md contract), as in studio.browser.mjs.
const minutesFixture=path.join(repo,'testdata','integrations','minutes.file-transcript.v1.json');
const minutesCli=path.join(data,'stand-in-minutes-cli');
await writeFile(minutesCli,`#!/bin/sh\nif [ "$1" = help ]; then echo "minutes-cli transcribe-file <audio-path>"; exit 0; fi\nwhile [ $# -gt 0 ]; do if [ "$1" = --output ]; then cp '${minutesFixture}' "$2"; fi; shift; done\n`,{mode:0o755});

// ---- Local fake Parachute vault (docs/integrations/PARACHUTE.md request shape).
const vaultToken='rehearsal-vault-jwt-5d1f0a';
const tokenFile=path.join(data,'parachute-token');
await writeFile(tokenFile,vaultToken+'\n',{mode:0o600});
const vaultRequests=[];
const vault=http.createServer((req,res)=>{let body='';req.on('data',c=>body+=c);req.on('end',()=>{
 vaultRequests.push({method:req.method,url:req.url,auth:req.headers.authorization,type:req.headers['content-type'],body:body?JSON.parse(body):null});
 if(req.method==='POST'&&req.url==='/vault/default/api/notes'){const b=JSON.parse(body);res.writeHead(201,{'Content-Type':'application/json'});res.end(JSON.stringify({note:{id:'note-rehearsal-1',path:b.path,updatedAt:new Date().toISOString()}}));return;}
 res.writeHead(404);res.end('not found');
});});
const vaultPort=await freePort('tcp');
await new Promise(r=>vault.listen(vaultPort,'127.0.0.1',r));

// ---- Local RTMP sink.
const flv=path.join(data,'livestream.flv');
const sink=spawn(ffmpeg,['-hide_banner','-v','error','-listen','1','-i',`rtmp://127.0.0.1:${rtmpPort}/live/${streamKey}`,'-c','copy','-y',flv],{stdio:['ignore','pipe','pipe'],env});
let sinkLogs='';sink.stderr.on('data',b=>sinkLogs+=b);
const sinkExit=new Promise(r=>sink.once('exit',c=>r(c)));

// ---- Daemon.
const daemonLogPath=path.join(runDir,'daemon.log');
const daemonLogFile=createWriteStream(daemonLogPath);
const daemon=spawn(process.env.STUDIO_BINARY||'../bin/p2pvideod',['--data-dir',data,'--port',String(port),'--no-funnel','--no-public-stun',
 '--minutes-cli',minutesCli,'--parachute-url',`http://127.0.0.1:${vaultPort}`,'--parachute-vault','default','--parachute-token-file',tokenFile,
 ...await isolatedNetworkArgs()],{stdio:['ignore','pipe','pipe'],env});
let logs='';const onLog=b=>{logs+=b;daemonLogFile.write(b);};daemon.stderr.on('data',onLog);daemon.stdout.on('data',onLog);

// Loopback-only diagnostics: plain http.get (no Origin header, as the endpoint requires).
const runtimeStats=()=>new Promise(resolve=>{http.get(`${origin}/api/v1/debug/runtime`,res=>{let b='';res.on('data',c=>b+=c);res.on('end',()=>{try{resolve(JSON.parse(b));}catch{resolve(null);}});}).on('error',()=>resolve(null));});
const rssKB=()=>{try{return Number(execFileSync('ps',['-o','rss=','-p',String(daemon.pid)],{encoding:'utf8'}).trim());}catch{return null;}};
async function sample(label){const r=await runtimeStats();const s={at:+ts(),label,rssMB:+((rssKB()||0)/1024).toFixed(1),goroutines:r?.goroutines??null,heapMB:r?+(r.heapAllocBytes/1048576).toFixed(1):null,openFDs:r?.openFDs??null};resources.push(s);return s;}

// ---- Console/page errors. Each allowed message is listed with its reason.
const errors=[];const allowedSeen={};
// Empty on purpose: the rehearsal produced no console errors in its green
// runs. Add an entry only with the reason it is benign.
const ALLOWED=[];
const browsers=[];const pages=new Map();
const launchFor=who=>chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream',`--use-file-for-fake-audio-capture=${wavs[who]}`,'--autoplay-policy=no-user-gesture-required','--mute-audio']}).then(b=>{browsers.push(b);return b;});
const instrument=()=>{
 const O=window.RTCPeerConnection;window.__pcs=[];window.RTCPeerConnection=class extends O{constructor(...a){super(...a);window.__pcs.push(this);}};
 const W=window.WebSocket;window.__sockets=[];window.WebSocket=class extends W{constructor(...a){super(...a);window.__sockets.push(this);}};
};
async function open(browser,name,options={}){
 const context=await browser.newContext({permissions:['camera','microphone','clipboard-read','clipboard-write'],viewport:{width:1280,height:800},acceptDownloads:true,...options});
 await context.addInitScript(instrument);
 const page=await context.newPage();page.setDefaultTimeout(45000);
 page.on('console',m=>{if(m.type()!=='error')return;const text=`${m.text()} @${m.location().url.replace(/token=[^&\s]+/g,'token=…')}`;const a=ALLOWED.find(x=>x.re.test(text));if(a){allowedSeen[a.why]=(allowedSeen[a.why]||0)+1;return;}errors.push(`${name} console: ${text}`);});
 page.on('pageerror',e=>errors.push(`${name} pageerror: ${e.message}`));
 pages.set(name,page);return page;
}
const tileOf=(page,name)=>page.locator('#video-grid .tile[data-participant-id]').filter({has:page.locator('.tile-name',{hasText:new RegExp(`^${name}$`)})});
// Remote tiles with cameras decode and keep advancing; `expected` are the names on that page.
async function decodesEveryone(page,expected,{camOff=[]}={}){
 await page.waitForFunction(({expected,camOff})=>{
  const tiles=[...document.querySelectorAll('#video-grid .tile[data-participant-id]:not(.tile-screen)')];
  const byName=Object.fromEntries(tiles.map(t=>[t.querySelector('.tile-name')?.textContent,t]));
  return expected.every(n=>byName[n]&&(camOff.includes(n)||byName[n].querySelector('video')?.videoWidth>0));
 },{expected,camOff},{timeout:45000});
 const live=expected.filter(n=>!camOff.includes(n));
 const frames=()=>page.evaluate(live=>live.map(n=>{const t=[...document.querySelectorAll('#video-grid .tile[data-participant-id]')].find(t=>t.querySelector('.tile-name')?.textContent===n);return t?.querySelector('video')?.getVideoPlaybackQuality().totalVideoFrames??0;}),live);
 const before=await frames();
 await page.waitForFunction(({live,before})=>live.every((n,i)=>{const t=[...document.querySelectorAll('#video-grid .tile[data-participant-id]')].find(t=>t.querySelector('.tile-name')?.textContent===n);return (t?.querySelector('video')?.getVideoPlaybackQuality().totalVideoFrames??0)>before[i]+5;}),{live,before},{timeout:20000});
}
// Mono 8 kHz float samples of [start, start+dur) of a file's audio.
const decodeWindow=(file,start,dur)=>new Promise((resolve,reject)=>{
 const p=spawn(ffmpeg,['-v','error','-nostdin','-ss',String(Math.max(0,start)),'-i',file,'-t',String(dur),'-vn','-ac','1','-ar','8000','-f','f32le','pipe:1'],{env});
 const parts=[];p.stdout.on('data',b=>parts.push(b));p.on('error',reject);
 p.on('close',code=>{if(code)return reject(new Error('decode '+file));const b=Buffer.concat(parts);resolve(new Float32Array(b.buffer,b.byteOffset,Math.floor(b.length/4)));});
});
// The same window of a camera file's audio, cut from one decode of the whole
// file on its container timeline (sample 0 = pts 0, timestamp gaps kept as
// silence, as the export's WAV conversion does) [SL-QA-2]. ffmpeg's input
// seek into Chromium's MP4/Opus landed 31 ms off for targets just after the
// short packet MediaRecorder writes every ~5 s (decoding from the start put
// the same window at 0 ms), failing single windows at random.
const timelines=new Map();
const timelineWindow=async(file,start,dur)=>{
 if(!timelines.has(file))timelines.set(file,new Promise((resolve,reject)=>{
  const p=spawn(ffmpeg,['-v','error','-nostdin','-i',file,'-vn','-af','aresample=async=1:min_hard_comp=0.015:first_pts=0','-ac','1','-ar','8000','-f','f32le','pipe:1'],{env});
  const parts=[];p.stdout.on('data',b=>parts.push(b));p.on('error',reject);
  p.on('close',code=>{if(code)return reject(new Error('decode '+file));const b=Buffer.concat(parts);resolve(new Float32Array(b.buffer,b.byteOffset,Math.floor(b.length/4)));});
 }));
 const all=await timelines.get(file),from=Math.max(0,Math.round(start*8000));
 return all.subarray(from,from+Math.round(dur*8000));
};
// k maximizing the normalized correlation of x against y[k:k+len(x)].
function bestLag(x,y){
 let xx=0;for(const v of x)xx+=v*v;let best=0,bestC=-2;
 for(let k=0;k+x.length<=y.length;k++){let d=0,yy=0;for(let i=0;i<x.length;i+=2){d+=x[i]*y[k+i];yy+=y[k+i]*y[k+i];}const c=d/Math.sqrt((xx/2)*yy+1e-12);if(c>bestC){bestC=c;best=k;}}
 return {k:best,c:bestC};
}
const recIndicator=page=>page.waitForFunction(()=>document.querySelector('#rec-indicator')?.style.display==='inline-flex',null,{timeout:30000});

let summary={ok:false,minutes:MINUTES,runDir};
let session,hostHeaders;
try{
 let ready=false;for(let i=0;i<100;i++){try{ready=(await fetch(origin+'/health')).ok;if(ready)break;}catch{}await wait(100);}assert.ok(ready,'daemon did not start: '+logs);
 await sample('daemon ready');
 const provider=await connectFakeProvider({origin,dataDir:data});

 // ================= 1. Host home: room + links.
 begin('1 host creates room, copies links');
 const [hostBrowser,aBrowser,bBrowser,pBrowser]=await Promise.all(['Host','Guest A','Guest B','Pat'].map(launchFor));
 const host=await open(hostBrowser,'Host');
 await host.goto(await launchURL(data,origin));
 await host.locator('#home-view').waitFor();
 await host.locator('#rail-rooms').click();
 await host.locator('#room-name-input').fill('Rehearsal');
 await host.locator('#room-create-btn').click();
 const roomRow=host.locator('.room-row',{has:host.locator('.room-title strong',{hasText:/^Rehearsal$/})});
 await roomRow.waitFor();
 const clipboard=()=>host.evaluate(()=>navigator.clipboard.readText());
 await roomRow.locator('[data-act="copy"]').click();
 await host.locator('.toast',{hasText:'Room link for “Rehearsal” copied'}).waitFor();
 const guestLink=await clipboard();
 assert.match(guestLink,/\/join\?room=rehearsal&token=/,'guest link');
 await roomRow.locator('.room-menu summary').click();
 await roomRow.locator('[data-act="producer"]').click();
 await host.locator('.toast',{hasText:'Producer link for “Rehearsal” copied'}).waitFor();
 const producerLink=await clipboard();
 assert.match(producerLink,/\/join\?room=rehearsal&token=/,'producer link');
 assert.notEqual(producerLink,guestLink);
 const toLocal=u=>origin+new URL(u).pathname+new URL(u).search;
 log('room created; guest + producer links copied from the home');

 // ================= 2. Early arrivals wait for the host.
 begin('2 guests + producer wait for host');
 const a=await open(aBrowser,'Guest A'),b=await open(bBrowser,'Guest B'),pat=await open(pBrowser,'Pat');
 const bCdp=await b.context().newCDPSession(b);await bCdp.send('Network.enable');
 // Pre-arm CDP throttling so it applies to B's WebRTC sockets (see netimpair.browser.mjs).
 const PREARM={offline:false,latency:0,downloadThroughput:-1,uploadThroughput:100_000_000,packetLoss:0,packetQueueLength:0,packetReordering:false};
 const IMPAIRED={offline:false,latency:150,downloadThroughput:-1,uploadThroughput:125_000,packetLoss:3,packetQueueLength:0,packetReordering:false};
 await bCdp.send('Network.emulateNetworkConditions',PREARM);
 for(const [page,name,headphones] of [[a,'Guest A',true],[b,'Guest B',false]]){
  await page.goto(toLocal(guestLink));
  await page.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
  await page.locator('#green-room h1',{hasText:'Join Rehearsal'}).waitFor();
  await page.locator('#headphones-toggle').waitFor();
  if(headphones)await page.locator('#headphones-toggle').check();
  await openRecordingOptions(page);await page.locator('#pcm-audio-toggle').check();
  await page.locator('#name-input').fill(name);
  await page.waitForFunction(()=>!document.querySelector('#join-btn').disabled);
  await page.locator('#join-btn').click();
  await page.locator('#waiting-room.is-waiting-host').waitFor();
 }
 await pat.goto(toLocal(producerLink));
 await pat.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
 await pat.locator('#producer-note').waitFor({state:'visible'});
 await pat.locator('#name-input').fill('Pat');
 await pat.waitForFunction(()=>!document.querySelector('#join-btn').disabled);
 await pat.locator('#join-btn').click();
 await pat.locator('#waiting-room.is-waiting-host').waitFor();
 log('Guest A (headphones), Guest B (speakers) and producer Pat wait for the host');

 // ================= 3. Host starts; host admits Pat; Pat admits the guests.
 begin('3 host starts, admissions, everyone decodes');
 await roomRow.locator('[data-act="start"]').click();
 await host.locator('#name-input').fill('Host');await host.locator('#join-btn').click();
 await host.locator('#studio-view').waitFor({state:'visible'});
 const sessionId=new URL(host.url()).searchParams.get('session');
 const hostToken=await host.evaluate(()=>new URL(location.href).searchParams.get('token'));
 assert.ok(sessionId&&hostToken,'host URL carries the session');
 hostHeaders={Authorization:`Bearer ${hostToken}`};
 session={id:sessionId,hostToken};
 await host.waitForFunction(()=>document.querySelector('#host-admission-banner').style.display==='flex'&&/\(producer\)/.test(document.querySelector('#admission-guest-name').textContent),null,{timeout:30000});
 await host.locator('#admit-btn').click();
 await pat.locator('#studio-view').waitFor({state:'visible'});
 for(const name of ['Guest A','Guest B']){
  await pat.waitForFunction(()=>document.querySelector('#host-admission-banner').style.display==='flex',null,{timeout:30000});
  const who=await pat.locator('#admission-guest-name').innerText();
  assert.ok(/^Guest [AB]$/.test(who),`producer banner: ${who}`);
  await pat.locator('#admit-btn').click();
  await pat.waitForFunction(who=>document.querySelector('#admission-guest-name').textContent!==who||document.querySelector('#host-admission-banner').style.display!=='flex',who);
 }
 await a.locator('#studio-view').waitFor({state:'visible'});await b.locator('#studio-view').waitFor({state:'visible'});
 const everyone=['Host','Guest A','Guest B','Pat'];
 for(const [name,page] of [['Host',host],['Guest A',a],['Guest B',b],['Pat',pat]])await decodesEveryone(page,everyone.filter(n=>n!==name),{camOff:['Pat']});
 // Signal bars on every remote tile (host view).
 await host.waitForFunction(()=>[...document.querySelectorAll('#video-grid .tile[data-participant-id]:not(.tile-screen)')].every(t=>['good','fair','poor'].includes(t.querySelector('.tile-net')?.dataset.quality)),null,{timeout:30000});
 const bars=await host.evaluate(()=>Object.fromEntries([...document.querySelectorAll('#video-grid .tile[data-participant-id]:not(.tile-screen)')].map(t=>[t.querySelector('.tile-name').textContent,t.querySelector('.tile-net').dataset.quality])));
 // Pre-flight: the host chip lists both guests with their recording mic modes.
 await (await revealControl(host,'#preflight-chip')).click();
 await host.waitForFunction(()=>{const s=[...document.querySelectorAll('#preflight-popover .preflight-participant')];const m=Object.fromEntries(s.map(x=>[x.querySelector('strong')?.textContent,x.querySelector('.preflight-recmic')?.dataset.recmic]));return m['Guest A']==='raw'&&m['Guest B']==='clean';},null,{timeout:20000});
 const preflightNames=await host.locator('#preflight-popover .preflight-participant strong').allInnerTexts();
 const patPreflight=await host.locator('#preflight-popover .preflight-participant',{has:host.locator('strong',{hasText:/^Pat$/})}).locator('.preflight-recmic').innerText();
 assert.equal(patPreflight,'Producer · not recorded');
 await (await revealControl(host,'#preflight-chip')).click();
 log('everyone decodes everyone; bars',JSON.stringify(bars),'pre-flight',JSON.stringify(preflightNames));
 await sample('call up (4 people)');

 // ================= 4. Captions, chat, hand, reaction.
 begin('4 captions, chat, hand, reaction');
 await host.locator('body').click({position:{x:5,y:5}});await host.keyboard.press('KeyC');
 for(const page of [host,a,b,pat])await page.locator('#captions-indicator',{hasText:'Captions on'}).waitFor();
 const captionLines=page=>page.$$eval('#captions-overlay .caption-line',rows=>{let who='';return rows.map(r=>{const s=r.querySelector('.caption-speaker');if(s)who=s.textContent;return `${who}: ${r.querySelector('.caption-text').textContent}`;});});
 const seenCaptions=new Set();
 const deadline=Date.now()+40000;
 while(Date.now()<deadline&&!(seenCaptions.has('Guest A')&&seenCaptions.has('Guest B')&&seenCaptions.has('You'))){
  for(const l of await captionLines(host))seenCaptions.add(l.split(':')[0]);
  await wait(300);
 }
 assert.ok(seenCaptions.has('Guest A')&&seenCaptions.has('Guest B')&&seenCaptions.has('You'),'host captions per speaker: '+[...seenCaptions]);
 assert.ok(!seenCaptions.has('Pat'),'the producer is never captioned');
 await host.locator('#chat-btn').click();
 await host.locator('#chat-input').fill('Welcome to the rehearsal! We roll in a minute.');await host.keyboard.press('Enter');
 for(const page of [a,b,pat])await page.locator('#chat-btn .collab-badge').waitFor();
 await b.locator('#chat-btn').click();await b.locator('.chat-log .chat-text',{hasText:'Welcome to the rehearsal'}).waitFor();
 await b.locator('#chat-input').fill('Ready when you are.');await b.locator('.chat-send').click();
 await host.locator('.chat-log .chat-text',{hasText:'Ready when you are.'}).waitFor();
 await host.keyboard.press('Escape');await b.keyboard.press('Escape');
 await a.locator('#hand-btn').click();
 await tileOf(host,'Guest A').locator('.tile-hand').waitFor();
 await a.locator('#hand-btn').click();
 await tileOf(host,'Guest A').locator('.tile-hand').waitFor({state:'hidden'});
 await b.locator('#react-btn').click();await b.locator('.reaction-choice[data-emoji="🎉"]').click();
 await tileOf(host,'Guest B').locator('.reaction-float',{hasText:'🎉'}).waitFor();
 log('captions per speaker',JSON.stringify([...seenCaptions]),'; chat, hand, reaction ok');
 // Branding (SL-P5-2): show name + Guest A's lower-third title; the livestream below carries the look.
 const aId=(await(await fetch(`${origin}/api/v1/sessions/${sessionId}`,{headers:hostHeaders})).json()).participants.find(p=>p.name==='Guest A').id;
 await (await revealControl(host,'#branding-btn')).click();
 await host.locator('#branding-panel.open').waitFor({state:'visible'});
 await host.locator('#brand-show-name').fill('Rehearsal Show');
 await host.locator('#brand-tile-labels').click();
 await host.locator('#brand-save').click();
 await host.waitForFunction(()=>/^Saved/.test(document.querySelector('#brand-status').textContent));
 const ltRow=host.locator(`[data-lt-row="${aId}"]`);
 await ltRow.locator('[data-field=title]').fill('Founder, Example Co');
 await ltRow.locator('[data-save]').click();
 await host.waitForFunction(id=>document.querySelector(`#video-grid .tile[data-participant-id="${id}"] .tile-lower-third .lt-title`)?.textContent==='Founder, Example Co',aId);
 await host.locator('#branding-panel [data-close]').click();
 log('branding: show name + Guest A lower-third title');

 // ================= 5. The take.
 begin('5 take (producer starts)');
 const problems=await startTake(pat);
 if(problems)log('pre-flight confirmed:',problems.replace(/\s+/g,' ').slice(0,240));
 for(const page of [host,a,b])await recIndicator(page);
 const takeStart=Date.now();
 const events=[];const ev=(name,extra={})=>{events.push({atSec:+((Date.now()-takeStart)/1000).toFixed(1),name,...extra});log('take +'+((Date.now()-takeStart)/1000).toFixed(0)+'s',name,Object.keys(extra).length?JSON.stringify(extra):'');};
 ev('recording');
 await wait(5000);await sample('take +5s');

 // 5a. Guest A screen-shares ~10 s.
 await a.locator('#screen-share-btn').click();
 await a.waitForFunction(()=>document.querySelector('#screen-share-btn').getAttribute('aria-pressed')==='true');
 await host.waitForFunction(()=>[...document.querySelectorAll('#video-grid > .tile-screen:not(.tile-screen-local)')].some(t=>t.querySelector('video')?.videoWidth>0),null,{timeout:30000});
 const shareStart=Date.now();ev('screen share on');
 await wait(10000);
 await a.locator('#screen-share-btn').click();
 await a.waitForFunction(()=>document.querySelector('#screen-share-btn').getAttribute('aria-pressed')==='false');
 await host.waitForFunction(()=>!document.querySelector('#video-grid > .tile-screen'),null,{timeout:20000});
 const shareSec=(Date.now()-shareStart)/1000;ev('screen share off',{sec:+shareSec.toFixed(1)});

 // 5b. Livestream ~15 s to the local sink.
 await (await revealControl(host,'#livestream-btn')).click();
 await host.locator('#livestream-panel.open').waitFor({state:'visible'});
 const dest=host.locator('#livestream-dest-list .livestream-dest').first();
 await dest.locator('[data-field=platform]').selectOption('custom');
 await dest.locator('[data-field=url]').fill(`rtmp://127.0.0.1:${rtmpPort}/live`);
 await dest.locator('[data-field=key]').fill(streamKey);
 await host.locator('#livestream-go').click();
 await host.waitForFunction(()=>document.querySelector('#livestream-status').dataset.phase==='live',null,{timeout:30000});
 ev('livestream live');
 await wait(15000);
 const liveStatus=await(await fetch(`${origin}/api/v1/sessions/${sessionId}/livestream`,{headers:hostHeaders})).json();
 assert.equal(liveStatus.state,'live',JSON.stringify(liveStatus));
 assert.ok(!JSON.stringify(liveStatus).includes(streamKey),'livestream status leaked the key');
 assert.ok(liveStatus.bitrateKbps>0,'livestream bitrate still 0: '+JSON.stringify({bitrateKbps:liveStatus.bitrateKbps,ingestKbps:liveStatus.ingestKbps}));
 await host.locator('#livestream-end').click();
 await host.waitForFunction(()=>document.querySelector('#livestream-status').dataset.phase==='stopped',null,{timeout:20000});
 await host.locator('#livestream-close-btn').click();
 const sinkCode=await Promise.race([sinkExit,wait(15000).then(()=>'timeout')]);
 assert.notEqual(sinkCode,'timeout','RTMP sink did not finish: '+sinkLogs);
 const flvInfo=JSON.parse(execFileSync(ffprobe,['-v','error','-show_streams','-show_format','-of','json',flv],{encoding:'utf8',env}));
 assert.ok(flvInfo.streams.some(s=>s.codec_name==='h264')&&flvInfo.streams.some(s=>s.codec_name==='aac'),'livestream codecs');
 const flvSec=Number(flvInfo.format.duration);assert.ok(flvSec>=10,`livestream received ${flvSec}s`);
 ev('livestream ended',{receivedSec:+flvSec.toFixed(1),fps:liveStatus.progress?.fps});
 await sample('after livestream');

 // 5c. Guest B's uplink impaired ~20 s, then restored.
 await bCdp.send('Network.emulateNetworkConditions',IMPAIRED);ev('B impaired');
 const bId=(await(await fetch(`${origin}/api/v1/sessions/${sessionId}`,{headers:hostHeaders})).json()).participants.find(p=>p.name==='Guest B').id;
 const bQuality=()=>host.evaluate(id=>document.querySelector(`#tile-${CSS.escape(id)} .tile-net`)?.dataset.quality,bId);
 let worst='good';const impairEnd=Date.now()+20000;
 while(Date.now()<impairEnd){const q=await bQuality();if(q==='poor'||(q==='fair'&&worst==='good'))worst=q;await wait(1000);}
 await bCdp.send('Network.emulateNetworkConditions',PREARM);ev('B restored',{worstQualitySeenByHost:worst});
 await host.waitForFunction(id=>document.querySelector(`#tile-${CSS.escape(id)} .tile-net`)?.dataset.quality==='good',bId,{timeout:90000});
 ev('B quality good again');

 // 5d. Guest A's PeerConnection fails and recovers.
 const pcsBefore=await a.evaluate(()=>window.__pcs.length);
 await a.evaluate(()=>{const pc=window.__pcs.at(-1);Object.defineProperty(pc,'connectionState',{get:()=>'failed',configurable:true});pc.onconnectionstatechange?.(new Event('connectionstatechange'));});
 await a.waitForFunction(n=>window.__pcs.length>n&&window.__pcs.at(-1).connectionState==='connected',pcsBefore,{timeout:30000});
 for(const [name,page] of [['Host',host],['Guest A',a],['Guest B',b]])await decodesEveryone(page,everyone.filter(n=>n!==name),{camOff:['Pat']});
 assert.equal(await a.locator('#rec-indicator').isVisible(),true,'A kept recording through the PeerConnection failure');
 ev('A PeerConnection recovered');

 // 5e. Guest B's WebSocket drops once.
 const socketsBefore=await b.evaluate(()=>window.__sockets.length);
 await b.evaluate(()=>window.__sockets.at(-1).close(4001,'rehearsal drop'));
 await b.waitForFunction(n=>window.__sockets.length>n&&window.__sockets.at(-1).readyState===1,socketsBefore,{timeout:30000});
 await decodesEveryone(b,['Host','Guest A','Pat'],{camOff:['Pat']});
 assert.equal(await b.locator('#rec-indicator').isVisible(),true,'B kept recording through the socket drop');
 const detailAfterDrop=await(await fetch(`${origin}/api/v1/sessions/${sessionId}`,{headers:hostHeaders})).json();
 assert.equal(detailAfterDrop.participants.filter(p=>p.name==='Guest B').length,1,'B kept one identity');
 ev('B WebSocket reconnected');
 await sample('after faults');

 // 5f. Guest A adds a phone camera mid-take.
 await (await revealControl(a,'#phone-camera-btn')).click();
 await a.locator('#phone-camera-dialog [data-qr] svg').waitFor();
 const phoneLink=await a.locator('#phone-link-input').inputValue();
 const phone=await open(aBrowser,'Phone',{...devices['iPhone 13'],permissions:['camera','microphone'],viewport:{width:390,height:844}});
 await phone.goto(phoneLink);
 await phone.waitForFunction(()=>document.body.dataset.state==='recording',null,{timeout:45000});
 for(const page of [host,a])await page.waitForFunction(()=>[...document.querySelectorAll('#video-grid .tile[data-participant-id]')].some(t=>t.querySelector('.tile-name')?.textContent==='Guest A · Phone'&&t.querySelector('video')?.videoWidth>0),null,{timeout:30000});
 await a.locator('#phone-camera-dialog button[value=done]').click();
 const phoneJoinSec=(Date.now()-takeStart)/1000;ev('phone camera recording');

 // 5g. Run until the take is MINUTES long, sampling resources.
 const takeEnd=takeStart+MINUTES*60_000;let n=0;
 while(Date.now()<takeEnd){await wait(Math.min(10000,Math.max(0,takeEnd-Date.now())));if(++n%3===0)await sample(`take +${Math.round((Date.now()-takeStart)/1000)}s`);}
 for(const page of [host,a,b])assert.equal(await page.locator('#rec-indicator').isVisible(),true,'still recording at the end');
 await decodesEveryone(host,['Guest A','Guest B','Pat','Guest A · Phone'],{camOff:['Pat']});

 // ================= 6. Stop; uploads complete.
 begin('6 stop take, uploads complete');
 await pat.locator('#rec-btn').click();
 const takeSec=(Date.now()-takeStart)/1000;ev('stop',{takeSec:+takeSec.toFixed(1)});
 for(const page of [host,a,b])await page.waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='none',null,{timeout:30000});
 // Guests see their upload progress (acknowledged / queued) until done.
 const guestProgress=await Promise.all([a,b].map(p=>p.locator('#telemetry-chunks').innerText()));
 await phone.waitForFunction(()=>document.body.dataset.state==='done',null,{timeout:180000});
 const detail=await(await fetch(`${origin}/api/v1/sessions/${sessionId}`,{headers:hostHeaders})).json();
 const take=detail.takes.at(-1);
 let snap;const uploadsFrom=Date.now();
 for(;;){
  snap=await(await fetch(`${origin}/api/v1/takes/${take.id}`,{headers:hostHeaders})).json();
  if(snap.tracks.length>=7&&snap.tracks.every(t=>t.status==='assembled')&&!snap.missingParticipants?.length)break;
  if(Date.now()-uploadsFrom>300000)throw new Error('uploads did not complete: '+JSON.stringify(snap.tracks.map(t=>[t.participantName,t.sourceLabel,t.status,t.totalBytes])));
  await wait(500);
 }
 const uploadSec=(Date.now()-uploadsFrom)/1000;
 for(const p of [a,b])await p.waitForFunction(()=>/ 0\.0 MB waiting/.test(document.querySelector('#telemetry-chunks').textContent),null,{timeout:60000});
 const trackTable=snap.tracks.map(t=>({who:t.participantName,label:t.sourceLabel,kind:t.kind,MB:+(t.totalBytes/1048576).toFixed(1)}));
 console.table(trackTable);
 assert.equal(snap.tracks.length,7,'host cam, A cam, B cam, A PCM, B PCM, A screen, A phone');
 assert.ok(!snap.tracks.some(t=>t.participantName==='Pat'),'the producer recorded nothing');
 log('all uploads assembled',uploadSec.toFixed(1)+'s after stop; guest progress at stop',JSON.stringify(guestProgress));
 await sample('uploads complete');

 // ================= 7. Originals during export; editor ZIP; ffprobe.
 begin('7 originals + export + editor ZIP');
 await host.locator('#transfers-recovery-modal').waitFor({state:'visible'});
 await host.locator('#refresh-transfers-btn').click();
 await host.waitForFunction(()=>!document.querySelector('#prepare-export-btn').disabled&&document.querySelector('a#download-originals-btn'),null,{timeout:30000});
 const processed=host.waitForResponse(r=>r.url().endsWith('/postprocess')&&r.request().method()==='POST',{timeout:600000});
 const exportFrom=Date.now();
 await host.locator('#prepare-export-btn').click();
 const [originals]=await Promise.all([host.waitForEvent('download',{timeout:120000}),host.locator('a#download-originals-btn').click()]);
 const originalsZip=path.join(data,'originals.zip');await originals.saveAs(originalsZip);
 const originalsMB=(await stat(originalsZip)).size/1048576;
 const exportWhenOriginalsDone=(await(await fetch(`${origin}/api/v1/takes/${take.id}`,{headers:hostHeaders})).json()).postproc;
 log('originals downloaded',originalsMB.toFixed(1),'MB; export meanwhile',JSON.stringify({state:exportWhenOriginalsDone?.state,stage:exportWhenOriginalsDone?.stage,percent:exportWhenOriginalsDone?.percent}));
 const origDir=path.join(data,'originals');await mkdir(origDir);execFileSync('unzip',['-q',originalsZip,'-d',origDir]);
 const origManifest=JSON.parse(await readFile(path.join(origDir,'manifest.json'),'utf8'));
 assert.ok(existsSync(path.join(origDir,'offsets.json')),'originals carry offsets.json');
 const processedResponse=await processed;assert.ok(processedResponse.ok(),await processedResponse.text());
 const exportSec=(Date.now()-exportFrom)/1000;
 await host.locator('#export-modal').waitFor({state:'visible'});
 const [pkgDownload]=await Promise.all([host.waitForEvent('download',{timeout:120000}),host.locator('#open-folder-btn').click()]);
 const pkgZip=path.join(data,'editor-package.zip');await pkgDownload.saveAs(pkgZip);
 const pkgDir=path.join(data,'editor-package');await mkdir(pkgDir);execFileSync('unzip',['-q',pkgZip,'-d',pkgDir]);
 const findFile=async(dir,re)=>{for(const e of await readdir(dir,{withFileTypes:true})){const p=path.join(dir,e.name);if(e.isDirectory()){const f=await findFile(p,re);if(f)return f;}else if(re.test(e.name))return p;}return null;};
 const manifestPath=await findFile(pkgDir,/^manifest\.json$/);assert.ok(manifestPath,'manifest.json in the ZIP');
 const pkgRoot=path.dirname(manifestPath);
 const manifest=JSON.parse(await readFile(manifestPath,'utf8'));
 const sources=manifest.sources;
 const probe=f=>JSON.parse(execFileSync(ffprobe,['-v','error','-show_streams','-show_format','-of','json',f],{encoding:'utf8',env}));
 const angles=[],mics=[];
 for(const s of sources){
  const f=path.join(pkgRoot,s.file);assert.ok(existsSync(f),'missing '+s.file);
  const info=probe(f);const dur=Number(info.format.duration);
  execFileSync(ffmpeg,['-v','error','-i',f,'-f','null','-'],{stdio:'pipe',env});
  const row={participant:s.participant,label:s.label,source:s.source||'',kind:s.kind,offsetSec:+s.offsetSec.toFixed(2),durationSec:+dur.toFixed(2),endSec:+(s.offsetSec+dur).toFixed(2),mic:s.micProcessing?.resolved};
  (s.kind==='video'?angles:mics).push(row);
 }
 console.table([...angles,...mics]);
 const angle=(who,src='')=>angles.find(r=>r.participant===who&&r.source===src);
 for(const [who,src] of [['Host',''],['Guest A',''],['Guest B',''],['Guest A','screen'],['Guest A','phone']])assert.ok(angle(who,src),`angle ${who} ${src||'camera'} in export: `+JSON.stringify(angles));
 assert.equal(angles.length,5,'exactly five angles');
 for(const who of ['Host','Guest A','Guest B'])assert.ok(mics.some(r=>r.participant===who&&r.source===''),`${who} mic in export`);
 assert.ok(!sources.some(s=>s.participant==='Pat'),'producer absent from export');
 assert.ok(!(manifest.participants||[]).some(p=>p.name==='Pat'),'producer in manifest participants');
 assert.equal((manifest.participants||[]).find(p=>p.name==='Guest A')?.title,'Founder, Example Co','lower-third title in the manifest: '+JSON.stringify(manifest.participants));
 // Offsets + durations: full-length sources end with the take; the screen ends when the share stopped; the phone starts late.
 const takeLen=Math.max(...angles.filter(r=>r.source==='').map(r=>r.endSec));
 for(const r of [...angles,...mics].filter(r=>r.source===''||r.source==='phone'))assert.ok(Math.abs(r.endSec-takeLen)<=2,`${r.participant} ${r.label} ends at ${r.endSec}s, take ${takeLen}s`);
 assert.ok(Math.abs(takeLen-takeSec)<=4,`exported take ${takeLen}s vs wall clock ${takeSec.toFixed(1)}s`);
 const scr=angle('Guest A','screen');assert.ok(scr.offsetSec>2&&Math.abs(scr.durationSec-shareSec)<=2,`screen angle ${JSON.stringify(scr)} vs share ${shareSec.toFixed(1)}s`);
 const ph=angle('Guest A','phone');assert.ok(Math.abs(ph.offsetSec-phoneJoinSec)<=8,`phone offset ${ph.offsetSec}s vs joined at ~${phoneJoinSec.toFixed(1)}s`);
 // micProcessing per guest: A raw (headphones), B clean (auto, no headphones).
 const micOf=who=>sources.filter(s=>s.participant===who&&s.kind==='audio'&&s.source!=='screen'&&s.source!=='phone').map(s=>s.micProcessing);
 assert.ok(micOf('Guest A').length&&micOf('Guest A').every(m=>m?.resolved==='raw'&&m.headphones===true&&!m.echoCancellation),'Guest A micProcessing: '+JSON.stringify(micOf('Guest A')));
 assert.ok(micOf('Guest B').length&&micOf('Guest B').every(m=>m?.resolved==='clean'&&m.echoCancellation&&m.noiseSuppression&&!m.autoGainControl),'Guest B micProcessing: '+JSON.stringify(micOf('Guest B')));
 // Guest mics are the uncompressed PCM tracks.
 for(const who of ['Guest A','Guest B']){const s=sources.find(x=>x.participant===who&&x.kind==='audio'&&!x.source);const info=probe(path.join(pkgRoot,s.file));assert.ok(/^pcm_/.test(info.streams[0].codec_name),`${who} mic is PCM: ${info.streams[0].codec_name}`);}
 // Lip sync: every exported microphone against the same microphone's audio in
 // that person's own camera original (timestamped decode at the camera's clock
 // offset). One microphone feeds both, so any shift is an alignment error. The
 // guests' PCM is read from the untouched original placed at its export offset
 // (the exported WAV is drift-stretched, which blurs the waveform match).
 const pcmSync={};
 for(const who of ['Host','Guest A','Guest B']){
  const mic=sources.find(x=>x.participant===who&&x.kind==='audio'&&!x.source);
  const cam=origManifest.files.find(f=>f.participant===who&&f.kind==='video'&&!f.source);
  assert.ok(mic&&cam,`${who} mic + camera original`);
  const original=origManifest.files.find(f=>f.trackId===mic.id&&f.kind==='audio');
  const micFile=original?path.join(origDir,original.file):path.join(pkgRoot,mic.file);
  const windows=[];
  for(const frac of [0.15,0.3,0.45,0.6,0.75,0.9]){
   const t=takeLen*frac;
   const x=await decodeWindow(micFile,t-mic.offsetSec,6);
   const y=await timelineWindow(path.join(origDir,cam.file),t-cam.offsetSec-0.3,6.6);
   const {k,c}=bestLag(x,y);
   windows.push({t:Math.round(t),shiftMs:+((k/8000-0.3)*1000).toFixed(1),corr:+c.toFixed(2)});
  }
  // The host's mic is only in the exported WAV, whose drift correction
  // (atempo) blurs the waveform: a lower but still unambiguous peak.
  const good=windows.filter(w=>w.corr>=(original?0.5:0.35));
  pcmSync[who]={method:mic.method,windows};
  assert.ok(good.length>=3,`${who}: mic vs camera audio not comparable ${JSON.stringify(windows)}`);
  assert.ok(good.every(w=>Math.abs(w.shiftMs)<=20),`${who} mic is off its own camera audio: ${JSON.stringify(windows)} (method ${mic.method})`);
 }
 log('mic lip sync vs own camera audio',JSON.stringify(pcmSync));
 // Acoustic verification against the host's received copy of each mic
 // [SL-P0-10]: every person is measured (A's PeerConnection fails and
 // reconnects, B's uplink is impaired). All browsers share one machine and
 // one Chromium build, so the measured send-path delays should agree: their
 // spread bounds the relative cross-device error of the clock placement.
 const acousticSync={};
 for(const who of ['Host','Guest A','Guest B']){
  const s=sources.find(x=>x.participant===who&&x.kind==='video'&&!x.source);
  const a=s?.acoustic;
  assert.ok(a,`${who}: no acoustic check in the manifest`);
  acousticSync[who]={status:a.status,method:s.method,errorMs:a.errorMs,delayMs:a.delayMs,expectedDelayMs:a.expectedDelayMs,spreadMs:a.spreadMs,driftPpm:a.driftPpm,appliedMs:a.appliedMs,windows:`${a.usedWindows}/${a.windows}`};
  // Guest A records raw (headphones), so its call mic is a second capture of
  // the fake device: Chromium replays the WAV independently for each capture
  // (here ~1.2 s apart and drifting), so A's reference is not the same sound
  // as its recording. A real microphone is one sound for both captures. The
  // check must then refuse (unmeasurable), never verify or correct.
  if(who==='Guest A')assert.ok(['unmeasurable','verified'].includes(a.status),`${who}: acoustic check ${JSON.stringify(a)}`);
  else assert.ok(['verified','corrected'].includes(a.status),`${who}: acoustic check ${JSON.stringify(a)}`);
 }
 const delays=Object.values(acousticSync).filter(a=>a.status!=='unmeasurable').map(a=>a.delayMs);
 acousticSync.delaySpreadMs=+(Math.max(...delays)-Math.min(...delays)).toFixed(2);
 log('acoustic sync vs host reference',JSON.stringify(acousticSync));
 assert.ok(acousticSync.delaySpreadMs<=10,`send-path delays disagree across people: ${JSON.stringify(acousticSync)}`);
 const readme=await readFile(path.join(pkgRoot,'README.txt'),'utf8');
 assert.ok(!/not independently verified/.test(readme)&&/verified acoustically|corrected acoustically/.test(readme),'README reports the acoustic check: '+readme);
 // FCP7 XML references every exported file.
 const xmlPath=await findFile(pkgRoot,/_premiere\.xml$/);assert.ok(xmlPath,'FCP7 XML in the package');
 const xml=await readFile(xmlPath,'utf8');
 for(const s of sources)assert.ok(xml.includes(path.basename(s.file)),`FCP7 XML lacks ${s.file}`);
 assert.ok(!/Pat/.test(xml),'producer in the XML');
 log('export verified',JSON.stringify({exportSec:+exportSec.toFixed(1),angles:angles.length,mics:mics.length,takeLen}));
 await sample('export complete');

 // ================= 8. Built-in transcript + Parachute.
 begin('8 transcribe + send to Parachute');
 const eco=host.locator('#ecosystem-actions');
 // The p2p.video app's on-device recognizer (fake) is the default engine [SL-P4-3].
 const transcriber=await connectFakeTranscriber({origin,dataDir:data,jobMs:300});
 await eco.locator('[data-run="transcript"]:not([disabled])').waitFor({timeout:30000});
 await eco.locator('[data-run="transcript"]').click();
 await host.waitForFunction(()=>document.querySelector('#ecosystem-actions [data-action="transcript"] [data-badge]')?.textContent==='Done',null,{timeout:120000});
 await eco.locator('[data-run="parachute"]:not([disabled])').waitFor({timeout:30000});
 await eco.locator('[data-run="parachute"]').click();
 await host.waitForFunction(()=>document.querySelector('#ecosystem-actions [data-action="parachute"]')?.dataset.state==='success',null,{timeout:60000});
 const exportsRoot=path.join(data,'exports'),sessionDir=(await readdir(exportsRoot)).find(n=>!n.startsWith('.'));
 const pkgOnDisk=path.join(exportsRoot,sessionDir,(await readdir(path.join(exportsRoot,sessionDir))).find(n=>n.startsWith('take_')&&!n.includes('.previous-')));
 const spoken=JSON.parse(await readFile(path.join(pkgOnDisk,'transcript','transcript.json'),'utf8'));
 assert.equal(spoken.schema,'studiolink.transcript/1');
 assert.equal(spoken.engine.name,'studiolink-builtin');
 assert.ok(spoken.segments.length>=3,'one fake segment per voice: '+spoken.segments.length);
 assert.deepEqual(transcriber.stats.errors,[]);
 const speakers=new Set(spoken.segments.map(s=>s.speaker));
 assert.ok([...speakers].every(s=>['Host','Guest A','Guest B'].includes(s)),'transcript speakers: '+[...speakers]);
 for(const f of ['transcript/transcript.srt','transcript/transcript.vtt','transcript/transcript.md'])await readFile(path.join(pkgOnDisk,f));
 assert.equal(vaultRequests.length,1,JSON.stringify(vaultRequests.map(r=>[r.method,r.url])));
 const note=vaultRequests[0];
 assert.equal(note.method,'POST');assert.equal(note.url,'/vault/default/api/notes');
 assert.equal(note.auth,`Bearer ${vaultToken}`);assert.match(note.type,/application\/json/);
 assert.match(note.body.path,/^Meetings\/\d{4}-\d\d-\d\d \d{4} Rehearsal/);
 assert.deepEqual(note.body.tags.sort(),['meeting','podcast','studiolink']);
 assert.equal(note.body.metadata.source,'studiolink');assert.equal(note.body.metadata.transcript,'builtin');
 assert.match(note.body.content,/## Transcript/);
 for(const who of ['Host','Guest A','Guest B'])assert.ok(note.body.metadata.attendees.includes(who),'attendees: '+note.body.metadata.attendees);
 assert.ok(!note.body.metadata.attendees.includes('Pat'),'producer listed as attendee');
 transcriber.close();
 log('Built-in transcript + Parachute note ok',JSON.stringify({speakers:[...speakers],notePath:note.body.path}));

 // ================= 9. End meeting.
 begin('9 end meeting');
 await host.locator('#close-modal-btn').click();
 await host.locator('#end-meeting-btn').click();
 await host.locator('.room-dialog [data-confirm]').click();
 await host.locator('#captions-transcript-dialog').waitFor({state:'visible'});
 const [vttDl]=await Promise.all([host.waitForEvent('download'),host.locator('#captions-transcript-dialog [data-format="vtt"]').click()]);
 const vtt=await readFile(await vttDl.path(),'utf8');
 assert.match(vtt,/^WEBVTT/);assert.match(vtt,/<v Guest A>/);assert.match(vtt,/<v Guest B>/);assert.ok(!/<v Pat>/.test(vtt),'producer captioned');
 for(const page of [a,b,pat])await page.locator('#meeting-ended h1',{hasText:'The host ended the meeting'}).waitFor({timeout:30000});
 await host.locator('#captions-transcript-dialog button',{hasText:/Close|Done/}).first().click().catch(()=>{});
 await host.locator('#meeting-ended h1',{hasText:'Meeting ended'}).waitFor();
 await wait(1500);
 for(const page of [a,b,pat])assert.equal(await page.locator('#meeting-ended').isVisible(),true,'stayed on the end screen');
 provider.close();
 await sample('meeting ended');

 // ================= 10. Hygiene: errors and secrets.
 begin('10 log + error hygiene');
 const secrets={hostToken,guestLink:new URL(guestLink).searchParams.get('token'),producerLink:new URL(producerLink).searchParams.get('token'),phoneLink:new URL(phoneLink).searchParams.get('token'),
  desktop:(await readFile(path.join(data,'.desktop_token'),'utf8')).trim(),launch:(await readFile(path.join(data,'.launch_token'),'utf8')).trim(),vaultToken,streamKey};
 const secretHits=Object.entries(secrets).filter(([,v])=>v&&logs.includes(v)).map(([k])=>k);
 assert.deepEqual(secretHits,[],'secrets in the daemon log');
 assert.ok(!/heard on this stream|Speech heard|Tone heard/.test(logs),'caption text in the daemon log');
 assert.ok(!/Welcome to the rehearsal|Ready when you are/.test(logs),'chat text in the daemon log');
 assert.deepEqual(errors,[],'unexpected page/console errors');
 phase.sec=+((Date.now()-phase.start)/1000).toFixed(1);
 summary={ok:true,minutes:MINUTES,runDir,chromium:hostBrowser.version(),totalSec:+ts(),takeSec:+takeSec.toFixed(1),uploadSecAfterStop:+uploadSec.toFixed(1),exportSec:+exportSec.toFixed(1),originalsMB:+originalsMB.toFixed(1),
  phases:phases.map(p=>({name:p.name,sec:p.sec})),takeEvents:events,tracks:trackTable,angles,mics,signalBars:bars,netImpairWorstSeenByHost:worst,
  livestream:{receivedSec:+flvSec.toFixed(1),fps:liveStatus.progress?.fps,bitrateKbps:liveStatus.bitrateKbps,bitrateSource:liveStatus.bitrateSource},
  resources,peak:{rssMB:Math.max(...resources.map(r=>r.rssMB)),goroutines:Math.max(...resources.map(r=>r.goroutines??0)),openFDs:Math.max(...resources.map(r=>r.openFDs??0))},
  allowedConsoleErrors:allowedSeen,originalsFiles:origManifest.files?.length??null,pcmLipSync:pcmSync,acousticSync};
 log('REHEARSAL PASSED');
}catch(e){
 summary.error=String(e?.stack||e);
 console.error(e);console.error('page errors:',JSON.stringify(errors,null,1));
 for(const [name,page] of pages)if(!page.isClosed())console.error(`--- ${name}:`,(await page.locator('body').innerText({timeout:2000}).catch(()=>'')).slice(-1200));
 console.error('--- daemon log tail ---\n'+logs.split('\n').slice(-60).join('\n'));
 process.exitCode=1;
}finally{
 if(phase&&phase.sec===undefined)phase.sec=+((Date.now()-phase.start)/1000).toFixed(1);
 if(!summary.phases)summary.phases=phases.map(p=>({name:p.name,sec:p.sec}));
 if(!summary.resources)summary.resources=resources;
 await writeFile(path.join(runDir,'summary.json'),JSON.stringify(summary,null,1));
 console.log('summary:',path.join(runDir,'summary.json'));
 for(const b of browsers)await b.close().catch(()=>{});
 vault.close();
 for(const proc of [daemon,sink])if(proc.exitCode===null&&proc.signalCode===null){proc.kill('SIGTERM');await Promise.race([new Promise(r=>proc.once('exit',r)),wait(3000)]);if(proc.exitCode===null&&proc.signalCode===null)proc.kill('SIGKILL');}
 daemonLogFile.end();
 if(!process.env.REHEARSAL_KEEP)await rm(data,{recursive:true,force:true});else console.log('data kept:',data);
}
