// Real signaling/RTC/recording recovery, fake capture devices, no audible playback.
// Env: STUDIO_BINARY (daemon), STUDIO_TEST_PORT (HTTP, default: free port),
// STUDIO_UDP_PORT (SFU mux, default: free port, kept across the restart),
// STUDIO_DAEMON_LOG (append daemon stdout/stderr here; default <data>/daemon.log),
// RESTART_RECOVERY_MS (max media recovery after the daemon is healthy, default 10000),
// RESTART_GAP_MS (daemon outage before it is started again, default 16000: launchd
// throttles KeepAlive respawns to >=10 s, so the client's reconnect backoff has grown).
import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {createWriteStream} from 'node:fs';
import {mkdtemp,rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import net from 'node:net';
import path from 'node:path';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
import {startTake,launchURL,readyToJoin} from './lib/harness.mjs';
const freePort=(type)=>new Promise((resolve,reject)=>{
 if(type==='udp'){import('node:dgram').then(({createSocket})=>{const s=createSocket('udp4');s.on('error',reject);s.bind(0,'127.0.0.1',()=>{const p=s.address().port;s.close(()=>resolve(p));});});return;}
 const s=net.createServer();s.on('error',reject);s.listen(0,'127.0.0.1',()=>{const p=s.address().port;s.close(()=>resolve(p));});
});
const data=await mkdtemp(path.join(tmpdir(),'studio-reconnect-'));
const port=Number(process.env.STUDIO_TEST_PORT||await freePort('tcp')),origin=`http://127.0.0.1:${port}`;
const udpPort=Number(process.env.STUDIO_UDP_PORT||await freePort('udp')),turnPort=await freePort('tcp');
const recoveryBudget=Number(process.env.RESTART_RECOVERY_MS||10000);
const logPath=process.env.STUDIO_DAEMON_LOG||path.join(data,'daemon.log');
const logFile=createWriteStream(logPath,{flags:'a'});
const t0=Date.now();const ts=()=>((Date.now()-t0)/1000).toFixed(2).padStart(7);
const timeline=[];// Never keep tokens (invite, host or resume) in the trace, even from browser console URLs.
const mark=(who,msg)=>timeline.push(`${ts()} ${who}: ${String(msg).replace(/(token=)[^&\s'"]+/gi,'$1[redacted]')}`);
let daemon,browser,daemonLogs='',ok=false;
const start=()=>{
 mark('test','spawn daemon');
 daemon=spawn(process.env.STUDIO_BINARY||'../bin/p2pvideod',['--data-dir',data,'--port',String(port),'--no-funnel','--udp-port',String(udpPort),'--turn-listen',`127.0.0.1:${turnPort}`],{stdio:['ignore','pipe','pipe']});
 const sink=b=>{logFile.write(b);daemonLogs+=b;};daemon.stdout.on('data',sink);daemon.stderr.on('data',sink);
};
async function ready(){for(let n=0;n<100;n++){try{if((await fetch(origin+'/health')).ok){mark('test','daemon healthy');return}}catch{}await new Promise(r=>setTimeout(r,100));}throw Error('daemon unavailable')}
try{
 start();await ready();browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream','--mute-audio']});
 const context=await browser.newContext({permissions:['camera','microphone']});
 await context.addInitScript(()=>{
  const Original=window.WebSocket;window.__sockets=[];window.WebSocket=class extends Original{constructor(...args){super(...args);window.__sockets.push(this);console.debug('[trace] ws open attempt',window.__sockets.length);this.addEventListener('close',e=>console.debug('[trace] ws close',e.code));this.addEventListener('open',()=>console.debug('[trace] ws opened'));}};
  const OriginalPC=window.RTCPeerConnection;window.__pcs=[];window.RTCPeerConnection=class extends OriginalPC{constructor(...args){super(...args);const n=window.__pcs.push(this);console.debug('[trace] pc',n,'created');this.addEventListener('connectionstatechange',()=>console.debug('[trace] pc',n,this.connectionState));this.addEventListener('signalingstatechange',()=>console.debug('[trace] pc',n,'sig',this.signalingState));}
   // Test hook: leave one server offer's setRemoteDescription pending forever, which is what
   // Chromium does when the peer is closed (daemon gone) while the operation is in flight.
   setRemoteDescription(d){if(d?.type==='offer'&&window.__hangNextServerOffer){window.__hangNextServerOffer=false;window.__hungOffer=true;console.debug('[trace] holding server offer in flight');return new Promise(()=>{});}return super.setRemoteDescription(d);}};
  addEventListener('DOMContentLoaded',()=>{for(const id of ['conn-text','telemetry-status']){const el=document.getElementById(id);if(el)new MutationObserver(()=>console.debug('[trace]',id,JSON.stringify(el.textContent))).observe(el,{childList:true,characterData:true,subtree:true});}});
 });
 const watch=(page,who)=>{page.on('console',m=>mark(who,m.text()));page.on('pageerror',e=>mark(who,'pageerror '+e.message));};
 const host=await context.newPage();watch(host,'host');await host.goto(await launchURL(data,origin));await readyToJoin(host);await host.locator('#name-input').fill('Synthetic host');
 const created=host.waitForResponse(r=>r.url().endsWith('/api/v1/sessions')&&r.request().method()==='POST');await host.locator('#join-btn').click();const session=await(await created).json();await host.locator('#studio-view').waitFor({state:'visible'});
 const guest=await context.newPage();watch(guest,'guest');await guest.goto(`${origin}/join?session=${session.id}&token=${session.token}`);await guest.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);await guest.locator('#name-input').fill('Synthetic guest');await guest.locator('#join-btn').click();await host.locator('#admit-btn').click();
 const decoded=async(p,timeout=20000)=>p.waitForFunction(()=>document.querySelector('#conn-text').textContent==='Remote video decoded',{},{timeout});await decoded(host);await decoded(guest);console.log('Initial peers decoded');
 const headers={Authorization:`Bearer ${session.hostToken}`};const detail=async()=>await(await fetch(`${origin}/api/v1/sessions/${session.id}`,{headers})).json();let d=await detail();const guestId=d.participants.find(p=>p.name==='Synthetic guest').id;
 await guest.evaluate(()=>window.__sockets.at(-1).close(4001,'test reconnect'));await guest.waitForFunction(()=>window.__sockets.length>=2,{},{timeout:12000});await decoded(guest);await decoded(host);d=await detail();assert.equal(d.participants.filter(p=>p.name==='Synthetic guest').length,1);assert.equal(d.participants.find(p=>p.name==='Synthetic guest').id,guestId);assert.equal(await host.locator('#admit-btn').isVisible(),false,'reconnect required repeated host admission');console.log('Guest socket loss recovered with stable identity');
 // Capture continues across signaling loss. Host stops while guest is offline;
 // reconnect must reconcile and stop the same take without a second recording.
 await startTake(host);await guest.waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='inline-flex');await new Promise(r=>setTimeout(r,1800));
 await guest.evaluate(()=>window.__sockets.at(-1).close(4001,'stop missed'));await host.locator('#rec-btn').click();await guest.waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='none',{},{timeout:15000});
 d=await detail();const take=d.takes.at(-1);let snapshot;
 for(let i=0;i<100;i++){snapshot=await(await fetch(`${origin}/api/v1/takes/${take.id}`,{headers})).json();if(snapshot.tracks?.length===2&&snapshot.tracks.every(t=>t.status==='assembled'))break;await new Promise(r=>setTimeout(r,100));}
 assert.equal(snapshot.tracks.length,2);assert.ok(snapshot.tracks.every(t=>t.status==='assembled'&&t.totalBytes>0));assert.equal(snapshot.missingParticipants.length,0);console.log('Missed Stop recovered with both originals assembled');
 // A daemon restart preserves the admitted identity but not the obsolete RTC peer.
 // Kill it while the host is mid-renegotiation (answering the SFU's offer for the
 // guest's re-published tracks): the abandoned SDP step must not wedge the host's
 // signaling, which used to leave it unable to process the new admission [SL-P0-7].
 await host.evaluate(()=>{window.__hangNextServerOffer=true;window.__hungOffer=false;});await guest.evaluate(()=>window.__sockets.at(-1).close(4001,'republish'));await host.waitForFunction(()=>window.__hungOffer===true,{},{timeout:15000});mark('test','host holds a server offer in flight');
 // The restarted daemon must rebind the same SFU UDP mux port, and media must
 // be back within the recovery budget of the daemon being healthy again.
 const exited=new Promise((resolve,reject)=>{const timeout=setTimeout(()=>reject(Error('daemon did not stop')),5000);daemon.once('exit',()=>{clearTimeout(timeout);resolve()});});mark('test','SIGTERM daemon');daemon.kill('SIGTERM');await exited;mark('test','daemon exited');console.log('Restarting daemon');
 const gap=Number(process.env.RESTART_GAP_MS??16000);if(gap>0)await new Promise(r=>setTimeout(r,gap));
 const logsBefore=daemonLogs.length;start();await ready();const healthyAt=Date.now();
 await decoded(host,recoveryBudget);await decoded(guest,Math.max(1,recoveryBudget-(Date.now()-healthyAt)));const recoveryMs=Date.now()-healthyAt;mark('test',`media recovered ${recoveryMs}ms after healthy`);
 console.log('--- restart timeline\n'+timeline.slice(timeline.findIndex(l=>l.includes('SIGTERM daemon'))).join('\n'));
 const restartLogs=daemonLogs.slice(logsBefore);
 assert.match(restartLogs,new RegExp(`SFU UDP mux listening.*port=${udpPort}`),'restarted daemon did not rebind the SFU UDP mux port');
 d=await detail();assert.equal(d.participants.filter(p=>p.name==='Synthetic guest').length,1);assert.equal(d.participants.find(p=>p.name==='Synthetic guest').id,guestId);
 console.log(JSON.stringify({automaticReconnect:true,stableGuestIdentity:true,noReadmission:true,missedStopRecovered:true,restartDuringRenegotiation:true,originalsAssembled:2,daemonRestartRecovered:true,restartGapMs:gap,restartRecoveryMs:recoveryMs,udpMuxRebound:true,syntheticCaptureOnly:true,playbackMuted:true}));ok=true;
}catch(e){
 console.error('--- timeline\n'+timeline.filter(l=>!l.includes('WebSocket connection to')).slice(-250).join('\n'));
 console.error('--- daemon log tail ('+logPath+')\n'+daemonLogs.slice(-6000));
 throw e;
}finally{
 await browser?.close();
 if(daemon&&daemon.exitCode===null&&daemon.signalCode===null){daemon.kill('SIGTERM');await Promise.race([new Promise(r=>daemon.once('exit',r)),new Promise(r=>setTimeout(r,3000))]);if(daemon.exitCode===null&&daemon.signalCode===null)daemon.kill('SIGKILL');}
 logFile.end();
 if(ok&&!process.env.STUDIO_DAEMON_LOG)await rm(data,{recursive:true,force:true}).catch(()=>{});
}
