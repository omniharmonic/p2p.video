// Network impairment [SL-P0-8]: a host and a guest in a call while the guest's
// uplink is throttled mid-take with Chrome DevTools Protocol network emulation
// (1 Mbit/s up, 150 ms latency, 3% WebRTC packet loss), then restored.
//
// What CDP throttling affects (Chromium 149, measured for this test): HTTP
// and WebSocket traffic always; WebRTC UDP sockets only if emulation was
// already active on that CDP session when the socket was created, after which
// later Network.emulateNetworkConditions calls on the same session apply to
// them live. So the guest is "pre-armed" with a no-op condition before it joins.
//
// Asserts: the host sees the guest's quality drop to fair/poor (net_stats +
// signal bars), the guest's camera caps step down (real sender parameters),
// guest uploads throttle to one at a time during the take and the host's
// dashboard shows the guest's upload backlog; after the impairment is removed
// quality and caps recover, the take assembles completely and the export decodes.
//
// Run: make build; cd web && STUDIO_TEST_PORT=18343 STUDIO_BINARY=$PWD/../bin/p2pvideod node tests/netimpair.browser.mjs
import assert from 'node:assert/strict';
import {spawn,execFileSync} from 'node:child_process';
import {mkdtemp,readdir,rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {freePort,isolatedNetworkArgs,startTake,launchURL,readyToJoin} from './lib/harness.mjs';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');

const IMPAIRED={offline:false,latency:150,downloadThroughput:-1,uploadThroughput:125_000,packetLoss:3,packetQueueLength:0,packetReordering:false};
// "Emulation on" but effectively unlimited: installs the throttle on WebRTC sockets.
const PREARM={offline:false,latency:0,downloadThroughput:-1,uploadThroughput:100_000_000,packetLoss:0,packetQueueLength:0,packetReordering:false};
const IMPAIR_MS=Number(process.env.NETIMPAIR_MS||30000);

const data=await mkdtemp(path.join(tmpdir(),'studio-netimpair-'));
const port=Number(process.env.STUDIO_TEST_PORT||await freePort('tcp')),origin=`http://127.0.0.1:${port}`;
const daemon=spawn(process.env.STUDIO_BINARY||'../bin/p2pvideod',['--data-dir',data,'--port',String(port),'--no-funnel','--no-public-stun',...await isolatedNetworkArgs()],{stdio:['ignore','pipe','pipe']});
let logs='',browser,debugDump=()=>{};const errors=[];const t0=Date.now();
const ts=()=>((Date.now()-t0)/1000).toFixed(1);
const log=(...a)=>console.log(`[${ts()}s]`,...a);
const sleep=ms=>new Promise(r=>setTimeout(r,ms));
daemon.stderr.on('data',b=>logs+=b);daemon.stdout.on('data',b=>logs+=b);
try{
 let ready=false;for(let i=0;i<100;i++){try{ready=(await fetch(origin+'/health')).ok;if(ready)break;}catch{}await sleep(100);}
 assert.ok(ready,'daemon did not start: '+logs);
 browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream','--autoplay-policy=no-user-gesture-required','--mute-audio']});
 const hostContext=await browser.newContext({permissions:['camera','microphone']});
 const guestContext=await browser.newContext({permissions:['camera','microphone']});
 await guestContext.addInitScript(()=>{
  const Native=window.RTCPeerConnection;window.__pcs=[];
  window.RTCPeerConnection=function(...args){const pc=new Native(...args);window.__pcs.push(pc);return pc;};
  window.RTCPeerConnection.prototype=Native.prototype;Object.setPrototypeOf(window.RTCPeerConnection,Native);
 });
 const host=await hostContext.newPage(),guest=await guestContext.newPage();
 for(const p of [host,guest]){p.setDefaultTimeout(45000);p.on('pageerror',e=>errors.push(e.message));p.on('console',m=>{if(m.type()==='error'&&!(m.text().includes('422')&&m.location().url.endsWith('/invite')))errors.push(`console: ${m.text()}`);});}

 // Host's view of the guest: every net_stats frame relayed by the hub.
 const reports=[];
 host.on('websocket',ws=>ws.on('framereceived',f=>{try{const env=JSON.parse(f.payload);if(env.t==='net_stats')reports.push({at:Date.now(),...env.payload});}catch{}}));
 // Guest chunk uploads in flight over time.
 let inflight=0;const putLog=[];
 const putDone=r=>{if(r.method()==='PUT'&&r.url().includes('/chunks/')){inflight--;putLog.push({at:Date.now(),inflight,seq:r.url().split('/').pop()});}};
 guest.on('request',r=>{if(r.method()==='PUT'&&r.url().includes('/chunks/')){inflight++;putLog.push({at:Date.now(),inflight});}});
 // fetch() resolves on response headers (the queue never reads the body), so
 // 'response' marks the end of an upload as the scheduler sees it.
 guest.on('response',r=>putDone(r.request()));guest.on('requestfailed',putDone);
 debugDump=()=>{if(!process.env.NETIMPAIR_DEBUG)return;
  console.log('PUT timeline',JSON.stringify(putLog.map(e=>[((e.at-t0)/1000).toFixed(1),e.inflight,e.seq])));
  console.log('reports',JSON.stringify(reports.map(r=>[((r.at-t0)/1000).toFixed(1),r.name,r.quality,r.capTier,r.upload?.mode,r.upload?.concurrency,r.upload?.pendingBytes,r.upload?.rateBps,r.availableOutgoingKbps,r.qualityLimitation,r.rttMs])));};
 const peakPuts=(from,to=Infinity)=>putLog.filter(e=>e.at>=from&&e.at<=to).reduce((m,e)=>Math.max(m,e.inflight),0);

 await host.goto(await launchURL(data,origin));await readyToJoin(host);
 await host.locator('#name-input').fill('Host');
 const created=host.waitForResponse(r=>r.url().endsWith('/api/v1/sessions')&&r.request().method()==='POST');
 await host.locator('#join-btn').click();const session=await (await created).json();
 await host.locator('#studio-view').waitFor({state:'visible'});
 await guest.goto(`${origin}/join?session=${session.id}&token=${session.token}`);
 await guest.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
 const cdp=await guestContext.newCDPSession(guest);await cdp.send('Network.enable');
 await cdp.send('Network.emulateNetworkConditions',PREARM);
 await guest.locator('#name-input').fill('Guest');await guest.locator('#join-btn').click();
 await host.locator('#admit-btn').click();await guest.locator('#studio-view').waitFor({state:'visible'});
 for(const p of [host,guest])await p.waitForFunction(()=>document.querySelector('#conn-text').textContent==='Remote video decoded',{},{timeout:30000});
 log('call up, remote video decoded on both sides');
 const guestId=(await (await fetch(`${origin}/api/v1/sessions/${session.id}`,{headers:{Authorization:`Bearer ${session.hostToken}`}})).json()).participants.find(p=>p.name==='Guest').id;
 const latest=()=>reports.filter(r=>r.participantId===guestId).at(-1);
 const waitReport=async(pred,what,timeout=60000)=>{const end=Date.now()+timeout;for(;;){const r=reports.filter(x=>x.participantId===guestId&&pred(x));if(r.length)return r.at(-1);if(Date.now()>end)throw new Error(`timed out waiting for ${what}; last report ${JSON.stringify(latest())}`);await sleep(250);}};
 const hostBars=()=>host.evaluate(id=>document.querySelector(`#tile-${CSS.escape(id)} .tile-net`)?.dataset.quality,guestId);
 const guestVideoCap=()=>guest.evaluate(()=>{for(const pc of window.__pcs){if(pc.connectionState!=='connected')continue;for(const s of pc.getSenders()){if(s.track?.kind==='video'){const e=s.getParameters().encodings?.[0];return e?{maxBitrate:e.maxBitrate,scale:e.scaleResolutionDownBy}:null;}}}return null;});

 // 1. Baseline: good quality, top tier, bars on the host's tile for the guest.
 const base=await waitReport(r=>r.quality==='good'&&r.capTier===0,'baseline good report',30000);
 assert.equal(base.name,'Guest');
 await host.waitForFunction(id=>document.querySelector(`#tile-${CSS.escape(id)} .tile-net`)?.dataset.quality==='good',guestId,{timeout:10000});
 assert.equal((await guestVideoCap())?.maxBitrate,1_500_000);
 log('baseline',JSON.stringify(base));

 // 2. Take starts on a healthy network: live upload mode.
 await startTake(host);await guest.waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='inline-flex');
 const takeStart=Date.now();
 const live=await waitReport(r=>r.at>takeStart&&r.upload?.mode==='live','live upload mode',15000);
 assert.equal(live.upload.concurrency,2);
 await sleep(4000);
 log('take recording, live uploads peak',peakPuts(takeStart));

 // 3. Impair the guest's uplink.
 const impairAt=Date.now();
 await cdp.send('Network.emulateNetworkConditions',IMPAIRED);
 log('impairment on',JSON.stringify(IMPAIRED));
 const degraded=await waitReport(r=>r.at>impairAt&&(r.quality==='fair'||r.quality==='poor'),'quality drop',30000);
 log('host sees guest quality',degraded.quality,'after',((degraded.at-impairAt)/1000).toFixed(1),'s',JSON.stringify(degraded));
 await host.waitForFunction(id=>['fair','poor'].includes(document.querySelector(`#tile-${CSS.escape(id)} .tile-net`)?.dataset.quality),guestId,{timeout:10000});
 const stepped=await waitReport(r=>r.at>impairAt&&r.capTier>=1,'camera caps stepping down',40000);
 log('caps stepped to tier',stepped.capTier,'after',((stepped.at-impairAt)/1000).toFixed(1),'s');
 const throttled=await waitReport(r=>r.at>impairAt&&r.upload?.mode==='throttled','upload throttling',40000);
 assert.equal(throttled.upload.concurrency,1);
 assert.ok(throttled.upload.rateLimitBps>0);
 const throttledAt=throttled.at;
 log('uploads throttled',JSON.stringify(throttled.upload));
 const cap=await guestVideoCap();
 assert.ok(cap&&cap.maxBitrate<=900_000,'guest sender maxBitrate not lowered: '+JSON.stringify(cap));
 log('guest sender parameters',JSON.stringify(cap));
 // Stay impaired; the backlog grows in OPFS while uploads trickle.
 const holdUntil=impairAt+IMPAIR_MS;while(Date.now()<holdUntil)await sleep(500);
 const impairedReports=reports.filter(r=>r.participantId===guestId&&r.at>throttledAt+2500&&r.at<=Date.now());
 // A PUT that started just before the switch may still be finishing; after it, one at a time.
 const settleAt=throttledAt+2500;
 const throttledPeak=peakPuts(settleAt,Date.now());
 assert.ok(throttledPeak<=1,`throttled uploads ran ${throttledPeak} at once`);
 const backlog=await waitReport(r=>r.at>impairAt&&r.upload?.pendingBytes>0,'upload backlog',5000);
 const backlogText=await host.evaluate(()=>document.querySelector('.health-upload-backlog')?.textContent||'');
 assert.match(backlogText,/Still uploading.*Guest: \d/,'host dashboard does not show the guest upload backlog');
 log('throttled window peak PUTs',throttledPeak,'reports',impairedReports.length,'backlog',backlog.upload.pendingBytes,'host sees:',backlogText);
 const worst=reports.filter(r=>r.participantId===guestId&&r.at>impairAt).reduce((m,r)=>Math.max(m,r.capTier),0);

 // 4. Remove the impairment and press Stop right away: the paced backlog
 // drains at full speed (concurrency up to 4) while the call keeps going.
 const restoreAt=Date.now();
 await cdp.send('Network.emulateNetworkConditions',PREARM);
 log('impairment off');
 const pendingAtStop=latest()?.upload?.pendingBytes??0;
 const stopAt=Date.now();
 await host.locator('#rec-btn').click();
 await guest.waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='none',{},{timeout:20000});
 const drained=await waitReport(r=>r.at>stopAt&&(r.upload?.mode==='drain'||r.upload?.mode==='idle'),'drain mode after Stop',20000);
 assert.equal(drained.upload.concurrency,4);
 log('after Stop upload mode',drained.upload.mode,'concurrency',drained.upload.concurrency,'pending at Stop',pendingAtStop);

 // 5. The call recovers: quality first, then the camera caps step back up.
 const good=await waitReport(r=>r.at>restoreAt&&r.quality==='good','quality recovery',60000);
 log('quality good again after',((good.at-restoreAt)/1000).toFixed(1),'s');
 await host.waitForFunction(id=>document.querySelector(`#tile-${CSS.escape(id)} .tile-net`)?.dataset.quality==='good',guestId,{timeout:10000});
 const top=await waitReport(r=>r.at>restoreAt&&r.capTier===0,'caps recovery to tier 0',120000);
 log('caps back to tier 0 after',((top.at-restoreAt)/1000).toFixed(1),'s');
 await guest.waitForFunction(()=>window.__pcs.some(pc=>pc.connectionState==='connected'&&pc.getSenders().some(s=>s.track?.kind==='video'&&s.getParameters().encodings?.[0]?.maxBitrate===1_500_000)),{},{timeout:10000});
 const drainPeak=peakPuts(stopAt);
 // Several chunks were waiting (one ~5 s MediaRecorder chunk per 1 MB here): they go in parallel.
 // Drain concurrency is asserted from the reported pacer state above. The
// observed in-flight peak depends on 2 s sampling vs. chunk timing (a ~2 MB
// backlog is only two chunks), so it is logged, not asserted (1/4 flake).
if(pendingAtStop>=2_000_000&&drainPeak<2)log('note: drain peak observed',drainPeak,'for backlog',pendingAtStop);

 // 6. The take assembles completely and the export decodes.
 const headers={Authorization:`Bearer ${session.hostToken}`};
 const detail=await (await fetch(`${origin}/api/v1/sessions/${session.id}`,{headers})).json();const take=detail.takes.at(-1);
 let snapshot;for(let i=0;i<600;i++){snapshot=await (await fetch(`${origin}/api/v1/takes/${take.id}`,{headers})).json();if(snapshot.tracks?.length===2&&snapshot.tracks.every(t=>t.status==='assembled'))break;await sleep(250);}
 assert.equal(snapshot.tracks.length,2,JSON.stringify(snapshot));
 assert.ok(snapshot.tracks.every(t=>t.status==='assembled'&&t.totalBytes>1000),JSON.stringify(snapshot.tracks));
 assert.equal(snapshot.missingParticipants?.length??0,0);
 log('take assembled',snapshot.tracks.map(t=>`${t.totalBytes} B`).join(', '),'drain peak PUTs',drainPeak);
 const processed=await fetch(`${origin}/api/v1/takes/${take.id}/postprocess`,{method:'POST',headers});
 assert.ok(processed.ok,await processed.text());
 const out=path.join(data,'sessions',session.id,'takes',take.id,'out');
 const files=[...(await readdir(path.join(out,'video'))).map(f=>path.join(out,'video',f)),...(await readdir(path.join(out,'audio'))).map(f=>path.join(out,'audio',f))];
 assert.ok(files.length>=4,'export files: '+files.join(','));
 for(const f of files)execFileSync('ffmpeg',['-v','error','-i',f,'-f','null','-'],{stdio:'pipe'});
 log('export decoded',files.length,'files');
 debugDump();
 assert.deepEqual(errors,[]);
 console.log(JSON.stringify({ok:true,baseline:{quality:base.quality,capTier:base.capTier,rttMs:base.rttMs},
  impaired:{quality:degraded.quality,qualityAfterS:+((degraded.at-impairAt)/1000).toFixed(1),worstCapTier:worst,capStepAfterS:+((stepped.at-impairAt)/1000).toFixed(1),sender:cap,upload:throttled.upload,throttledPeakPuts:throttledPeak,rttMs:degraded.rttMs,lossPct:degraded.outboundLossPct,availableOutgoingKbps:degraded.availableOutgoingKbps,reasons:degraded.reasons},
  recovered:{qualityAfterS:+((good.at-restoreAt)/1000).toFixed(1),capsAfterS:+((top.at-restoreAt)/1000).toFixed(1)},
  livePeakPuts:peakPuts(takeStart,impairAt),pendingAtStop,drainPeakPuts:drainPeak,assembledTracks:snapshot.tracks.length,exportedFiles:files.length}));
}catch(e){
 try{debugDump();}catch{}
 console.error('page errors:',JSON.stringify(errors));console.error(logs.slice(-6000));throw e;
}finally{
 await browser?.close();
 if(daemon.exitCode===null&&daemon.signalCode===null){daemon.kill('SIGTERM');await Promise.race([new Promise(r=>daemon.once('exit',r)),new Promise(r=>setTimeout(r,3000))]);if(daemon.exitCode===null&&daemon.signalCode===null)daemon.kill('SIGKILL');}
 await rm(data,{recursive:true,force:true});
}
