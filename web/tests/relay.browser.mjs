// Public-guest relay path [SL-P0-1]: the guest's browser is forced to
// iceTransportPolicy "relay", so its only way to reach the SFU is the embedded
// TURN server (TURN-over-TCP on loopback, i.e. the Funnel :8443 path without
// TLS). Asserts remote video decodes on both sides and the guest's selected
// candidate pair uses a relay local candidate.
// Build first (make build); STUDIO_BINARY points to the daemon.
import assert from 'node:assert/strict';
import {launchURL,readyToJoin} from './lib/harness.mjs';
import {spawn} from 'node:child_process';
import {mkdtemp,rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import net from 'node:net';
import path from 'node:path';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');

const freePort=(type)=>new Promise((resolve,reject)=>{
 if(type==='udp'){import('node:dgram').then(({createSocket})=>{const s=createSocket('udp4');s.on('error',reject);s.bind(0,'127.0.0.1',()=>{const p=s.address().port;s.close(()=>resolve(p));});});return;}
 const s=net.createServer();s.on('error',reject);s.listen(0,'127.0.0.1',()=>{const p=s.address().port;s.close(()=>resolve(p));});
});
const data=await mkdtemp(path.join(tmpdir(),'studio-relay-'));
const port=Number(process.env.STUDIO_TEST_PORT||await freePort('tcp')),origin=`http://127.0.0.1:${port}`;
const turnPort=await freePort('tcp'),udpPort=await freePort('udp');
const turnURL=`turn:127.0.0.1:${turnPort}?transport=tcp`;
const daemon=spawn(process.env.STUDIO_BINARY||'../bin/p2pvideod',[
 '--data-dir',data,'--port',String(port),'--no-funnel','--no-public-stun',
 '--udp-port',String(udpPort),'--turn-listen',`127.0.0.1:${turnPort}`,'--turn-url',turnURL,
],{stdio:['ignore','pipe','pipe']});
let logs='',browser,host,guest;const errors=[];
daemon.stderr.on('data',b=>logs+=b);daemon.stdout.on('data',b=>logs+=b);
try{
 let ready=false;for(let i=0;i<100;i++){try{ready=(await fetch(origin+'/health')).ok;if(ready)break;}catch{}await new Promise(r=>setTimeout(r,100));}
 assert.ok(ready,'daemon did not start: '+logs);
 assert.match(logs,/TURN relay listening/,'TURN relay not started: '+logs);
 assert.match(logs,new RegExp(`SFU UDP mux listening.*port=${udpPort}`),'SFU UDP mux not bound: '+logs);
 browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream','--autoplay-policy=no-user-gesture-required']});
 const hostContext=await browser.newContext({permissions:['camera','microphone']});
 const guestContext=await browser.newContext({permissions:['camera','microphone']});
 // Guest: force relay-only ICE and keep handles for getStats().
 await guestContext.addInitScript(()=>{
  const Native=window.RTCPeerConnection;window.__pcs=[];window.__iceConfigs=[];
  window.RTCPeerConnection=function(config={},...rest){
   const forced={...config,iceTransportPolicy:'relay'};window.__iceConfigs.push(JSON.parse(JSON.stringify(forced)));
   const pc=new Native(forced,...rest);window.__pcs.push(pc);return pc;
  };
  window.RTCPeerConnection.prototype=Native.prototype;
  Object.setPrototypeOf(window.RTCPeerConnection,Native);
 });
 host=await hostContext.newPage();guest=await guestContext.newPage();
 for(const p of [host,guest]){p.setDefaultTimeout(45000);p.on('pageerror',e=>errors.push(e.message));p.on('console',m=>{if(m.type()==='error'&&!(m.text().includes('422')&&m.location().url.endsWith('/invite')))errors.push(`console: ${m.text()}`);});}
 await host.goto(await launchURL(data,origin));await readyToJoin(host);
 await host.locator('#name-input').fill('Host');
 const created=host.waitForResponse(r=>r.url().endsWith('/api/v1/sessions')&&r.request().method()==='POST');
 await host.locator('#join-btn').click();const session=await (await created).json();
 await host.locator('#studio-view').waitFor({state:'visible'});console.log('host joined');
 await guest.goto(`${origin}/join?session=${session.id}&token=${session.token}`);
 await guest.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
 await guest.locator('#name-input').fill('Guest');await guest.locator('#join-btn').click();
 // Pending guests must not have created a PeerConnection with TURN credentials.
 await host.locator('#admit-btn').waitFor({state:'visible'});
 assert.equal(await guest.evaluate(()=>window.__iceConfigs.some(c=>(c.iceServers||[]).some(s=>s.credential))),false,'TURN credential present before admission');
 await host.locator('#admit-btn').click();await guest.locator('#studio-view').waitFor({state:'visible'});console.log('guest admitted');
 for(const p of [host,guest])await p.waitForFunction(()=>[...document.querySelectorAll('[id^="remote-"]')].some(v=>v.tagName==='VIDEO'&&v.videoWidth>0),{},{timeout:30000});
 console.log('remote video decoded on both sides');
 // A freshly nominated pair can briefly report zero bytes; poll for a settled sample.
 let result;
 for(let attempt=0;attempt<20;attempt++){
 result=await guest.evaluate(async()=>{
  const out=[];
  for(const pc of window.__pcs){
   if(pc.connectionState!=='connected')continue;
   const stats=await pc.getStats();let pairId;
   stats.forEach(s=>{if(s.type==='transport'&&s.selectedCandidatePairId)pairId=s.selectedCandidatePairId;});
   const pair=pairId&&stats.get(pairId);const local=pair&&stats.get(pair.localCandidateId),remote=pair&&stats.get(pair.remoteCandidateId);
   const localTypes=[];let inbound=0;stats.forEach(s=>{if(s.type==='local-candidate')localTypes.push(s.candidateType);if(s.type==='inbound-rtp')inbound+=s.bytesReceived||0;});
   out.push({policy:pc.getConfiguration().iceTransportPolicy,state:pair?.state,local:local&&{type:local.candidateType,protocol:local.protocol,relayProtocol:local.relayProtocol,address:local.address},remote:remote&&{type:remote.candidateType,address:remote.address,port:remote.port},localTypes,bytesReceived:pair?.bytesReceived,inboundRtpBytes:inbound});
  }
  return {pcs:out,configs:window.__iceConfigs};
 });
 if(result.pcs.some(p=>p.local&&p.inboundRtpBytes>0))break;await new Promise(r=>setTimeout(r,250));
 }
 console.log(JSON.stringify(result.pcs));
 const connected=result.pcs.filter(p=>p.local);
 assert.ok(connected.length>=1,'guest has no connected PeerConnection: '+JSON.stringify(result));
 for(const p of connected){
  assert.equal(p.policy,'relay');assert.equal(p.local.type,'relay','selected local candidate is not relay: '+JSON.stringify(p));
  assert.equal(p.local.relayProtocol,'tcp');assert.ok(p.localTypes.every(t=>t==='relay'),'non-relay local candidate gathered');
  assert.equal(p.remote.port,udpPort,'relay did not reach the SFU UDP mux port');
  // Relay-only policy means every inbound RTP byte crossed the TURN relay; pair
  // counters can lag after a nomination switch between the two relay candidates.
  assert.ok(p.inboundRtpBytes>0,'no media received over relay: '+JSON.stringify(p));
 }
 // The admitted guest's TURN entry is participant-bound and never logged.
 const turnEntry=result.configs.flatMap(c=>c.iceServers||[]).find(s=>(s.urls||[]).includes(turnURL));
 assert.ok(turnEntry?.username&&turnEntry?.credential,'no TURN credential in admission');
 assert.match(turnEntry.username,/^\d+:p_/);
 assert.equal(logs.includes(turnEntry.credential),false,'TURN credential leaked into daemon logs');
 assert.equal(result.configs.flatMap(c=>c.iceServers||[]).some(s=>(s.urls||[]).some(u=>u.startsWith('stun:'))),false,'--no-public-stun ignored');
 assert.deepEqual(errors,[]);
 console.log(JSON.stringify({relayOnlyGuest:true,remoteVideoDecoded:true,selectedLocalCandidate:connected[0].local,selectedRemote:connected[0].remote,errors}));
}catch(e){
 console.error('page errors:',JSON.stringify(errors));console.error(logs);throw e;
}finally{
 await browser?.close();
 if(daemon.exitCode===null&&daemon.signalCode===null){daemon.kill('SIGTERM');await Promise.race([new Promise(r=>daemon.once('exit',r)),new Promise(r=>setTimeout(r,3000))]);if(daemon.exitCode===null&&daemon.signalCode===null)daemon.kill('SIGKILL');}
 await rm(data,{recursive:true,force:true});
}
