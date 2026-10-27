// Leave call for guests [SL-P1-6] against the real daemon with Chromium fake
// devices. Host + guest. A take records, then the guest's chunk uploads are
// held back so the guest leaves mid-upload: Leave asks for confirmation
// ("still uploading … reopen this link from this browser to finish"), the
// post-leave screen shows upload progress, the camera and mic are released,
// and the host loses the guest's tile. Uploads then resume while the tab stays
// open, Rejoin comes back without a second admission prompt, both decode each
// other again, and every track of the take is assembled on the host. Leaving
// with nothing pending skips the confirmation. Screenshots (1280×720, 390 px)
// go to STUDIO_SCREENSHOT_DIR when set. Build first; STUDIO_BINARY is the daemon.
import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {mkdtemp,rm,mkdir} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {isolatedNetworkArgs,startTake,launchURL,readyToJoin} from './lib/harness.mjs';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const data=await mkdtemp(path.join(tmpdir(),'studio-leave-'));
const port=Number(process.env.STUDIO_TEST_PORT||18943),origin=`http://127.0.0.1:${port}`;
const daemon=spawn(process.env.STUDIO_BINARY||'../bin/p2pvideod',['--data-dir',data,'--port',String(port),'--no-funnel',...await isolatedNetworkArgs()],{stdio:['ignore','pipe','pipe']});
let logs='';daemon.stderr.on('data',b=>logs+=b);daemon.stdout.on('data',b=>logs+=b);
const screenshotDir=process.env.STUDIO_SCREENSHOT_DIR;
if(screenshotDir)await mkdir(screenshotDir,{recursive:true});
const screenshot=async(name,page)=>{if(screenshotDir)await page.screenshot({path:path.join(screenshotDir,name+'.png'),animations:'disabled'});};
const errors=[];let browser;
const wait=ms=>new Promise(r=>setTimeout(r,ms));
// Held chunk requests can time out client-side and are retried; the Tailscale-less invite is 422.
const expectedConsole=m=>/422|Failed to load resource/.test(m.text());
const decodesRemote=(page,n=1)=>page.waitForFunction(n=>{const v=[...document.querySelectorAll('#video-grid .tile[data-participant-id] video')];return v.length===n&&v.every(x=>x.videoWidth>0);},n,{timeout:30000});

try{
 let ready=false;for(let i=0;i<100;i++){try{ready=(await fetch(origin+'/health')).ok;if(ready)break;}catch{}await wait(100);}assert.ok(ready,'daemon did not start: '+logs);
 browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream','--autoplay-policy=no-user-gesture-required','--mute-audio']});
 const open=async name=>{
  const context=await browser.newContext({permissions:['camera','microphone'],viewport:{width:1280,height:720}});
  const page=await context.newPage();page.setDefaultTimeout(45000);
  page.on('console',m=>{if(m.type()==='error'&&!expectedConsole(m))errors.push(`${name} console: ${m.text()}`);});
  page.on('pageerror',e=>errors.push(`${name}: ${e.message}`));
  return page;
 };
 const host=await open('Host');
 await host.goto(await launchURL(data,origin));await readyToJoin(host);
 await host.locator('#name-input').fill('Host');
 const created=host.waitForResponse(r=>r.url().endsWith('/api/v1/sessions')&&r.request().method()==='POST');
 await host.locator('#join-btn').click();const session=await(await created).json();
 await host.locator('#studio-view').waitFor({state:'visible'});
 assert.ok(await host.locator('#leave-btn').isHidden(),'the host ends the meeting instead of leaving');
 const guest=await open('Guest');
 await guest.goto(`${origin}/join?session=${session.id}&token=${session.token}`);
 await guest.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
 await guest.locator('#name-input').fill('Guest');await guest.locator('#join-btn').click();
 await host.waitForFunction(()=>document.querySelector('#host-admission-banner').style.display==='flex');
 await host.locator('#admit-btn').click();await guest.locator('#studio-view').waitFor({state:'visible'});
 await decodesRemote(host);await decodesRemote(guest);
 await guest.locator('#leave-btn').waitFor({state:'visible'});
 console.log('host and guest decode each other; the guest has Leave call');

 // Hold the guest's chunk uploads so it leaves with a backlog.
 const held=[];let holding=true;
 await guest.route('**/api/v1/takes/*/tracks/*/chunks/*',route=>{if(holding)held.push(route);else route.continue().catch(()=>{});});
 await startTake(host);
 await guest.waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='inline-flex');
 await wait(4000);
 await host.locator('#rec-btn').click();
 await guest.waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='none');
 for(let i=0;i<100&&held.length===0;i++)await wait(100);
 assert.ok(held.length>0,'guest chunk uploads are held');

 // Leave mid-upload: confirmation first.
 await guest.locator('#leave-btn').click();
 const dialog=guest.locator('dialog.room-dialog');
 await dialog.waitFor();
 const body=await dialog.innerText();
 assert.match(body,/still uploading/);assert.match(body,/reopen this link from this browser to finish/);
 await dialog.locator('[data-confirm]').click();
 await guest.locator('#left-call').waitFor({state:'visible'});
 assert.match(await guest.locator('#left-call h1').innerText(),/You left the call/);
 await guest.waitForFunction(()=>/Uploading|Waiting for the host/.test(document.querySelector('[data-left-uploads]').textContent));
 // Camera and microphone are released; the host loses the guest's tile.
 await guest.waitForFunction(()=>{const s=document.querySelector('#local-video').srcObject;return s&&s.getTracks().length>0&&s.getTracks().every(t=>t.readyState==='ended');});
 await host.waitForFunction(()=>document.querySelectorAll('#video-grid .tile[data-participant-id]').length===0,{},{timeout:20000});
 console.log('guest left mid-upload:',await guest.locator('[data-left-uploads]').innerText());
 await guest.waitForFunction(()=>document.querySelectorAll('.toast').length===0,{},{timeout:15000});
 await screenshot('parity-leave-desktop',guest);
 await guest.setViewportSize({width:390,height:844});
 assert.ok(await guest.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth),'leave screen overflows at 390px');
 await screenshot('parity-leave-390',guest);
 await guest.setViewportSize({width:1280,height:720});

 // Uploads resume while the tab stays open.
 holding=false;for(const route of held.splice(0))await route.continue().catch(()=>{});
 const headers={Authorization:`Bearer ${session.hostToken}`};
 const takeSnapshot=async()=>{const d=await(await fetch(`${origin}/api/v1/sessions/${session.id}`,{headers})).json();return d.takes[0]?await(await fetch(`${origin}/api/v1/takes/${d.takes[0].id}`,{headers})).json():null;};
 let snapshot;
 for(let i=0;i<600;i++){snapshot=await takeSnapshot();if(snapshot&&snapshot.tracks.length>=2&&snapshot.tracks.every(t=>t.status==='assembled'))break;await wait(100);}
 assert.ok(snapshot.tracks.length>=2&&snapshot.tracks.every(t=>t.status==='assembled'),JSON.stringify(snapshot));
 assert.equal(snapshot.missingParticipants.length,0,JSON.stringify(snapshot.missingParticipants));
 assert.ok(snapshot.tracks.some(t=>t.participantName==='Guest'&&t.totalBytes>1000),'the guest track finished after leaving');
 await guest.waitForFunction(()=>/reached the host/.test(document.querySelector('[data-left-uploads]').textContent),{},{timeout:30000});
 console.log('uploads finished after leaving; take assembled');

 // Rejoin: no second admission prompt.
 let prompted=false;
 const watch=setInterval(()=>host.evaluate(()=>document.querySelector('#host-admission-banner').style.display==='flex').then(v=>{if(v)prompted=true;}).catch(()=>{}),100);
 await guest.locator('[data-left-rejoin]').click();
 await guest.locator('#studio-view').waitFor({state:'visible'});
 assert.equal(await guest.locator('#left-call').count(),0,'leave screen removed on rejoin');
 await decodesRemote(host);await decodesRemote(guest);
 clearInterval(watch);
 assert.equal(prompted,false,'rejoin did not ask the host to admit again');
 console.log('guest rejoined with its resume grant; both decode again');

 // Nothing pending now: leaving needs no confirmation.
 await guest.locator('#leave-btn').click();
 await guest.locator('#left-call').waitFor({state:'visible'});
 assert.equal(await guest.locator('dialog.room-dialog').count(),0);
 console.log('leaving with nothing pending skips the confirmation');
 assert.deepEqual(errors,[]);
 console.log('leave browser test passed');
}catch(err){console.error(err);console.error(logs.slice(-4000));process.exitCode=1;}
finally{await browser?.close();daemon.kill();await rm(data,{recursive:true,force:true});}
