// Guest tab closed mid-take, then the invite reopened in the same browser
// profile [SL-P0-7]. Real daemon, Chromium fake devices, playback muted.
// The guest's tail fragments never reach the host before the tab dies (their
// uploads are dropped), so only the OPFS journal has them. Reopening the invite
// must resume and finalize the interrupted track automatically (as a partial
// capture with every committed fragment), and the guest must rejoin the call
// cleanly: one admission, media both ways, no stale tile, take still stoppable.
// Env: STUDIO_BINARY, STUDIO_TEST_PORT (default: free port).
import assert from 'node:assert/strict';
import {spawn,execFileSync} from 'node:child_process';
import {mkdtemp,rm,readFile} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {freePort,isolatedNetworkArgs,startTake,launchURL,readyToJoin} from './lib/harness.mjs';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const data=await mkdtemp(path.join(tmpdir(),'studio-tabkill-'));
const port=Number(process.env.STUDIO_TEST_PORT||await freePort('tcp')),origin=`http://127.0.0.1:${port}`;
const daemon=spawn(process.env.STUDIO_BINARY||'../bin/p2pvideod',['--data-dir',data,'--port',String(port),'--no-funnel',...await isolatedNetworkArgs()],{stdio:['ignore','pipe','pipe']});
let logs='',browser;const errors=[];daemon.stderr.on('data',b=>logs+=b);daemon.stdout.on('data',b=>logs+=b);
const wait=ms=>new Promise(r=>setTimeout(r,ms));
const decoded=(page,timeout=30000)=>page.waitForFunction(()=>{const t=[...document.querySelectorAll('.tile[data-participant-id]')];return t.length===1&&t[0].querySelector('video')?.videoWidth>0&&document.querySelector('#conn-text').textContent==='Remote video decoded';},{},{timeout});
try{
 let ready=false;for(let i=0;i<100&&!ready;i++){try{ready=(await fetch(origin+'/health')).ok}catch{}if(!ready)await wait(100);}assert.ok(ready,'daemon did not start');
 browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream','--autoplay-policy=no-user-gesture-required','--mute-audio']});
 const watch=(page,name)=>{page.setDefaultTimeout(45000);page.on('pageerror',e=>errors.push(`${name}: ${e.message}`));};
 const hostContext=await browser.newContext({permissions:['camera','microphone']});
 const guestContext=await browser.newContext({permissions:['camera','microphone']});
 const host=await hostContext.newPage();watch(host,'host');
 await host.goto(await launchURL(data,origin));await readyToJoin(host);await host.locator('#name-input').fill('Host');
 const created=host.waitForResponse(r=>r.url().endsWith('/api/v1/sessions')&&r.request().method()==='POST');
 await host.locator('#join-btn').click();const session=await(await created).json();await host.locator('#studio-view').waitFor({state:'visible'});
 const invite=`${origin}/join?session=${session.id}&token=${session.token}`;
 const join=async()=>{
  const page=await guestContext.newPage();watch(page,'guest');
  await page.goto(invite);await page.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
  await page.locator('#name-input').fill('Synthetic guest');await page.locator('#join-btn').click();
  await host.waitForFunction(()=>document.querySelector('#host-admission-banner').style.display==='flex'&&document.querySelector('#admission-guest-name').textContent==='Synthetic guest');
  await host.locator('#admit-btn').click();await page.locator('#studio-view').waitFor({state:'visible'});
  return page;
 };
 let guest=await join();await decoded(host);await decoded(guest);console.log('call up');
 await startTake(host);await guest.waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='inline-flex');
 // The guest's open (unfinished) track journal in OPFS.
 const journal=page=>page.evaluate(async()=>{
  const root=await navigator.storage.getDirectory(),takes=await root.getDirectoryHandle('takes',{create:true});
  for await(const [,take] of takes.entries()){const tracks=await take.getDirectoryHandle('tracks');for await(const [,track] of tracks.entries()){const meta=JSON.parse(await(await(await track.getFileHandle('meta.json')).getFile()).text());if(!meta.isComplete)return meta;}}
  return null;
 });
 const journaledAtLeast=async n=>{for(let i=0;i<120;i++){const m=await journal(guest);if(m&&m.lastSeq>=n)return m;await wait(500);}throw Error(`guest never journaled ${n+1} fragments`);};
 const atDrop=await journaledAtLeast(2);
 // From now on the guest's fragment uploads are lost in transit: the tail only lives in OPFS.
 let dropped=0;await guestContext.route('**/api/v1/takes/*/tracks/*/chunks/*',route=>{dropped++;return route.abort('connectionfailed');});
 const before=await journaledAtLeast(atDrop.lastSeq+4);
 assert.ok(before&&before.lastSeq>=5,`guest journaled too little: ${JSON.stringify(before&&{lastSeq:before.lastSeq})}`);
 assert.ok(before.sidecar?.events?.some(e=>e.type==='recorder-start'),'no timing checkpoint journaled');
 const headers={Authorization:`Bearer ${session.hostToken}`};
 const serverBefore=await(await fetch(`${origin}/api/v1/takes/${before.takeId}/tracks/${before.trackId}/status`,{headers:{Authorization:`Bearer ${before.uploadToken}`}})).json();
 const serverHad=serverBefore.have.reduce((n,[a,b])=>n+b-a+1,0);
 assert.ok(dropped>0&&serverHad<=before.lastSeq,`expected an unsent tail (server has ${serverHad}, local lastSeq ${before.lastSeq})`);
 console.log(`guest journaled ${before.lastSeq+1} fragments, host has ${serverHad}; killing tab`);
 await guest.close({runBeforeUnload:false});
 await guestContext.unroute('**/api/v1/takes/*/tracks/*/chunks/*');
 // Host notices the guest left; the take keeps recording.
 await host.waitForFunction(()=>document.querySelectorAll('.tile[data-participant-id]').length===0,{},{timeout:30000});
 assert.equal(await host.locator('#rec-indicator').isVisible(),true,'take stopped when a guest left');
 // Reopen the invite in the same profile: uploads resume by themselves.
 guest=await join();
 await guest.locator('#opfs-recovery-banner').waitFor({state:'visible'});
 await guest.waitForFunction(()=>/1 saved on the host\. 0 still to upload/.test(document.querySelector('#opfs-recovery-banner')?.textContent||''),{},{timeout:60000});
 await decoded(host);await decoded(guest);
 assert.equal(await host.locator('#admit-btn').isVisible(),false);
 console.log('recovered and rejoined');
 await wait(1500);await host.locator('#rec-btn').click();
 let snapshot;for(let i=0;i<300;i++){snapshot=await(await fetch(`${origin}/api/v1/takes/${before.takeId}`,{headers})).json();if(snapshot.take.status!=='recording'&&snapshot.tracks.every(t=>t.status==='assembled'))break;await wait(100);}
 const recovered=snapshot.tracks.find(t=>t.trackId===before.trackId);
 assert.equal(recovered?.status,'assembled',JSON.stringify(snapshot.tracks));
 assert.equal(snapshot.tracks.length,2,'host track + recovered guest track');
 assert.ok(snapshot.tracks.every(t=>t.status==='assembled'),JSON.stringify(snapshot.tracks));
 assert.equal(snapshot.missingParticipants.length,0,JSON.stringify(snapshot.missingParticipants));
 const raw=path.join(data,'sessions',session.id,'takes',before.takeId,'raw');
 const sidecar=JSON.parse(await readFile(path.join(raw,`${before.trackId}.sidecar.json`),'utf8'));
 assert.equal(sidecar.captureComplete,false);assert.ok(sidecar.events.some(e=>e.type==='capture-interrupted'));
 const status=await(await fetch(`${origin}/api/v1/takes/${before.takeId}/tracks/${before.trackId}/status`,{headers:{Authorization:`Bearer ${before.uploadToken}`}})).json();
 const finalHave=status.have.reduce((n,[a,b])=>n+b-a+1,0);
 assert.ok(finalHave>=before.lastSeq+1,`host has ${finalHave} fragments, guest had journaled ${before.lastSeq+1}`);
 const file=(await (await import('node:fs/promises')).readdir(raw)).find(f=>f.startsWith(before.trackId)&&/\.(webm|mp4)$/.test(f));
 execFileSync('ffmpeg',['-v','error','-i',path.join(raw,file),'-f','null','-'],{stdio:'pipe'});
 assert.deepEqual(errors,[]);
 console.log(JSON.stringify({tabKilledMidTake:true,uploadsDroppedBeforeKill:dropped,serverFragmentsAtKill:serverHad,journaledFragments:before.lastSeq+1,recoveredFragments:finalHave,recoveredTrack:'assembled',captureComplete:false,decodes:true,rejoinedWithOneAdmission:true,mediaBothWays:true,missingParticipants:0,errors}));
}catch(e){console.error('errors',JSON.stringify(errors));console.error(logs.slice(-5000));throw e;}
finally{await browser?.close();daemon.kill('SIGTERM');await Promise.race([new Promise(r=>daemon.once('exit',r)),wait(3000)]);await rm(data,{recursive:true,force:true});}
