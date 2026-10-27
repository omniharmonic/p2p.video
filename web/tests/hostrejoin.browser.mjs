// Host page closed mid-take, then reopened in the same profile [SL-P2-3]
// (the in-app studio window closed and opened again). The take keeps going on
// the host; the reopened page uploads the closed page's journaled fragments
// from OPFS, shows the running take after rejoining, and its Record button
// stops it. Real daemon, Chromium fake devices.
// Env: STUDIO_BINARY, STUDIO_TEST_PORT (default: free port).
import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {mkdtemp,rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {freePort,isolatedNetworkArgs,startTake,launchURL,readyToJoin} from './lib/harness.mjs';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const data=await mkdtemp(path.join(tmpdir(),'studio-hostrejoin-'));
const port=Number(process.env.STUDIO_TEST_PORT||await freePort('tcp')),origin=`http://127.0.0.1:${port}`;
const daemon=spawn(process.env.STUDIO_BINARY||'../bin/p2pvideod',['--data-dir',data,'--port',String(port),'--no-funnel',...await isolatedNetworkArgs()],{stdio:['ignore','pipe','pipe']});
let logs='',browser;const errors=[];daemon.stderr.on('data',b=>logs+=b);daemon.stdout.on('data',b=>logs+=b);
const wait=ms=>new Promise(r=>setTimeout(r,ms));
try{
 let ready=false;for(let i=0;i<100&&!ready;i++){try{ready=(await fetch(origin+'/health')).ok}catch{}if(!ready)await wait(100);}assert.ok(ready,'daemon did not start');
 browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream','--autoplay-policy=no-user-gesture-required','--mute-audio']});
 const context=await browser.newContext({permissions:['camera','microphone']});
 const open=async url=>{const p=await context.newPage();p.setDefaultTimeout(45000);p.on('pageerror',e=>errors.push(e.message));await p.goto(url);await p.waitForFunction(()=>document.querySelector('#home-view')?.offsetParent||document.querySelector('#preview-video').videoWidth>0);return p;};
 let host=await open(await launchURL(data,origin));await readyToJoin(host);await host.locator('#name-input').fill('Host');
 const created=host.waitForResponse(r=>r.url().endsWith('/api/v1/sessions')&&r.request().method()==='POST');
 await host.locator('#join-btn').click();const session=await(await created).json();await host.locator('#studio-view').waitFor({state:'visible'});
 await startTake(host);
 // Close only once a few fragments are journaled (fragments are seconds long).
 const journaled=()=>host.evaluate(async()=>{
  const root=await navigator.storage.getDirectory(),takes=await root.getDirectoryHandle('takes',{create:true});
  for await(const [,take] of takes.entries()){const tracks=await take.getDirectoryHandle('tracks');for await(const [,track] of tracks.entries()){const meta=JSON.parse(await(await(await track.getFileHandle('meta.json')).getFile()).text());if(!meta.isComplete)return meta.lastSeq;}}
  return -1;
 });
 for(let i=0;i<120&&await journaled()<2;i++)await wait(500);
 assert.ok(await journaled()>=2,'host never journaled 3 fragments');
 const headers={Authorization:`Bearer ${session.hostToken}`};
 const takeId=(await(await fetch(`${origin}/api/v1/sessions/${session.id}`,{headers})).json()).takes[0].id;
 await host.close({runBeforeUnload:false});
 // The take is still recording without the host page.
 await wait(1500);
 assert.equal((await(await fetch(`${origin}/api/v1/takes/${takeId}`,{headers})).json()).take.status,'recording');
 host=await open(`${origin}/?session=${encodeURIComponent(session.id)}&token=${encodeURIComponent(session.hostToken)}`);
 // Uploads of the closed page's fragments resume on load.
 await host.locator('#opfs-recovery-banner').waitFor({state:'visible'});
 await host.waitForFunction(()=>/1 saved on the host\. 0 still to upload/.test(document.querySelector('#opfs-recovery-banner')?.textContent||''),{},{timeout:60000})
  .catch(async e=>{throw new Error(`recovery did not finish: ${await host.locator('#opfs-recovery-banner').innerText()}`,{cause:e});});
 await host.locator('#join-btn').click();await host.locator('#studio-view').waitFor({state:'visible'});
 await host.waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='inline-flex'&&document.querySelector('#rec-btn').textContent==='Stop recording'&&!document.querySelector('#rec-btn').disabled);
 await host.locator('#rec-btn').click();
 let snapshot;for(let i=0;i<300;i++){snapshot=await(await fetch(`${origin}/api/v1/takes/${takeId}`,{headers})).json();if(snapshot.take.status!=='recording'&&snapshot.tracks.length&&snapshot.tracks.every(t=>t.status==='assembled'))break;await wait(100);}
 assert.notEqual(snapshot.take.status,'recording',JSON.stringify(snapshot.take));
 assert.ok(snapshot.tracks.length===1&&snapshot.tracks.every(t=>t.status==='assembled'&&t.totalBytes>1000),JSON.stringify(snapshot.tracks));
 await host.waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='none'&&document.querySelector('#rec-btn').textContent==='Start recording');
 assert.deepEqual(errors,[]);
 console.log(JSON.stringify({hostRejoinedMidTake:true,recoveredTrack:snapshot.tracks[0].status,stoppedFromRejoinedPage:true,errors}));
}catch(e){console.error(logs.slice(-4000));throw e;}
finally{await browser?.close();daemon.kill('SIGTERM');await rm(data,{recursive:true,force:true});}
